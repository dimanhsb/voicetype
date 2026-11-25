//go:build windows

package audio

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"voicetype/internal/logging"
)

// DeviceInfo describes an active Windows audio capture device.
type DeviceInfo struct {
	ID        string
	Name      string
	IsDefault bool
}

// FrameCallback receives 16 kHz mono s16le PCM chunks (usually 640 bytes = 20 ms)
// and the perceptual audio meter level in [0.0, 1.0].
type FrameCallback func(pcm16 []byte, level float64)

// DeviceChangeCallback is called when audio devices are added, removed, or the default changes.
type DeviceChangeCallback func()

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	procPropVariantClear = ole32.NewProc("PropVariantClear")
)

const (
	coinitMultithreaded   = 0x0
	clsctxAll             = 0x17
	eCapture              = 1
	eConsole              = 0
	eCommunications       = 2
	deviceStateActive     = 0x00000001
	stgmRead              = 0x00000000
	audclntShareModeShared = 0
	audclntStreamFlagsEventCallback = 0x00040000
	audclntBufferFlagsSilent        = 0x00000002

	hrOK                     = 0x00000000
	hrFalse                  = 0x00000001
	hrChangedMode            = 0x80010106
	hrAudclntDeviceInvalid   = 0x88890004
	hrAudclntServiceStopped  = 0x88890010
	hrAudclntBufferEmpty     = 0x08890001
	hrNotFound               = 0x80070490

	waveFormatPCM        = 1
	waveFormatIEEEFloat  = 3
	waveFormatExtensible = 0xFFFE
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type propertyKey struct {
	FmtID guid
	PID   uint32
}

type propVariant struct {
	VT         uint16
	Reserved1  uint16
	Reserved2  uint16
	Reserved3  uint16
	Val        uintptr
	Val2       uintptr
}

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

type waveFormatExtensibleStruct struct {
	Format              waveFormatEx
	ValidBitsPerSample  uint16
	ChannelMask         uint32
	SubFormat           guid
}

var (
	clsidMMDeviceEnumerator = guid{0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIUnknown             = guid{0x00000000, 0x0000, 0x0000, [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIMMDeviceEnumerator  = guid{0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIMMNotificationClient = guid{0x7991EEC9, 0x7E89, 0x4D85, [8]byte{0x83, 0x90, 0x6C, 0x70, 0x3C, 0xEC, 0x60, 0xC0}}
	iidIAudioClient         = guid{0x1CB9AD4C, 0xDBFA, 0x4C32, [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient  = guid{0xC8ADBD64, 0xE71E, 0x48A0, [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}

	subtypePCM       = guid{0x00000001, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	subtypeIEEEFloat = guid{0x00000003, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}

	pkeyDeviceFriendlyName = propertyKey{
		FmtID: guid{0xA45C254E, 0xDF1C, 0x4EFD, [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}},
		PID:   14,
	}
)

// comObj points to a COM object whose first field is a pointer to its vtable.
type comObj struct {
	vtbl *uintptr
}

func (o *comObj) vtableEntry(index uintptr) uintptr {
	return *(*uintptr)(unsafe.Add(unsafe.Pointer(o.vtbl), index*unsafe.Sizeof(uintptr(0))))
}

func (o *comObj) release() {
	if o == nil {
		return
	}
	syscall.SyscallN(o.vtableEntry(2), uintptr(unsafe.Pointer(o)))
}

func hrSucceeded(hr uintptr) bool {
	return int32(hr) >= 0
}

func coInit() bool {
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	return hr == hrOK || hr == hrFalse
}

func createEnumerator() (*comObj, error) {
	var enum *comObj
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0,
		clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enum)),
	)
	if !hrSucceeded(hr) || enum == nil {
		return nil, fmt.Errorf("CoCreateInstance(MMDeviceEnumerator) failed: 0x%08x", uint32(hr))
	}
	return enum, nil
}

func getDeviceID(dev *comObj) string {
	if dev == nil {
		return ""
	}
	var wstr *uint16
	hr, _, _ := syscall.SyscallN(dev.vtableEntry(5), uintptr(unsafe.Pointer(dev)), uintptr(unsafe.Pointer(&wstr)))
	if !hrSucceeded(hr) || wstr == nil {
		return ""
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(wstr)))
	return windows.UTF16PtrToString(wstr)
}

func getDeviceFriendlyName(dev *comObj) string {
	if dev == nil {
		return "Unknown Microphone"
	}
	var store *comObj
	hr, _, _ := syscall.SyscallN(dev.vtableEntry(4), uintptr(unsafe.Pointer(dev)), stgmRead, uintptr(unsafe.Pointer(&store)))
	if !hrSucceeded(hr) || store == nil {
		return "Microphone"
	}
	defer store.release()

	var pv propVariant
	hr, _, _ = syscall.SyscallN(
		store.vtableEntry(5),
		uintptr(unsafe.Pointer(store)),
		uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)),
		uintptr(unsafe.Pointer(&pv)),
	)
	if !hrSucceeded(hr) {
		return "Microphone"
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv)))
	// VT_LPWSTR == 31
	if pv.VT == 31 && pv.Val != 0 {
		return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(pv.Val)))
	}
	return "Microphone"
}

