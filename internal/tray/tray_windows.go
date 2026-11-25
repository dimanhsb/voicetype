//go:build windows

package tray

import (
	"embed"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"voicetype/internal/audio"
	"voicetype/internal/config"
	"voicetype/internal/logging"
	"voicetype/internal/transcript"
)

//go:embed icons/*.ico
var embeddedIcons embed.FS

// IconState selects which embedded tray icon variant is displayed.
type IconState int

const (
	IconIdle IconState = iota
	IconRecording
	IconEditing
	IconError
)

// MenuState is queried dynamically each time the user opens the tray context menu.
type MenuState struct {
	StatusLine      string
	Version         string
	Config          config.Config
	Devices         []audio.DeviceInfo
	RecentHistory   []config.HistoryEntry
	StartWithWin    bool
}

// Callbacks handles user selections from the system tray menu.
type Callbacks struct {
	GetMenuState         func() MenuState
	OnLeftClick          func()
	OnSelectMic          func(deviceID, deviceName string)
	OnSelectGain         func(gainPercent int)
	OnSelectOpacity      func(opacityPercent int)
	OnSelectInsertMethod func(method string)
	OnSelectPushToTalk   func(ptt bool)
	OnCopyHistory        func(text string)
	OnClearHistory       func()
	OnToggleStartWithWin func(enabled bool)
	OnToggleSoundCues    func(enabled bool)
	OnToggleDebugWAVs    func(enabled bool)
	OnToggleAICleanup    func(enabled bool)
	OnSetAPIKey          func()
	OnCheckModelUpdate   func()
	OnOpenSettings       func()
	OnOpenLog            func()
	OnQuit               func()
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW        = user32.NewProc("RegisterClassExW")
	procCreateWindowExW         = user32.NewProc("CreateWindowExW")
	procDestroyWindow           = user32.NewProc("DestroyWindow")
	procDefWindowProcW          = user32.NewProc("DefWindowProcW")
	procGetMessageW             = user32.NewProc("GetMessageW")
	procTranslateMessage        = user32.NewProc("TranslateMessage")
	procDispatchMessageW        = user32.NewProc("DispatchMessageW")
	procPostMessageW            = user32.NewProc("PostMessageW")
	procPostQuitMessage         = user32.NewProc("PostQuitMessage")
	procCreatePopupMenu         = user32.NewProc("CreatePopupMenu")
	procDestroyMenu             = user32.NewProc("DestroyMenu")
	procAppendMenuW             = user32.NewProc("AppendMenuW")
	procTrackPopupMenuEx        = user32.NewProc("TrackPopupMenuEx")
	procGetCursorPos            = user32.NewProc("GetCursorPos")
	procSetForegroundWindow     = user32.NewProc("SetForegroundWindow")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon             = user32.NewProc("DestroyIcon")
	procGetSystemMetrics        = user32.NewProc("GetSystemMetrics")
	procRegisterWindowMessageW  = user32.NewProc("RegisterWindowMessageW")
	procSetTimer                = user32.NewProc("SetTimer")
	procKillTimer               = user32.NewProc("KillTimer")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW    = shell32.NewProc("ShellExecuteW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmDestroy     = 0x0002
	wmNull        = 0x0000
	wmTimer       = 0x0113
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B
	wmAppTray     = 0x8100
	wmAppSetIcon  = 0x8101
	wmAppClose    = 0x8102

	timerRetryTrayID = 1

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString    = 0x00000000
	mfGrayed    = 0x00000001
	mfDisabled  = 0x00000002
	mfChecked   = 0x00000008
	mfPopup     = 0x00000010
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmNonotify    = 0x0080

	smCxSmIcon = 49
	smCySmIcon = 50
)

// Menu command ID ranges
const (
	cmdMicDefault     = 1000
	cmdMicBase        = 1001 // 1001..1099
	cmdGainBase       = 1100 // 1100..1115
	cmdOpacityBase    = 1120 // 1120..1130
	cmdInsertPaste    = 1200
	cmdInsertSend     = 1201
	cmdModeToggle     = 1210
	cmdModePTT        = 1211
	cmdHistBase       = 1300 // 1300..1320
	cmdHistClear      = 1399
	cmdToggleStartup  = 1400
	cmdToggleSounds   = 1401
	cmdToggleDebugWAV = 1402
	cmdToggleAIClean  = 1403
	cmdCheckModel     = 1498
	cmdSetAPIKey      = 1499
	cmdOpenSettings   = 1500
	cmdOpenLog        = 1501
	cmdQuit           = 1599
)

var gainChoices = []int{50, 75, 100, 125, 150, 200, 250, 300}
var opacityChoices = []int{100, 90, 82, 72, 60, 50}

type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type point struct {
	X, Y int32
}

type winMsg struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// Icon manages the Windows system tray icon and dynamic popup menu on a dedicated UI thread.
type Icon struct {
	mu           sync.Mutex
	cb           Callbacks
	hwnd         uintptr
	icons        map[IconState]uintptr
	curState     IconState
	curTooltip   string
	msgTaskbarCr uint32
	trayAdded    bool

	readyCh   chan error
	doneCh    chan struct{}
	closeOnce sync.Once
}

var (
	trayInstMu   sync.Mutex
	trayInst     *Icon
	trayProcOnce sync.Once
	trayProcPtr  uintptr
)

func initTrayWndProc() {
	trayProcOnce.Do(func() {
		trayProcPtr = syscall.NewCallback(trayWndProc)
	})
}

func trayWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	t := trayInst
	if t != nil && (t.hwnd == 0 || t.hwnd == hwnd) {
		if handled, ret := t.handleMessage(hwnd, uint32(msg), wParam, lParam); handled {
			return ret
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// Start creates and registers the system tray icon.
func Start(cb Callbacks) (*Icon, error) {
	t := &Icon{
		cb:         cb,
		icons:      make(map[IconState]uintptr),
		curState:   IconIdle,
		curTooltip: "VoiceType — Idle",
		readyCh:    make(chan error, 1),
		doneCh:     make(chan struct{}),
	}
	initTrayWndProc()
	trayInstMu.Lock()
	trayInst = t
	trayInstMu.Unlock()

	go t.uiLoop()

	if err := <-t.readyCh; err != nil {
		return nil, err
	}
	return t, nil
}

// SetState updates the tray icon image and hover tooltip.
func (t *Icon) SetState(st IconState, tooltip string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.curState = st
	if tooltip != "" {
		t.curTooltip = tooltip
	}
	hwnd := t.hwnd
	t.mu.Unlock()

	if hwnd != 0 {
		_, _, _ = procPostMessageW.Call(hwnd, wmAppSetIcon, 0, 0)
	}
}

// Close removes the tray icon and exits the tray UI thread cleanly.
func (t *Icon) Close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() {
		t.mu.Lock()
		hwnd := t.hwnd
		t.mu.Unlock()
		if hwnd != 0 {
			_, _, _ = procPostMessageW.Call(hwnd, wmAppClose, 0, 0)
		}
		<-t.doneCh
		trayInstMu.Lock()
		if trayInst == t {
			trayInst = nil
		}
		trayInstMu.Unlock()
	})
}

// OpenPathInShell opens a file (e.g. config.json or voicetype.log) using the default Windows app.
func OpenPathInShell(path string) {
	verb, _ := syscall.UTF16PtrFromString("open")
	target, _ := syscall.UTF16PtrFromString(path)
	_, _, _ = procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, 1)
}

