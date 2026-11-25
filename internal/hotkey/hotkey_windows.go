//go:build windows

package hotkey

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"voicetype/internal/logging"
)

// Mode controls which keys the low-level keyboard hook intercepts and swallows.
type Mode int32

const (
	ModeIdle      Mode = 0 // Only the configured dictation hotkey is intercepted
	ModeRecording Mode = 1 // Hotkey, Enter, and Esc are intercepted and swallowed
	ModeEditing   Mode = 2 // Hotkey, non-Shift Enter, and Esc are intercepted and swallowed
)

const (
	ModCtrl  uint32 = 1 << 0
	ModAlt   uint32 = 1 << 1
	ModShift uint32 = 1 << 2
	ModWin   uint32 = 1 << 3
)

// Spec defines a keyboard shortcut as modifier flags plus a Windows Virtual-Key code.
type Spec struct {
	Raw  string
	Mods uint32
	VK   uint32
}

// ParseSpec parses strings like "F9", "F8", "Ctrl+Shift+Space", "Alt+Space", "Pause".
func ParseSpec(raw string) (Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "F9"
	}
	parts := strings.Split(raw, "+")
	var mods uint32
	var vk uint32
	for i, part := range parts {
		tok := strings.ToUpper(strings.TrimSpace(part))
		if tok == "" {
			continue
		}
		isLast := i == len(parts)-1
		if !isLast {
			switch tok {
			case "CTRL", "CONTROL":
				mods |= ModCtrl
				continue
			case "ALT":
				mods |= ModAlt
				continue
			case "SHIFT":
				mods |= ModShift
				continue
			case "WIN", "WINDOWS", "META", "SUPER":
				mods |= ModWin
				continue
			}
		}
		code, ok := keyNameToVK(tok)
		if !ok {
			return Spec{}, fmt.Errorf("unknown hotkey token %q in %q", part, raw)
		}
		vk = code
	}
	if vk == 0 {
		return Spec{}, fmt.Errorf("hotkey %q has no primary key", raw)
	}
	return Spec{Raw: raw, Mods: mods, VK: vk}, nil
}

func keyNameToVK(tok string) (uint32, bool) {
	// Function keys F1..F24
	if strings.HasPrefix(tok, "F") && len(tok) >= 2 && len(tok) <= 3 {
		var n int
		if _, err := fmt.Sscanf(tok[1:], "%d", &n); err == nil && n >= 1 && n <= 24 {
			return uint32(0x70 + n - 1), true
		}
	}
	// Single alphanumeric key A-Z, 0-9
	if len(tok) == 1 {
		c := tok[0]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return uint32(c), true
		}
	}
	switch tok {
	case "SPACE", "SPACEBAR":
		return 0x20, true
	case "PAUSE", "BREAK":
		return 0x13, true
	case "SCROLLLOCK", "SCROLL":
		return 0x91, true
	case "CAPSLOCK", "CAPS":
		return 0x14, true
	case "INSERT", "INS":
		return 0x2D, true
	case "HOME":
		return 0x24, true
	case "END":
		return 0x23, true
	case "PAGEUP", "PGUP":
		return 0x21, true
	case "PAGEDOWN", "PGDN":
		return 0x22, true
	case "PRINT", "PRINTSCREEN", "PRTSC":
		return 0x2C, true
	case "TILDE", "GRAVE", "`", "OEM_3":
		return 0xC0, true
	case "APPS", "MENU":
		return 0x5D, true
	case "RALT", "RMENU":
		return 0xA5, true
	case "RCTRL", "RCONTROL":
		return 0xA3, true
	case "RSHIFT":
		return 0xA1, true
	case "NUMPAD0":
		return 0x60, true
	case "NUMPAD1":
		return 0x61, true
	case "NUMPAD2":
		return 0x62, true
	case "NUMPAD3":
		return 0x63, true
	case "NUMPAD4":
		return 0x64, true
	case "NUMPAD5":
		return 0x65, true
	case "NUMPAD6":
		return 0x66, true
	case "NUMPAD7":
		return 0x67, true
	case "NUMPAD8":
		return 0x68, true
	case "NUMPAD9":
		return 0x69, true
	}
	return 0, false
}

// Callbacks receive asynchronous hotkey/control-key notifications (invoked off the hook thread!).
type Callbacks struct {
	OnHotkeyDown func()
	OnHotkeyUp   func()
	OnEnter      func()
	OnEscape     func()
}

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procGetCurrentThreadID  = kernel32.NewProc("GetCurrentThreadId")
)