// ListCaptureDevices enumerates all currently active Windows audio capture devices.
func ListCaptureDevices() ([]DeviceInfo, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if coInit() {
		defer procCoUninitialize.Call()
	}

	enum, err := createEnumerator()
	if err != nil {
		return nil, err
	}
	defer enum.release()

	// Find default capture endpoint ID
	var defaultID string
	var defDev *comObj
	hr, _, _ := syscall.SyscallN(
		enum.vtableEntry(4), // GetDefaultAudioEndpoint
		uintptr(unsafe.Pointer(enum)),
		eCapture,
		eConsole,
		uintptr(unsafe.Pointer(&defDev)),
	)
	if hrSucceeded(hr) && defDev != nil {
		defaultID = getDeviceID(defDev)
		defDev.release()
	}

	var coll *comObj
	hr, _, _ = syscall.SyscallN(
		enum.vtableEntry(3), // EnumAudioEndpoints
		uintptr(unsafe.Pointer(enum)),
		eCapture,
		deviceStateActive,
		uintptr(unsafe.Pointer(&coll)),
	)
	if !hrSucceeded(hr) || coll == nil {
		return nil, fmt.Errorf("EnumAudioEndpoints failed: 0x%08x", uint32(hr))
	}
	defer coll.release()

	var count uint32
	hr, _, _ = syscall.SyscallN(coll.vtableEntry(3), uintptr(unsafe.Pointer(coll)), uintptr(unsafe.Pointer(&count)))
	if !hrSucceeded(hr) {
		return nil, fmt.Errorf("IMMDeviceCollection.GetCount failed: 0x%08x", uint32(hr))
	}

	devices := make([]DeviceInfo, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev *comObj
		hr, _, _ = syscall.SyscallN(coll.vtableEntry(4), uintptr(unsafe.Pointer(coll)), uintptr(i), uintptr(unsafe.Pointer(&dev)))
		if !hrSucceeded(hr) || dev == nil {
			continue
		}
		id := getDeviceID(dev)
		name := getDeviceFriendlyName(dev)
		dev.release()
		devices = append(devices, DeviceInfo{
			ID:        id,
			Name:      name,
			IsDefault: id != "" && id == defaultID,
		})
	}
	return devices, nil
}

// --- IMMNotificationClient COM callback object ---

type notificationVtbl struct {
	QueryInterface         uintptr
	AddRef                 uintptr
	Release                uintptr
	OnDeviceStateChanged   uintptr
	OnDeviceAdded          uintptr
	OnDeviceRemoved        uintptr
	OnDefaultDeviceChanged uintptr
	OnPropertyValueChanged uintptr
}

type notificationClient struct {
	vtbl     *notificationVtbl
	refCount int32
	owner    *Capturer
}

var (
	notifVtblOnce sync.Once
	notifVtblInst notificationVtbl
)