func loadIconFromICOBytes(ico []byte, targetSize int) uintptr {
	if len(ico) < 22 {
		return 0
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	if count == 0 || len(ico) < 6+count*16 {
		return 0
	}

	bestIdx := -1
	bestDiff := 1 << 30
	for i := 0; i < count; i++ {
		off := 6 + i*16
		w := int(ico[off])
		if w == 0 {
			w = 256
		}
		diff := w - targetSize
		if diff < 0 {
			diff = -diff + 1000 // prefer >= targetSize when downscaling
		}
		if diff < bestDiff {
			bestDiff = diff
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return 0
	}
	entryOff := 6 + bestIdx*16
	imgSize := binary.LittleEndian.Uint32(ico[entryOff+8 : entryOff+12])
	imgOff := binary.LittleEndian.Uint32(ico[entryOff+12 : entryOff+16])
	if int(imgOff+imgSize) > len(ico) || imgSize == 0 {
		return 0
	}
	imgBytes := ico[imgOff : imgOff+imgSize]
	hIcon, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&imgBytes[0])),
		uintptr(len(imgBytes)),
		1,          // TRUE = icon
		0x00030000, // version
		uintptr(targetSize),
		uintptr(targetSize),
		0,
	)
	return hIcon
}

func (t *Icon) loadEmbeddedIcons() {
	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	sz := int(cx)
	if sz < 16 {
		sz = 16
	}
	files := map[IconState]string{
		IconIdle:      "icons/voicetype.ico",
		IconRecording: "icons/voicetype_rec.ico",
		IconEditing:   "icons/voicetype_edit.ico",
		IconError:     "icons/voicetype_err.ico",
	}
	for st, name := range files {
		data, err := embeddedIcons.ReadFile(name)
		if err != nil {
			logging.Warnf("tray: failed reading embedded %s: %v", name, err)
			continue
		}
		if h := loadIconFromICOBytes(data, sz); h != 0 {
			t.icons[st] = h
		}
	}
}