const (
	whKeyboardLL  = 13
	wmKeyDown     = 0x0100
	wmKeyUp       = 0x0101
	wmSysKeyDown  = 0x0104
	wmSysKeyUp    = 0x0105
	wmQuit        = 0x0012
	llkhfInjected = 0x10

	vkReturn   = 0x0D
	vkEscape   = 0x1B
	vkShift    = 0x10
	vkControl  = 0x11
	vkMenu     = 0x12 // Alt
	vkLWin     = 0x5B
	vkRWin     = 0x5C
)

type kbdLLHookStruct struct {
	VKCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type winMsg struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	PtX     int32
	PtY     int32
}

// Manager runs a dedicated Windows WH_KEYBOARD_LL hook thread so hotkeys, Enter, and Esc
// are captured and swallowed reliably without leaking to target apps or blocking Windows.
type Manager struct {
	mu           sync.RWMutex
	spec         Spec
	cb           Callbacks
	mode         atomic.Int32
	threadID     atomic.Uint32
	hookHandle   uintptr
	hotkeyDown   bool
	hotkeyDownVK uint32
	downKeys     [256]bool
	swallowedVK  [256]bool

	eventCh chan func()
	readyCh chan error
	doneCh  chan struct{}
	stopMu  sync.Mutex
	stopped bool
}

var (
	activeMgrMu sync.Mutex
	activeMgr   *Manager
	hookCbOnce  sync.Once
	hookCbPtr   uintptr
)

func initHookCallback() {
	hookCbOnce.Do(func() {
		hookCbPtr = syscall.NewCallback(lowLevelKeyboardProc)
	})
}