func initNotificationVtbl() {
	notifVtblOnce.Do(func() {
		notifVtblInst = notificationVtbl{
			QueryInterface: syscall.NewCallback(func(this, riid, ppv uintptr) uintptr {
				if ppv == 0 || riid == 0 {
					return 0x80004003 // E_POINTER
				}
				g := *(*guid)(unsafe.Pointer(riid))
				if g == iidIUnknown || g == iidIMMNotificationClient {
					nc := (*notificationClient)(unsafe.Pointer(this))
					atomic.AddInt32(&nc.refCount, 1)
					*(*uintptr)(unsafe.Pointer(ppv)) = this
					return hrOK
				}
				*(*uintptr)(unsafe.Pointer(ppv)) = 0
				return 0x80004002 // E_NOINTERFACE
			}),
			AddRef: syscall.NewCallback(func(this uintptr) uintptr {
				nc := (*notificationClient)(unsafe.Pointer(this))
				return uintptr(atomic.AddInt32(&nc.refCount, 1))
			}),
			Release: syscall.NewCallback(func(this uintptr) uintptr {
				nc := (*notificationClient)(unsafe.Pointer(this))
				return uintptr(atomic.AddInt32(&nc.refCount, -1))
			}),
			OnDeviceStateChanged: syscall.NewCallback(func(this, pwstrDeviceID, dwNewState uintptr) uintptr {
				nc := (*notificationClient)(unsafe.Pointer(this))
				id := ""
				if pwstrDeviceID != 0 {
					id = windows.UTF16PtrToString((*uint16)(unsafe.Pointer(pwstrDeviceID)))
				}
				nc.owner.onNotification("state-changed", id)
				return hrOK
			}),
			OnDeviceAdded: syscall.NewCallback(func(this, pwstrDeviceID uintptr) uintptr {
				nc := (*notificationClient)(unsafe.Pointer(this))
				nc.owner.onNotification("device-added", "")
				return hrOK
			}),
			OnDeviceRemoved: syscall.NewCallback(func(this, pwstrDeviceID uintptr) uintptr {
				nc := (*notificationClient)(unsafe.Pointer(this))
				id := ""
				if pwstrDeviceID != 0 {
					id = windows.UTF16PtrToString((*uint16)(unsafe.Pointer(pwstrDeviceID)))
				}
				nc.owner.onNotification("device-removed", id)
				return hrOK
			}),
			OnDefaultDeviceChanged: syscall.NewCallback(func(this, flow, role, pwstrDefaultID uintptr) uintptr {
				if flow == eCapture && (role == eConsole || role == eCommunications) {
					nc := (*notificationClient)(unsafe.Pointer(this))
					nc.owner.onNotification("default-changed", "")
				}
				return hrOK
			}),
			OnPropertyValueChanged: syscall.NewCallback(func(this, pwstrDeviceID, keyPtr uintptr) uintptr {
				return hrOK
			}),
		}
	})
}

// rawPreRollRing is a zero-allocation circular byte buffer storing the latest ~160ms
// of raw WASAPI audio while idle (eliminates GC churn and keeps idle CPU near 0%).
type rawPreRollRing struct {
	buf   []byte
	write int
	size  int
}

func (r *rawPreRollRing) configure(capacityBytes int) {
	if capacityBytes < 4096 {
		capacityBytes = 4096
	}
	if cap(r.buf) < capacityBytes {
		r.buf = make([]byte, capacityBytes)
	} else {
		r.buf = r.buf[:capacityBytes]
	}
	r.write = 0
	r.size = 0
}

func (r *rawPreRollRing) push(raw []byte) {
	capLen := len(r.buf)
	if len(raw) == 0 || capLen == 0 {
		return
	}
	if len(raw) >= capLen {
		copy(r.buf, raw[len(raw)-capLen:])
		r.write = 0
		r.size = capLen
		return
	}
	first := capLen - r.write
	if first >= len(raw) {
		copy(r.buf[r.write:], raw)
	} else {
		copy(r.buf[r.write:], raw[:first])
		copy(r.buf[0:], raw[first:])
	}
	r.write = (r.write + len(raw)) % capLen
	r.size += len(raw)
	if r.size > capLen {
		r.size = capLen
	}
}

// drain returns all stored raw bytes in chronological order, aligned to frameBytes, and resets the ring.
func (r *rawPreRollRing) drain(frameBytes int) []byte {
	if r.size == 0 || len(r.buf) == 0 {
		return nil
	}
	capLen := len(r.buf)
	start := (r.write - r.size + capLen) % capLen
	out := make([]byte, r.size)
	if start+r.size <= capLen {
		copy(out, r.buf[start:start+r.size])
	} else {
		first := capLen - start
		copy(out[:first], r.buf[start:])
		copy(out[first:], r.buf[:r.size-first])
	}
	r.write = 0
	r.size = 0
	if frameBytes > 1 {
		rem := len(out) % frameBytes
		if rem != 0 {
			out = out[rem:]
		}
	}
	return out
}

func (r *rawPreRollRing) clear() {
	r.write = 0
	r.size = 0
}

var errReopenRequested = fmt.Errorf("reopen requested")