func (t *Icon) destroyIcons() {
	for st, h := range t.icons {
		if h != 0 {
			_, _, _ = procDestroyIcon.Call(h)
		}
		delete(t.icons, st)
	}
}

func (t *Icon) uiLoop() {
	defer close(t.doneCh)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t.loadEmbeddedIcons()
	defer t.destroyIcons()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("VoiceTypeTrayWindow")
	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   trayProcPtr,
		HInstance:     hInst,
		LpszClassName: className,
	}
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	tcr, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	msgID, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(tcr)))
	t.msgTaskbarCr = uint32(msgID)

	title, _ := syscall.UTF16PtrFromString("VoiceTypeTray")
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0,
		0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		t.readyCh <- fmt.Errorf("CreateWindowExW tray failed: %v", err)
		return
	}

	t.mu.Lock()
	t.hwnd = hwnd
	t.mu.Unlock()

	if err := t.addOrUpdateIcon(hwnd, nimAdd); err != nil {
		logging.Warnf("tray: initial Shell_NotifyIconW(NIM_ADD) warning (will retry when taskbar is ready): %v", err)
		_, _, _ = procSetTimer.Call(hwnd, timerRetryTrayID, 2000, 0)
	} else {
		t.trayAdded = true
	}

	t.readyCh <- nil

	var msg winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (t *Icon) addOrUpdateIcon(hwnd uintptr, action uint32) error {
	t.mu.Lock()
	st := t.curState
	tip := t.curTooltip
	t.mu.Unlock()

	hIcon := t.icons[st]
	if hIcon == 0 {
		hIcon = t.icons[IconIdle]
	}

	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmAppTray
	nid.HIcon = hIcon
	tipU16, _ := syscall.UTF16FromString(transcript.TruncateRunes(tip, 120))
	copy(nid.SzTip[:], tipU16)

	r, _, err := procShellNotifyIconW.Call(uintptr(action), uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		return fmt.Errorf("Shell_NotifyIconW(%d) failed (cbSize=%d, hIcon=0x%X, hwnd=0x%X): %v", action, nid.CbSize, hIcon, hwnd, err)
	}
	return nil
}