func lowLevelKeyboardProc(nCode uintptr, wParam uintptr, lParam uintptr) uintptr {
	if int32(nCode) >= 0 && lParam != 0 {
		mgr := activeMgr
		if mgr != nil {
			kb := (*kbdLLHookStruct)(unsafe.Pointer(lParam))
			if mgr.handleKeyEvent(wParam, kb) {
				return 1 // swallow key completely
			}
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
}

// NewManager creates and starts the low-level keyboard hook manager.
func NewManager(hotkeySpec string, cb Callbacks) (*Manager, error) {
	spec, err := ParseSpec(hotkeySpec)
	if err != nil {
		logging.Warnf("hotkey: invalid %q (%v); defaulting to F9", hotkeySpec, err)
		spec, _ = ParseSpec("F9")
	}

	m := &Manager{
		spec:    spec,
		cb:      cb,
		eventCh: make(chan func(), 64),
		readyCh: make(chan error, 1),
		doneCh:  make(chan struct{}),
	}

	initHookCallback()
	activeMgrMu.Lock()
	activeMgr = m
	activeMgrMu.Unlock()

	// Async event dispatcher so callbacks NEVER run inside the WH_KEYBOARD_LL hook!
	go func() {
		for fn := range m.eventCh {
			if fn != nil {
				fn()
			}
		}
	}()

	go m.hookLoop()

	if err := <-m.readyCh; err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

// SetMode atomically updates the hook's interception mode (Idle, Recording, Editing).
func (m *Manager) SetMode(mode Mode) {
	if m == nil {
		return
	}
	m.mode.Store(int32(mode))
}

// SetHotkey updates the active hotkey specification at runtime.
func (m *Manager) SetHotkey(hotkeySpec string) error {
	spec, err := ParseSpec(hotkeySpec)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.spec = spec
	m.mu.Unlock()
	logging.Infof("hotkey: updated to %s (vk=0x%X mods=0x%X)", spec.Raw, spec.VK, spec.Mods)
	return nil
}

// Spec returns the currently configured hotkey Spec.
func (m *Manager) Spec() Spec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.spec
}

// Close unhooks WH_KEYBOARD_LL and stops the dedicated message loop cleanly.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.stopMu.Lock()
	if m.stopped {
		m.stopMu.Unlock()
		return
	}
	m.stopped = true
	m.stopMu.Unlock()

	if tid := m.threadID.Load(); tid != 0 {
		_, _, _ = procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
	}
	<-m.doneCh

	activeMgrMu.Lock()
	if activeMgr == m {
		activeMgr = nil
	}
	activeMgrMu.Unlock()
	close(m.eventCh)
}

func (m *Manager) dispatch(fn func(), critical bool) {
	if fn == nil {
		return
	}
	select {
	case m.eventCh <- fn:
		return
	default:
	}
	if critical {
		// Evict one stale queued event so PTT release (OnHotkeyUp) is NEVER silently dropped!
		select {
		case <-m.eventCh:
		default:
		}
		select {
		case m.eventCh <- fn:
		default:
		}
	}
}

func (m *Manager) hookLoop() {
	defer close(m.doneCh)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := procGetCurrentThreadID.Call()
	m.threadID.Store(uint32(tid))

	hook, _, err := procSetWindowsHookExW.Call(whKeyboardLL, hookCbPtr, 0, 0)
	if hook == 0 {
		m.readyCh <- fmt.Errorf("SetWindowsHookExW(WH_KEYBOARD_LL) failed: %v", err)
		return
	}
	m.hookHandle = hook
	defer procUnhookWindowsHookEx.Call(hook)
	logging.Infof("hotkey: WH_KEYBOARD_LL active (hotkey=%s)", m.Spec().Raw)
	m.readyCh <- nil

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

func keyPressed(vk uintptr) bool {
	st, _, _ := procGetAsyncKeyState.Call(vk)
	return (uint16(st) & 0x8000) != 0
}

func currentMods() uint32 {
	var mods uint32
	if keyPressed(vkControl) {
		mods |= ModCtrl
	}
	if keyPressed(vkMenu) {
		mods |= ModAlt
	}
	if keyPressed(vkShift) {
		mods |= ModShift
	}
	if keyPressed(vkLWin) || keyPressed(vkRWin) {
		mods |= ModWin
	}
	return mods
}

// handleKeyEvent runs synchronously on the hook thread; must remain ultra-fast and non-blocking.
func (m *Manager) handleKeyEvent(wParam uintptr, kb *kbdLLHookStruct) bool {
	// Never intercept synthetic/injected keystrokes from SendInput (e.g. our own Ctrl+V or typing!)
	if kb.Flags&llkhfInjected != 0 {
		return false
	}

	isDown := wParam == wmKeyDown || wParam == wmSysKeyDown
	isUp := wParam == wmKeyUp || wParam == wmSysKeyUp
	if !isDown && !isUp {
		return false
	}

	vk := kb.VKCode
	m.mu.RLock()
	spec := m.spec
	m.mu.RUnlock()

	// 1. Handle key-up of any previously pressed/swallowed key so target app never sees orphaned key-ups.
	if isUp {
		if m.hotkeyDown && vk == m.hotkeyDownVK {
			m.hotkeyDown = false
			m.hotkeyDownVK = 0
			if vk < 256 {
				m.downKeys[vk] = false
				m.swallowedVK[vk] = false
			}
			m.dispatch(m.cb.OnHotkeyUp, true)
			return true
		}
		if vk < 256 {
			m.downKeys[vk] = false
			if m.swallowedVK[vk] {
				m.swallowedVK[vk] = false
				return true
			}
		}
		return false
	}

	// 2. Latch OS auto-repeats on the hook thread so holding Esc, Enter, or hotkey never fires twice!
	if vk < 256 && m.downKeys[vk] {
		return m.swallowedVK[vk]
	}

	// 3. Check configured dictation hotkey on initial key-down.
	if vk == spec.VK {
		mods := currentMods()
		if mods == spec.Mods {
			m.hotkeyDown = true
			m.hotkeyDownVK = vk
			if vk < 256 {
				m.downKeys[vk] = true
				m.swallowedVK[vk] = true
			}
			m.dispatch(m.cb.OnHotkeyDown, false)
			return true
		}
	}

	// 4. Mode-specific Enter & Esc swallowing while recording or editing (latched on first press).
	mode := Mode(m.mode.Load())
	switch mode {
	case ModeRecording:
		if vk == vkReturn {
			m.downKeys[vkReturn] = true
			m.swallowedVK[vkReturn] = true
			m.dispatch(m.cb.OnEnter, false)
			return true
		}
		if vk == vkEscape {
			m.downKeys[vkEscape] = true
			m.swallowedVK[vkEscape] = true
			m.dispatch(m.cb.OnEscape, false)
			return true
		}
	case ModeEditing:
		// Enter (without Shift) submits; Shift+Enter inserts a newline in the edit box!
		if vk == vkReturn && !keyPressed(vkShift) {
			m.downKeys[vkReturn] = true
			m.swallowedVK[vkReturn] = true
			m.dispatch(m.cb.OnEnter, false)
			return true
		}
		// Second Esc clears everything and restarts recording fresh.
		if vk == vkEscape {
			m.downKeys[vkEscape] = true
			m.swallowedVK[vkEscape] = true
			m.dispatch(m.cb.OnEscape, false)
			return true
		}
	}

	if vk < 256 {
		m.downKeys[vk] = true
		m.swallowedVK[vk] = false
	}
	return false
}