// Capturer keeps WASAPI microphone capture initialized from app startup so dictation
// begins with zero open delay, maintains a 160ms zero-alloc raw pre-roll ring while idle,
// and handles live device hot-plug / default-device switching transparently.
type Capturer struct {
	mu             sync.RWMutex
	deviceID       string // "" means Windows Default
	activeDevID    string
	activeDevName  string
	gainPercent    int32
	recording      atomic.Bool
	sink           FrameCallback
	onDeviceChange DeviceChangeCallback

	// procMu protects rawPreRoll, resampler, and chunker state (NEVER held across sink calls!).
	// deliverMu strictly orders pre-roll delivery ahead of live frames without deadlocking EndRecording.
	procMu        sync.Mutex
	deliverMu     sync.Mutex
	curFmt        StreamFormat
	curFrameBytes int
	hasFmt        bool
	resampler     *Resampler
	chunker       Chunker
	rawPreRoll    rawPreRollRing
	lastErr       string

	reopenCh chan struct{}
	stopCh   chan struct{}
	doneCh   chan struct{}
	started  atomic.Bool

	// Keep COM notification object pinned for lifetime of Capturer
	notifObj *notificationClient
	pinner   runtime.Pinner
}

// NewCapturer creates a WASAPI capturer with the given initial microphone ID ("" = Default) and gain.
func NewCapturer(deviceID string, gainPercent int, onDeviceChange DeviceChangeCallback) *Capturer {
	if gainPercent < 50 {
		gainPercent = 50
	}
	if gainPercent > 300 {
		gainPercent = 300
	}
	c := &Capturer{
		deviceID:       deviceID,
		gainPercent:    int32(gainPercent),
		onDeviceChange: onDeviceChange,
		reopenCh:       make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
	initNotificationVtbl()
	c.notifObj = &notificationClient{
		vtbl:     &notifVtblInst,
		refCount: 1,
		owner:    c,
	}
	c.pinner.Pin(&notifVtblInst)
	c.pinner.Pin(c.notifObj)
	return c
}

// Start launches the dedicated COM/WASAPI capture thread immediately at app startup.
func (c *Capturer) Start() {
	if c.started.Swap(true) {
		return
	}
	go c.runLoop()
}

// Close shuts down the capture thread and releases WASAPI resources.
func (c *Capturer) Close() {
	if !c.started.Load() {
		return
	}
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
	<-c.doneCh
	c.pinner.Unpin()
}

// SetGain updates the live microphone gain multiplier (50..300%).
func (c *Capturer) SetGain(gainPercent int) {
	if gainPercent < 50 {
		gainPercent = 50
	}
	if gainPercent > 300 {
		gainPercent = 300
	}
	atomic.StoreInt32(&c.gainPercent, int32(gainPercent))
}

// SetDevice switches to a specific device ID (or "" for Windows Default) without restarting.
func (c *Capturer) SetDevice(deviceID string) {
	c.mu.Lock()
	c.deviceID = deviceID
	c.mu.Unlock()
	c.triggerReopen()
}

// ActiveDevice returns the currently open capture device ID and friendly name.
func (c *Capturer) ActiveDevice() (id, name string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.activeDevID, c.activeDevName
}

// LastError returns the most recent capture initialization/runtime error (empty when healthy).
func (c *Capturer) LastError() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastErr
}

// BeginRecording activates audio streaming to sink immediately, first resampling and flushing
// the 160 ms raw pre-roll ring strictly BEFORE any live audio chunks can be delivered.
// Never holds procMu while invoking sink so EndRecording cannot deadlock under backpressure.
func (c *Capturer) BeginRecording(sink FrameCallback) {
	c.deliverMu.Lock()
	defer c.deliverMu.Unlock()

	var preRollChunks [][]byte
	c.procMu.Lock()
	c.mu.Lock()
	c.sink = sink
	c.recording.Store(true)
	c.mu.Unlock()

	if c.hasFmt {
		c.resampler = NewResampler(c.curFmt)
		c.chunker.Reset()
		rawBytes := c.rawPreRoll.drain(c.curFrameBytes)
		if len(rawBytes) > 0 && sink != nil {
			gain := int(atomic.LoadInt32(&c.gainPercent))
			pcm16, _ := c.resampler.Process(rawBytes, gain)
			if len(pcm16) > 0 {
				preRollChunks = c.chunker.Push(pcm16)
			}
		}
	} else {
		c.rawPreRoll.clear()
	}
	c.procMu.Unlock()

	if sink != nil {
		for _, ch := range preRollChunks {
			if !c.recording.Load() {
				break
			}
			sink(ch, ComputePCM16Level(ch))
		}
	}
}