func (t *Icon) removeIcon(hwnd uintptr) {
	var nid notifyIconDataW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func (t *Icon) handleMessage(hwnd uintptr, msg uint32, wParam, lParam uintptr) (bool, uintptr) {
	if t.msgTaskbarCr != 0 && msg == t.msgTaskbarCr {
		// Explorer restarted: clear old flag and re-add (arming retry timer if Explorer is still warming up!)
		t.trayAdded = false
		if err := t.addOrUpdateIcon(hwnd, nimAdd); err == nil {
			t.trayAdded = true
			_, _, _ = procKillTimer.Call(hwnd, timerRetryTrayID)
		} else {
			_, _, _ = procSetTimer.Call(hwnd, timerRetryTrayID, 2000, 0)
		}
		return true, 0
	}

	switch msg {
	case wmTimer:
		if wParam == timerRetryTrayID && !t.trayAdded {
			if err := t.addOrUpdateIcon(hwnd, nimAdd); err == nil {
				t.trayAdded = true
				_, _, _ = procKillTimer.Call(hwnd, timerRetryTrayID)
				logging.Infof("tray: Shell_NotifyIconW(NIM_ADD) succeeded on retry")
			}
		}
		return true, 0

	case wmAppSetIcon:
		if t.trayAdded {
			_ = t.addOrUpdateIcon(hwnd, nimModify)
		}
		return true, 0

	case wmAppClose:
		_, _, _ = procKillTimer.Call(hwnd, timerRetryTrayID)
		if t.trayAdded {
			t.removeIcon(hwnd)
			t.trayAdded = false
		}
		_, _, _ = procDestroyWindow.Call(hwnd)
		return true, 0

	case wmDestroy:
		_, _, _ = procPostQuitMessage.Call(0)
		return true, 0

	case wmAppTray:
		ev := uint32(lParam & 0xFFFF)
		switch ev {
		case wmRButtonUp, wmContextMenu:
			t.showContextMenu(hwnd)
		case wmLButtonUp:
			if t.cb.OnLeftClick != nil {
				go t.cb.OnLeftClick()
			}
		}
		return true, 0
	}
	return false, 0
}

func appendMenuItem(menu uintptr, flags uint32, id uintptr, label string) {
	if flags&mfSeparator != 0 {
		_, _, _ = procAppendMenuW.Call(menu, mfSeparator, 0, 0)
		return
	}
	// Escape '&' so Win32 doesn't render underscores on transcript previews or mic names
	escaped := ""
	for _, r := range label {
		if r == '&' {
			escaped += "&&"
		} else {
			escaped += string(r)
		}
	}
	ptr, _ := syscall.UTF16PtrFromString(escaped)
	_, _, _ = procAppendMenuW.Call(menu, uintptr(flags), id, uintptr(unsafe.Pointer(ptr)))
}

func (t *Icon) showContextMenu(hwnd uintptr) {
	var st MenuState
	if t.cb.GetMenuState != nil {
		st = t.cb.GetMenuState()
	}

	root, _, _ := procCreatePopupMenu.Call()
	if root == 0 {
		return
	}
	defer procDestroyMenu.Call(root)

	// Header
	header := st.StatusLine
	if header == "" {
		header = "VoiceType"
	}
	appendMenuItem(root, mfString|mfGrayed|mfDisabled, 0, header)
	appendMenuItem(root, mfSeparator, 0, "")

	// 1. Microphone submenu (capped at 90 entries so IDs never collide with cmdGainBase)
	shownDevices := st.Devices
	if len(shownDevices) > 90 {
		shownDevices = shownDevices[:90]
	}
	micMenu, _, _ := procCreatePopupMenu.Call()
	defFlags := uint32(mfString)
	if st.Config.MicrophoneID == "" {
		defFlags |= mfChecked
	}
	appendMenuItem(micMenu, defFlags, cmdMicDefault, "Default (Windows)")
	if len(shownDevices) > 0 {
		appendMenuItem(micMenu, mfSeparator, 0, "")
		for i, dev := range shownDevices {
			f := uint32(mfString)
			if st.Config.MicrophoneID == dev.ID {
				f |= mfChecked
			}
			label := dev.Name
			if dev.IsDefault {
				label += "  (System Default)"
			}
			appendMenuItem(micMenu, f, uintptr(cmdMicBase+i), label)
		}
	}
	appendMenuItem(root, mfPopup, micMenu, "Microphone")

	// 2. Input Gain submenu (50% - 300%)
	gainMenu, _, _ := procCreatePopupMenu.Call()
	for i, g := range gainChoices {
		f := uint32(mfString)
		if st.Config.GainPercent == g {
			f |= mfChecked
		}
		label := fmt.Sprintf("%d%%", g)
		if g == 100 {
			label = "100%  (Normal)"
		}
		appendMenuItem(gainMenu, f, uintptr(cmdGainBase+i), label)
	}
	appendMenuItem(root, mfPopup, gainMenu, fmt.Sprintf("Input Gain (%d%%)", st.Config.GainPercent))

	// 2b. Overlay Opacity submenu (semi-transparency so user can see behind the overlay!)
	opMenu, _, _ := procCreatePopupMenu.Call()
	for i, op := range opacityChoices {
		f := uint32(mfString)
		if st.Config.OverlayOpacity == op {
			f |= mfChecked
		}
		label := fmt.Sprintf("%d%%", op)
		switch op {
		case 100:
			label = "100%  (Solid)"
		case config.DefaultOverlayOpacity:
			label = fmt.Sprintf("%d%%  (Semi-transparent)", op)
		}
		appendMenuItem(opMenu, f, uintptr(cmdOpacityBase+i), label)
	}
	appendMenuItem(root, mfPopup, opMenu, fmt.Sprintf("Overlay Opacity (%d%%)", st.Config.OverlayOpacity))

	// 3. Insertion Method submenu (Fast Paste vs Unicode SendInput)
	insMenu, _, _ := procCreatePopupMenu.Call()
	pasteFlags := uint32(mfString)
	sendFlags := uint32(mfString)
	if st.Config.InsertionMethod == config.InsertMethodSendInput {
		sendFlags |= mfChecked
	} else {
		pasteFlags |= mfChecked
	}
	appendMenuItem(insMenu, pasteFlags, cmdInsertPaste, "Clipboard Paste (Fast, Ctrl+V)")
	appendMenuItem(insMenu, sendFlags, cmdInsertSend, "Type Unicode (SendInput)")
	appendMenuItem(root, mfPopup, insMenu, "Insertion Method")

	// 4. Hotkey Mode submenu (Toggle vs Push-to-Talk)
	modeMenu, _, _ := procCreatePopupMenu.Call()
	togFlags := uint32(mfString)
	pttFlags := uint32(mfString)
	if st.Config.PushToTalk {
		pttFlags |= mfChecked
	} else {
		togFlags |= mfChecked
	}
	appendMenuItem(modeMenu, togFlags, cmdModeToggle, fmt.Sprintf("Toggle (Press %s to start/stop)", st.Config.Hotkey))
	appendMenuItem(modeMenu, pttFlags, cmdModePTT, fmt.Sprintf("Push-to-Talk (Hold %s)", st.Config.Hotkey))
	appendMenuItem(root, mfPopup, modeMenu, "Dictation Mode")

	// 5. Recent Transcripts (last 10 — click to copy!)
	histMenu, _, _ := procCreatePopupMenu.Call()
	if len(st.RecentHistory) == 0 {
		appendMenuItem(histMenu, mfString|mfGrayed|mfDisabled, 0, "(No transcripts yet)")
	} else {
		for i, entry := range st.RecentHistory {
			if i >= config.TrayHistoryCount {
				break
			}
			preview := transcript.TruncateRunes(entry.Text, 54)
			label := fmt.Sprintf("%d. %s", i+1, preview)
			appendMenuItem(histMenu, mfString, uintptr(cmdHistBase+i), label)
		}
		appendMenuItem(histMenu, mfSeparator, 0, "")
		appendMenuItem(histMenu, mfString, cmdHistClear, "Clear History")
	}
	appendMenuItem(root, mfPopup, histMenu, fmt.Sprintf("Recent Transcripts (%d)", len(st.RecentHistory)))

	appendMenuItem(root, mfSeparator, 0, "")

	// Options toggles
	optFlag := func(checked bool) uint32 {
		if checked {
			return mfString | mfChecked
		}
		return mfString
	}
	appendMenuItem(root, optFlag(st.StartWithWin), cmdToggleStartup, "Start with Windows")
	appendMenuItem(root, optFlag(st.Config.SoundCues), cmdToggleSounds, "Sound Cues")
	appendMenuItem(root, optFlag(st.Config.SaveDebugWAVs), cmdToggleDebugWAV, "Save Debug WAVs")
	appendMenuItem(root, optFlag(st.Config.AICleanupEnabled), cmdToggleAIClean, "AI Clean-up (Claude)")

	appendMenuItem(root, mfSeparator, 0, "")
	appendMenuItem(root, mfString, cmdCheckModel, fmt.Sprintf("Check Soniox Model (%s)…", st.Config.Model))
	appendMenuItem(root, mfString, cmdSetAPIKey, "Set Soniox API Key…")
	appendMenuItem(root, mfString, cmdOpenSettings, "Open Settings File")
	appendMenuItem(root, mfString, cmdOpenLog, "Open Log File")
	appendMenuItem(root, mfSeparator, 0, "")
	appendMenuItem(root, mfString, cmdQuit, "Quit VoiceType")

	// Win32 requirement: SetForegroundWindow before TrackPopupMenuEx so clicking outside dismisses cleanly.
	var pt point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	_, _, _ = procSetForegroundWindow.Call(hwnd)

	cmd, _, _ := procTrackPopupMenuEx.Call(
		root,
		uintptr(tpmRightButton|tpmReturnCmd|tpmNonotify),
		uintptr(pt.X),
		uintptr(pt.Y),
		hwnd,
		0,
	)
	_, _, _ = procPostMessageW.Call(hwnd, wmNull, 0, 0)

	if cmd == 0 {
		return
	}
	id := int(cmd)
	switch {
	case id == cmdMicDefault:
		if t.cb.OnSelectMic != nil {
			go t.cb.OnSelectMic("", "Default")
		}
	case id >= cmdMicBase && id < cmdMicBase+len(shownDevices):
		dev := shownDevices[id-cmdMicBase]
		if t.cb.OnSelectMic != nil {
			go t.cb.OnSelectMic(dev.ID, dev.Name)
		}
	case id >= cmdGainBase && id < cmdGainBase+len(gainChoices):
		g := gainChoices[id-cmdGainBase]
		if t.cb.OnSelectGain != nil {
			go t.cb.OnSelectGain(g)
		}
	case id >= cmdOpacityBase && id < cmdOpacityBase+len(opacityChoices):
		op := opacityChoices[id-cmdOpacityBase]
		if t.cb.OnSelectOpacity != nil {
			go t.cb.OnSelectOpacity(op)
		}
	case id == cmdInsertPaste:
		if t.cb.OnSelectInsertMethod != nil {
			go t.cb.OnSelectInsertMethod(config.InsertMethodPaste)
		}
	case id == cmdInsertSend:
		if t.cb.OnSelectInsertMethod != nil {
			go t.cb.OnSelectInsertMethod(config.InsertMethodSendInput)
		}
	case id == cmdModeToggle:
		if t.cb.OnSelectPushToTalk != nil {
			go t.cb.OnSelectPushToTalk(false)
		}
	case id == cmdModePTT:
		if t.cb.OnSelectPushToTalk != nil {
			go t.cb.OnSelectPushToTalk(true)
		}
	case id >= cmdHistBase && id < cmdHistBase+len(st.RecentHistory):
		entry := st.RecentHistory[id-cmdHistBase]
		if t.cb.OnCopyHistory != nil {
			go t.cb.OnCopyHistory(entry.Text)
		}
	case id == cmdHistClear:
		if t.cb.OnClearHistory != nil {
			go t.cb.OnClearHistory()
		}
	case id == cmdToggleStartup:
		if t.cb.OnToggleStartWithWin != nil {
			go t.cb.OnToggleStartWithWin(!st.StartWithWin)
		}
	case id == cmdToggleSounds:
		if t.cb.OnToggleSoundCues != nil {
			go t.cb.OnToggleSoundCues(!st.Config.SoundCues)
		}
	case id == cmdToggleDebugWAV:
		if t.cb.OnToggleDebugWAVs != nil {
			go t.cb.OnToggleDebugWAVs(!st.Config.SaveDebugWAVs)
		}
	case id == cmdToggleAIClean:
		if t.cb.OnToggleAICleanup != nil {
			go t.cb.OnToggleAICleanup(!st.Config.AICleanupEnabled)
		}
	case id == cmdCheckModel:
		if t.cb.OnCheckModelUpdate != nil {
			go t.cb.OnCheckModelUpdate()
		}
	case id == cmdSetAPIKey:
		if t.cb.OnSetAPIKey != nil {
			go t.cb.OnSetAPIKey()
		}
	case id == cmdOpenSettings:
		if t.cb.OnOpenSettings != nil {
			go t.cb.OnOpenSettings()
		}
	case id == cmdOpenLog:
		if t.cb.OnOpenLog != nil {
			go t.cb.OnOpenLog()
		}
	case id == cmdQuit:
		if t.cb.OnQuit != nil {
			go t.cb.OnQuit()
		}
	}
}