// EndRecording stops forwarding audio to the sink (WASAPI stays warm and open for instant next start).
func (c *Capturer) EndRecording() {
	c.procMu.Lock()
	defer c.procMu.Unlock()

	c.mu.Lock()
	c.recording.Store(false)
	c.sink = nil
	c.mu.Unlock()
	c.resampler = nil
	c.chunker.Reset()
	c.rawPreRoll.clear()
}

func (c *Capturer) triggerReopen() {
	select {
	case c.reopenCh <- struct{}{}:
	default:
	}
}

// onNotification runs inside Windows' COM notification callback thread and MUST NOT block or do heavy I/O.
func (c *Capturer) onNotification(reason, deviceID string) {
	c.mu.RLock()
	wanted := c.deviceID
	active := c.activeDevID
	c.mu.RUnlock()

	shouldReopen := false
	switch reason {
	case "default-changed":
		// Reopen only if we're using Windows Default (or fell back to Default)
		shouldReopen = (wanted == "" || wanted != active)
	case "device-removed", "state-changed":
		// Reopen only if the changed device is our active or configured mic (ignore speaker/HDMI unplugs!)
		shouldReopen = (deviceID != "" && (deviceID == active || deviceID == wanted))
	case "device-added":
		// Reopen if we currently have no mic or fell back from our preferred mic
		shouldReopen = (active == "" || (wanted != "" && wanted != active))
	}

	if shouldReopen {
		c.triggerReopen()
	}
	go func() {
		logging.Infof("audio: Windows device notification (%s, reopen=%v)", reason, shouldReopen)
		if c.onDeviceChange != nil {
			c.onDeviceChange()
		}
	}()
}

func (c *Capturer) runLoop() {
	defer close(c.doneCh)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if coInit() {
		defer procCoUninitialize.Call()
	}

	enum, err := createEnumerator()
	if err != nil {
		logging.Errorf("audio: %v", err)
		return
	}
	defer enum.release()

	// Register hot-plug / default-device change listener
	hr, _, _ := syscall.SyscallN(
		enum.vtableEntry(6), // RegisterEndpointNotificationCallback
		uintptr(unsafe.Pointer(enum)),
		uintptr(unsafe.Pointer(c.notifObj)),
	)
	if hrSucceeded(hr) {
		defer syscall.SyscallN(
			enum.vtableEntry(7), // UnregisterEndpointNotificationCallback
			uintptr(unsafe.Pointer(enum)),
			uintptr(unsafe.Pointer(c.notifObj)),
		)
	} else {
		logging.Warnf("audio: RegisterEndpointNotificationCallback failed: 0x%08x", uint32(hr))
	}

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		// Drain any pending reopen signal before opening
		select {
		case <-c.reopenCh:
		default:
		}

		err := c.captureSession(enum)
		if err != nil && err != errReopenRequested {
			c.mu.Lock()
			c.lastErr = err.Error()
			c.mu.Unlock()
			logging.Warnf("audio: capture session ended: %v", err)
		}

		delay := 1 * time.Second
		if err == errReopenRequested {
			delay = 120 * time.Millisecond
		}

		select {
		case <-c.stopCh:
			return
		case <-c.reopenCh:
			time.Sleep(120 * time.Millisecond)
		case <-time.After(delay):
		}
	}
}

func (c *Capturer) resolveDevice(enum *comObj) (*comObj, string, string, error) {
	c.mu.RLock()
	wantedID := c.deviceID
	c.mu.RUnlock()

	var dev *comObj
	if wantedID != "" {
		utf16ID, err := windows.UTF16PtrFromString(wantedID)
		if err == nil {
			hr, _, _ := syscall.SyscallN(
				enum.vtableEntry(5), // GetDevice
				uintptr(unsafe.Pointer(enum)),
				uintptr(unsafe.Pointer(utf16ID)),
				uintptr(unsafe.Pointer(&dev)),
			)
			if hrSucceeded(hr) && dev != nil {
				var st uint32
				hr2, _, _ := syscall.SyscallN(dev.vtableEntry(6), uintptr(unsafe.Pointer(dev)), uintptr(unsafe.Pointer(&st)))
				if !hrSucceeded(hr2) || st != deviceStateActive {
					logging.Warnf("audio: configured mic %q is not active (state=%d); falling back to Default", wantedID, st)
					dev.release()
					dev = nil
				}
			} else {
				logging.Warnf("audio: configured mic %q unavailable; falling back to Default", wantedID)
			}
		}
	}

	if dev == nil {
		hr, _, _ := syscall.SyscallN(
			enum.vtableEntry(4), // GetDefaultAudioEndpoint
			uintptr(unsafe.Pointer(enum)),
			eCapture,
			eConsole,
			uintptr(unsafe.Pointer(&dev)),
		)
		if !hrSucceeded(hr) || dev == nil {
			return nil, "", "", fmt.Errorf("no active default microphone (hr=0x%08x)", uint32(hr))
		}
	}

	id := getDeviceID(dev)
	name := getDeviceFriendlyName(dev)
	return dev, id, name, nil
}

func parseWaveFormat(pwfx *waveFormatEx) (StreamFormat, error) {
	if pwfx == nil {
		return StreamFormat{}, fmt.Errorf("nil WAVEFORMATEX")
	}
	// WAVEFORMATEX is 18 bytes packed (pshpack1.h) in Win32, so read fields cleanly
	// by raw byte offsets to avoid Go's 2-byte struct tail padding before SubFormat!
	raw := unsafe.Slice((*byte)(unsafe.Pointer(pwfx)), 18)
	formatTag := uint16(raw[0]) | uint16(raw[1])<<8
	channels := uint16(raw[2]) | uint16(raw[3])<<8
	sampleRate := uint32(raw[4]) | uint32(raw[5])<<8 | uint32(raw[6])<<16 | uint32(raw[7])<<24
	bitsPerSample := uint16(raw[14]) | uint16(raw[15])<<8
	cbSize := uint16(raw[16]) | uint16(raw[17])<<8

	sf := StreamFormat{
		SampleRate: int(sampleRate),
		Channels:   int(channels),
	}
	switch formatTag {
	case waveFormatIEEEFloat:
		if bitsPerSample != 32 {
			return sf, fmt.Errorf("unsupported IEEE float bits=%d", bitsPerSample)
		}
		sf.Format = FormatFloat32
	case waveFormatPCM:
		switch bitsPerSample {
		case 16:
			sf.Format = FormatInt16
		case 24:
			sf.Format = FormatInt24
		case 32:
			sf.Format = FormatInt32
		default:
			return sf, fmt.Errorf("unsupported PCM bits=%d", bitsPerSample)
		}
	case waveFormatExtensible:
		if cbSize < 22 {
			return sf, fmt.Errorf("short WAVEFORMATEXTENSIBLE cbSize=%d", cbSize)
		}
		// WAVEFORMATEXTENSIBLE packed layout:
		//   [0..17]  WAVEFORMATEX (18 bytes)
		//   [18..19] wValidBitsPerSample (2 bytes)
		//   [20..23] dwChannelMask (4 bytes)
		//   [24..39] SubFormat GUID (16 bytes)
		extBytes := unsafe.Slice((*byte)(unsafe.Pointer(pwfx)), 40)
		var sub guid
		sub.Data1 = uint32(extBytes[24]) | uint32(extBytes[25])<<8 | uint32(extBytes[26])<<16 | uint32(extBytes[27])<<24
		sub.Data2 = uint16(extBytes[28]) | uint16(extBytes[29])<<8
		sub.Data3 = uint16(extBytes[30]) | uint16(extBytes[31])<<8
		copy(sub.Data4[:], extBytes[32:40])

		switch sub {
		case subtypeIEEEFloat:
			if bitsPerSample != 32 {
				return sf, fmt.Errorf("unsupported extensible float bits=%d", bitsPerSample)
			}
			sf.Format = FormatFloat32
		case subtypePCM:
			switch bitsPerSample {
			case 16:
				sf.Format = FormatInt16
			case 24:
				sf.Format = FormatInt24
			case 32:
				sf.Format = FormatInt32
			default:
				return sf, fmt.Errorf("unsupported extensible PCM bits=%d", bitsPerSample)
			}
		default:
			return sf, fmt.Errorf("unsupported extensible subformat %08x-%04x-%04x", sub.Data1, sub.Data2, sub.Data3)
		}
	default:
		return sf, fmt.Errorf("unsupported wave format tag 0x%x", formatTag)
	}
	return sf, nil
}

func (c *Capturer) captureSession(enum *comObj) error {
	dev, id, name, err := c.resolveDevice(enum)
	if err != nil {
		return err
	}
	defer dev.release()

	var audioClient *comObj
	hr, _, _ := syscall.SyscallN(
		dev.vtableEntry(3), // Activate
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(&iidIAudioClient)),
		clsctxAll,
		0,
		uintptr(unsafe.Pointer(&audioClient)),
	)
	if !hrSucceeded(hr) || audioClient == nil {
		return fmt.Errorf("Activate(IAudioClient) failed: 0x%08x", uint32(hr))
	}
	defer audioClient.release()

	var pwfx *waveFormatEx
	hr, _, _ = syscall.SyscallN(
		audioClient.vtableEntry(8), // GetMixFormat
		uintptr(unsafe.Pointer(audioClient)),
		uintptr(unsafe.Pointer(&pwfx)),
	)
	if !hrSucceeded(hr) || pwfx == nil {
		return fmt.Errorf("GetMixFormat failed: 0x%08x", uint32(hr))
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(pwfx)))

	streamFmt, err := parseWaveFormat(pwfx)
	if err != nil {
		return err
	}
	bytesPerFrame := int(pwfx.BlockAlign)
	if bytesPerFrame <= 0 {
		return fmt.Errorf("invalid block align %d", bytesPerFrame)
	}

	// Microsoft's shared event-driven capture contract specifies hnsBufferDuration=0, hnsPeriodicity=0.
	// Fall back to 200ms only if a driver rejects 0.
	hr, _, _ = syscall.SyscallN(
		audioClient.vtableEntry(3), // Initialize
		uintptr(unsafe.Pointer(audioClient)),
		audclntShareModeShared,
		audclntStreamFlagsEventCallback,
		0,
		0,
		uintptr(unsafe.Pointer(pwfx)),
		0,
	)
	if !hrSucceeded(hr) {
		const fallbackDuration = 200 * 10000
		hr, _, _ = syscall.SyscallN(
			audioClient.vtableEntry(3), // Initialize
			uintptr(unsafe.Pointer(audioClient)),
			audclntShareModeShared,
			audclntStreamFlagsEventCallback,
			fallbackDuration,
			0,
			uintptr(unsafe.Pointer(pwfx)),
			0,
		)
	}
	if !hrSucceeded(hr) {
		return fmt.Errorf("IAudioClient.Initialize failed: 0x%08x", uint32(hr))
	}

	evHandle, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("CreateEvent: %w", err)
	}
	defer windows.CloseHandle(evHandle)

	hr, _, _ = syscall.SyscallN(
		audioClient.vtableEntry(13), // SetEventHandle
		uintptr(unsafe.Pointer(audioClient)),
		uintptr(evHandle),
	)
	if !hrSucceeded(hr) {
		return fmt.Errorf("SetEventHandle failed: 0x%08x", uint32(hr))
	}

	var capClient *comObj
	hr, _, _ = syscall.SyscallN(
		audioClient.vtableEntry(14), // GetService
		uintptr(unsafe.Pointer(audioClient)),
		uintptr(unsafe.Pointer(&iidIAudioCaptureClient)),
		uintptr(unsafe.Pointer(&capClient)),
	)
	if !hrSucceeded(hr) || capClient == nil {
		return fmt.Errorf("GetService(IAudioCaptureClient) failed: 0x%08x", uint32(hr))
	}
	defer capClient.release()

	hr, _, _ = syscall.SyscallN(audioClient.vtableEntry(10), uintptr(unsafe.Pointer(audioClient))) // Start
	if !hrSucceeded(hr) {
		return fmt.Errorf("IAudioClient.Start failed: 0x%08x", uint32(hr))
	}
	defer syscall.SyscallN(audioClient.vtableEntry(11), uintptr(unsafe.Pointer(audioClient))) // Stop

	c.mu.Lock()
	c.activeDevID = id
	c.activeDevName = name
	c.lastErr = ""
	c.mu.Unlock()
	logging.Infof("audio: initialized capture on %q (%d Hz, %d ch, %s)", name, streamFmt.SampleRate, streamFmt.Channels, streamFmt.Format)

	// Configure raw 160ms pre-roll ring for this device's format (zero idle resampling CPU!)
	preRollBytes := streamFmt.SampleRate * bytesPerFrame * 160 / 1000
	if bytesPerFrame > 0 {
		preRollBytes -= preRollBytes % bytesPerFrame
	}
	c.procMu.Lock()
	c.curFmt = streamFmt
	c.curFrameBytes = bytesPerFrame
	c.hasFmt = true
	c.rawPreRoll.configure(preRollBytes)
	if c.recording.Load() {
		// Seamless mid-dictation hot-plug recovery: initialize resampler for new device format!
		c.resampler = NewResampler(streamFmt)
		c.chunker.Reset()
	}
	c.procMu.Unlock()

	var silentBuf []byte
	var rawCopy []byte

	for {
		select {
		case <-c.stopCh:
			return nil
		case <-c.reopenCh:
			logging.Infof("audio: reopening capture device")
			return errReopenRequested
		default:
		}

		// Wait up to 50 ms for WASAPI audio-ready event (near-zero idle CPU!)
		_, _ = windows.WaitForSingleObject(evHandle, 50)

		for {
			var packetFrames uint32
			hr, _, _ = syscall.SyscallN(
				capClient.vtableEntry(5), // GetNextPacketSize
				uintptr(unsafe.Pointer(capClient)),
				uintptr(unsafe.Pointer(&packetFrames)),
			)
			if uint32(hr) == hrAudclntDeviceInvalid || uint32(hr) == hrAudclntServiceStopped {
				return fmt.Errorf("device invalidated (0x%08x)", uint32(hr))
			}
			if !hrSucceeded(hr) {
				return fmt.Errorf("GetNextPacketSize failed (0x%08x)", uint32(hr))
			}
			if packetFrames == 0 {
				break
			}

			var dataPtr uintptr
			var numFrames uint32
			var flags uint32
			hr, _, _ = syscall.SyscallN(
				capClient.vtableEntry(3), // GetBuffer
				uintptr(unsafe.Pointer(capClient)),
				uintptr(unsafe.Pointer(&dataPtr)),
				uintptr(unsafe.Pointer(&numFrames)),
				uintptr(unsafe.Pointer(&flags)),
				0,
				0,
			)
			if uint32(hr) == hrAudclntDeviceInvalid || uint32(hr) == hrAudclntServiceStopped {
				return fmt.Errorf("device invalidated on GetBuffer (0x%08x)", uint32(hr))
			}
			if uint32(hr) == hrAudclntBufferEmpty {
				break
			}
			if !hrSucceeded(hr) {
				return fmt.Errorf("GetBuffer failed (0x%08x)", uint32(hr))
			}

			byteLen := int(numFrames) * bytesPerFrame
			var raw []byte
			if byteLen > 0 {
				if flags&audclntBufferFlagsSilent != 0 || dataPtr == 0 {
					if len(silentBuf) < byteLen {
						silentBuf = make([]byte, byteLen)
					}
					raw = silentBuf[:byteLen]
				} else {
					if cap(rawCopy) < byteLen {
						rawCopy = make([]byte, byteLen)
					}
					raw = rawCopy[:byteLen]
					copy(raw, unsafe.Slice((*byte)(unsafe.Pointer(dataPtr)), byteLen))
				}
			}

			// Release WASAPI buffer promptly before any processing/callbacks
			syscall.SyscallN(
				capClient.vtableEntry(4), // ReleaseBuffer
				uintptr(unsafe.Pointer(capClient)),
				uintptr(numFrames),
			)

			if len(raw) == 0 {
				continue
			}

			c.procMu.Lock()
			if !c.recording.Load() {
				c.rawPreRoll.push(raw)
				c.procMu.Unlock()
				continue
			}
			if c.resampler == nil {
				c.resampler = NewResampler(streamFmt)
				c.chunker.Reset()
			}
			gain := int(atomic.LoadInt32(&c.gainPercent))
			pcm16, _ := c.resampler.Process(raw, gain)
			var chunks [][]byte
			if len(pcm16) > 0 {
				chunks = c.chunker.Push(pcm16)
			}
			c.mu.RLock()
			sink := c.sink
			c.mu.RUnlock()
			c.procMu.Unlock()

			if sink != nil && len(chunks) > 0 {
				c.deliverMu.Lock()
				for _, ch := range chunks {
					if !c.recording.Load() {
						break
					}
					sink(ch, ComputePCM16Level(ch))
				}
				c.deliverMu.Unlock()
			}
		}
	}
}
