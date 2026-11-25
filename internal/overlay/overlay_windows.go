//go:build windows

package overlay

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"voicetype/internal/logging"
)

// Kind determines the overlay's visual theme, status pill, and hint line.
type Kind int

const (
	KindHidden Kind = iota
	KindConnecting
	KindListening
	KindEditing
	KindReconnecting
	KindError
	KindNotice
)

// Snapshot describes the immutable state rendered by the overlay.
type Snapshot struct {
	Kind             Kind
	EditActive       bool // True whenever Edit mode is active (independent of KindReconnecting!)
	ClearedOnce      bool // True right after 2nd Esc; pressing Esc a 3rd time closes the overlay
	OpacityPercent   int  // 40..100 (default 82 = semi-transparent so user can see behind overlay)
	StatusText       string
	FinalText        string
	NonFinalText     string
	PostEditFinal    string // Live speech arriving during edit mode
	PostEditNonFinal string
	ErrorBanner      string
	HotkeyLabel      string
	StartTime        time.Time
	TargetHWND       uintptr // Focused window when dictation started (selects monitor)
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")

	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procGetMessageW           = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procSendMessageW          = user32.NewProc("SendMessageW")
	procShowWindow            = user32.NewProc("ShowWindow")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procInvalidateRect        = user32.NewProc("InvalidateRect")
	procBeginPaint            = user32.NewProc("BeginPaint")
	procEndPaint              = user32.NewProc("EndPaint")
	procGetClientRect         = user32.NewProc("GetClientRect")
	procFillRect              = user32.NewProc("FillRect")
	procMonitorFromWindow     = user32.NewProc("MonitorFromWindow")
	procMonitorFromPoint      = user32.NewProc("MonitorFromPoint")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procGetMonitorInfoW       = user32.NewProc("GetMonitorInfoW")
	procGetWindowLongPtrW     = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW     = user32.NewProc("SetWindowLongPtrW")
	procSetLayeredWindowAttrs = user32.NewProc("SetLayeredWindowAttributes")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procSetFocus              = user32.NewProc("SetFocus")
	procGetWindowTextLengthW  = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW        = user32.NewProc("GetWindowTextW")
	procSetWindowTextW        = user32.NewProc("SetWindowTextW")
	procMoveWindow            = user32.NewProc("MoveWindow")
	procSetTimer              = user32.NewProc("SetTimer")
	procKillTimer             = user32.NewProc("KillTimer")
	procLoadCursorW           = user32.NewProc("LoadCursorW")
	procGetKeyState           = user32.NewProc("GetKeyState")
	procSetProcessDpiCtx      = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	procCreatePen              = gdi32.NewProc("CreatePen")
	procRoundRect              = gdi32.NewProc("RoundRect")
	procEllipse                = gdi32.NewProc("Ellipse")
	procSetBkMode              = gdi32.NewProc("SetBkMode")
	procSetBkColor             = gdi32.NewProc("SetBkColor")
	procSetTextColor           = gdi32.NewProc("SetTextColor")
	procCreateFontW            = gdi32.NewProc("CreateFontW")
	procTextOutW               = gdi32.NewProc("TextOutW")
	procGetTextExtentPoint32W  = gdi32.NewProc("GetTextExtentPoint32W")

	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	procGetDpiForMonitor      = shcore.NewProc("GetDpiForMonitor")
)

const (
	wsPopup        = 0x80000000
	wsChild        = 0x40000000
	wsVisible      = 0x10000000
	wsVScroll      = 0x00200000
	wsClipChildren = 0x02000000

	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080
	wsExLayered    = 0x00080000
	wsExNoActivate = 0x08000000

	lwaAlpha = 0x00000002

	esMultiline   = 0x0004
	esAutoVScroll = 0x0040
	esWantReturn  = 0x1000

	emSetSel       = 0x00B1
	emScrollCaret  = 0x00B7
	emSetMargins   = 0x00D3
	ecLeftMargin   = 0x0001
	ecRightMargin  = 0x0002

	gwlExStyle = ^uintptr(19) // -20

	swHide           = 0
	swShow           = 5
	swShowNoActivate = 4

	swpNoSize       = 0x0001
	swpNoMove       = 0x0002
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
	swpShowWindow   = 0x0040

	hwndTopmost = ^uintptr(0) // -1

	wmDestroy      = 0x0002
	wmPaint        = 0x000F
	wmEraseBkgnd   = 0x0014
	wmSetFont      = 0x0030
	wmTimer        = 0x0113
	wmCtlColorEdit = 0x0133
	wmDpiChanged   = 0x02E0
	wmAppUpdate    = 0x8001
	wmAppLevel     = 0x8002
	wmAppHide      = 0x8003
	wmAppReadEdit  = 0x8004
	wmAppClose     = 0x8005
	wmAppEnterEdit = 0x8006
	wmAppExitEdit  = 0x8007

	timerClockID = 1

	monitorDefaultToNearest = 2
	mdtEffectiveDPI         = 0

	transparentBk = 1
	srcCopy       = 0x00CC0020
	psSolid       = 0
	psNull        = 5

	fwRegular  = 400
	fwMedium   = 500
	fwSemiBold = 600

	cleartypeQuality = 5
	idcArrow         = 32512

	dwmwaWindowCornerPreference = 33
	dwmwcpRound                 = 2
	dwmwaBorderColor            = 34
)

// RGB helper converting (r,g,b) to Win32 COLORREF (0x00BBGGRR).
func rgb(r, g, b byte) uint32 {
	return uint32(r) | (uint32(g) << 8) | (uint32(b) << 16)
}

var (
	colBg         = rgb(17, 22, 30)   // Deep slate card
	colSurface    = rgb(26, 34, 46)   // Edit box / pill surface
	colTrack      = rgb(36, 47, 63)   // Meter track
	colTextFinal  = rgb(244, 247, 251)
	colTextDraft  = rgb(138, 164, 184)
	colTextHint   = rgb(116, 134, 154)
	colTeal       = rgb(45, 212, 191)
	colTealBright = rgb(94, 234, 212)
	colCoral      = rgb(244, 63, 94)
	colAmber      = rgb(245, 158, 11)
	colRed        = rgb(239, 68, 68)
	colBorderIdle = rgb(45, 72, 86)
)

type rect struct {
	Left, Top, Right, Bottom int32
}

type point struct {
	X, Y int32
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
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

type paintStruct struct {
	HDC        uintptr
	FErase     int32
	RcPaint    rect
	FRestore   int32
	FIncUpdate int32
	RgbRes     [32]byte
}

type winMsg struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type sizeStruct struct {
	CX, CY int32
}

// EnablePerMonitorDPIv2 enables Per-Monitor DPI Awareness v2 at process startup.
func EnablePerMonitorDPIv2() {
	if procSetProcessDpiCtx.Find() == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (DPI_AWARENESS_CONTEXT)-4
		_, _, _ = procSetProcessDpiCtx.Call(^uintptr(3))
	}
}

type styledSegment struct {
	text  string
	draft bool
}

type styledLine struct {
	segs []styledSegment
}

// Window manages the topmost, per-monitor DPI aware dictation overlay and its
// flicker-free Edit mode control.
type Window struct {
	mu          sync.Mutex
	snap        Snapshot
	level       float64
	visible     bool
	editingUI   bool
	dpi         uint32
	winWidth    int32
	winHeight   int32
	editLayoutW int32
	editLayoutD uint32
	curAlpha    byte
	meterRect   rect
	clockRect   rect

	hwnd       uintptr
	editHWND   uintptr
	editBrush  uintptr
	fontMain   uintptr
	fontBold   uintptr
	fontSmall  uintptr
	fontMono   uintptr

	editInitCh  chan string
	readEditReq chan chan string
	readyCh     chan error
	doneCh      chan struct{}
	closeOnce   sync.Once
}

var (
	overlayInstMu sync.Mutex
	overlayInst   *Window
	wndProcOnce   sync.Once
	wndProcPtr    uintptr
)

func initWndProc() {
	wndProcOnce.Do(func() {
		wndProcPtr = syscall.NewCallback(overlayWndProc)
	})
}

func overlayWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	w := overlayInst
	if w != nil && (w.hwnd == 0 || w.hwnd == hwnd) {
		if handled, ret := w.handleMessage(hwnd, uint32(msg), wParam, lParam); handled {
			return ret
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// New creates and starts the overlay window on a dedicated Win32 UI thread.
func New() (*Window, error) {
	w := &Window{
		dpi:         96,
		editInitCh:  make(chan string, 8),
		readEditReq: make(chan chan string, 4),
		readyCh:     make(chan error, 1),
		doneCh:      make(chan struct{}),
	}
	initWndProc()
	overlayInstMu.Lock()
	overlayInst = w
	overlayInstMu.Unlock()

	go w.uiLoop()

	if err := <-w.readyCh; err != nil {
		return nil, err
	}
	return w, nil
}

// Update applies a new overlay snapshot (thread-safe, non-blocking).
// Preserves audio level and never clobbers an open Edit control.
func (w *Window) Update(snap Snapshot) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.snap = snap
	hwnd := w.hwnd
	w.mu.Unlock()

	if hwnd != 0 {
		_, _, _ = procPostMessageW.Call(hwnd, wmAppUpdate, 0, 0)
	}
}

// EnterEditMode opens and focuses the native EDIT box with initialText via a dedicated
// UI command that cannot be overwritten by concurrent token or reconnect updates.
func (w *Window) EnterEditMode(initialText string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	hwnd := w.hwnd
	w.mu.Unlock()
	if hwnd == 0 {
		return
	}
	select {
	case w.editInitCh <- initialText:
	default:
		// Drain stale pending entry if any
		select {
		case <-w.editInitCh:
		default:
		}
		w.editInitCh <- initialText
	}
	_, _, _ = procPostMessageW.Call(hwnd, wmAppEnterEdit, 0, 0)
}

// ExitEditMode hides and clears the native EDIT box and restores non-activating overlay mode.
func (w *Window) ExitEditMode() {
	if w == nil {
		return
	}
	w.mu.Lock()
	hwnd := w.hwnd
	w.mu.Unlock()
	if hwnd != 0 {
		_, _, _ = procPostMessageW.Call(hwnd, wmAppExitEdit, 0, 0)
	}
}

// SetLevel updates only the audio level meter (invalidates ONLY the meter rect — zero edit-box blink!).
func (w *Window) SetLevel(level float64) {
	if w == nil {
		return
	}
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}
	w.mu.Lock()
	// Smooth decay/attack so the bar feels fluid
	if level > w.level {
		w.level = w.level*0.35 + level*0.65
	} else {
		w.level = w.level*0.72 + level*0.28
	}
	hwnd := w.hwnd
	vis := w.visible
	w.mu.Unlock()

	if hwnd != 0 && vis {
		_, _, _ = procPostMessageW.Call(hwnd, wmAppLevel, 0, 0)
	}
}

// Hide hides the overlay and cleans up edit mode so an old edit box never lingers.
func (w *Window) Hide() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.snap.Kind = KindHidden
	hwnd := w.hwnd
	w.mu.Unlock()

	if hwnd != 0 {
		_, _, _ = procPostMessageW.Call(hwnd, wmAppHide, 0, 0)
	}
}

// ReadEditText synchronously reads the current text inside the edit box on the UI thread.
func (w *Window) ReadEditText() (string, error) {
	if w == nil {
		return "", fmt.Errorf("overlay is nil")
	}
	w.mu.Lock()
	hwnd := w.hwnd
	w.mu.Unlock()
	if hwnd == 0 {
		return "", fmt.Errorf("overlay window not initialized")
	}
	reply := make(chan string, 1)
	w.readEditReq <- reply
	r, _, err := procPostMessageW.Call(hwnd, wmAppReadEdit, 0, 0)
	if r == 0 {
		// Remove queued request if PostMessage failed
		select {
		case <-w.readEditReq:
		default:
		}
		return "", fmt.Errorf("PostMessageW(wmAppReadEdit) failed: %v", err)
	}
	select {
	case txt := <-reply:
		return txt, nil
	case <-time.After(750 * time.Millisecond):
		return "", fmt.Errorf("timed out reading edit box text")
	}
}

// Close destroys the overlay window and exits its UI thread.
func (w *Window) Close() {
	if w == nil {
		return
	}
	w.closeOnce.Do(func() {
		w.mu.Lock()
		hwnd := w.hwnd
		w.mu.Unlock()
		if hwnd != 0 {
			_, _, _ = procPostMessageW.Call(hwnd, wmAppClose, 0, 0)
		}
		<-w.doneCh
		overlayInstMu.Lock()
		if overlayInst == w {
			overlayInst = nil
		}
		overlayInstMu.Unlock()
	})
}

func (w *Window) dip(v int32) int32 {
	dpi := w.dpi
	if dpi == 0 {
		dpi = 96
	}
	return int32(math.Round(float64(v) * float64(dpi) / 96.0))
}

func (w *Window) uiLoop() {
	defer close(w.doneCh)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("VoiceTypeOverlayWindow")
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   wndProcPtr,
		HInstance:     hInst,
		HCursor:       cursor,
		LpszClassName: className,
	}
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	title, _ := syscall.UTF16PtrFromString("VoiceType")
	hwnd, _, err := procCreateWindowExW.Call(
		uintptr(wsExTopmost|wsExToolWindow|wsExNoActivate|wsExLayered),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsPopup|wsClipChildren),
		0, 0, 520, 120,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		w.readyCh <- fmt.Errorf("CreateWindowExW overlay failed: %v", err)
		return
	}

	// Set initial semi-transparent alpha before first show (82% default)
	w.curAlpha = 209
	_, _, _ = procSetLayeredWindowAttrs.Call(hwnd, 0, uintptr(w.curAlpha), lwaAlpha)

	w.mu.Lock()
	w.hwnd = hwnd
	w.mu.Unlock()

	// Enable Windows 11 rounded corners
	pref := uint32(dwmwcpRound)
	_, _, _ = procDwmSetWindowAttribute.Call(hwnd, dwmwaWindowCornerPreference, uintptr(unsafe.Pointer(&pref)), unsafe.Sizeof(pref))

	w.editBrush, _, _ = procCreateSolidBrush.Call(uintptr(colSurface))
	w.recreateFonts(96)

	// Create hidden child EDIT control for Edit Mode
	editClass, _ := syscall.UTF16PtrFromString("EDIT")
	emptyText, _ := syscall.UTF16PtrFromString("")
	editHWND, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(editClass)),
		uintptr(unsafe.Pointer(emptyText)),
		uintptr(wsChild|wsVScroll|esMultiline|esAutoVScroll|esWantReturn),
		0, 0, 480, 96,
		hwnd, 101, hInst, 0,
	)
	if editHWND == 0 {
		logging.Warnf("overlay: failed to create EDIT child control: %v", err)
	} else {
		w.editHWND = editHWND
		_, _, _ = procSendMessageW.Call(editHWND, wmSetFont, w.fontMain, 1)
		margins := uintptr(8) | (uintptr(8) << 16)
		_, _, _ = procSendMessageW.Call(editHWND, emSetMargins, ecLeftMargin|ecRightMargin, margins)
	}

	w.readyCh <- nil

	var msg winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		// Enable Ctrl+A (Select All) inside the multiline Win32 EDIT control
		if msg.HWND == w.editHWND && msg.Message == 0x0100 && msg.WParam == 'A' {
			ctrlState, _, _ := procGetKeyState.Call(0x11) // VK_CONTROL
			if int16(ctrlState) < 0 {
				_, _, _ = procSendMessageW.Call(w.editHWND, emSetSel, 0, ^uintptr(0))
				continue
			}
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	w.destroyFonts()
	if w.editBrush != 0 {
		_, _, _ = procDeleteObject.Call(w.editBrush)
		w.editBrush = 0
	}
}

func (w *Window) recreateFonts(dpi uint32) {
	w.destroyFonts()
	w.dpi = dpi
	scale := func(pt int32) int32 {
		return -int32(math.Round(float64(pt) * float64(dpi) / 72.0))
	}
	segoe, _ := syscall.UTF16PtrFromString("Segoe UI")
	consolas, _ := syscall.UTF16PtrFromString("Consolas")

	w.fontMain, _, _ = procCreateFontW.Call(
		uintptr(scale(11)), 0, 0, 0, fwRegular, 0, 0, 0, 1, 0, 0, cleartypeQuality, 0,
		uintptr(unsafe.Pointer(segoe)),
	)
	w.fontBold, _, _ = procCreateFontW.Call(
		uintptr(scale(10)), 0, 0, 0, fwSemiBold, 0, 0, 0, 1, 0, 0, cleartypeQuality, 0,
		uintptr(unsafe.Pointer(segoe)),
	)
	w.fontSmall, _, _ = procCreateFontW.Call(
		uintptr(scale(9)), 0, 0, 0, fwRegular, 0, 0, 0, 1, 0, 0, cleartypeQuality, 0,
		uintptr(unsafe.Pointer(segoe)),
	)
	w.fontMono, _, _ = procCreateFontW.Call(
		uintptr(scale(9)), 0, 0, 0, fwMedium, 0, 0, 0, 1, 0, 0, cleartypeQuality, 0,
		uintptr(unsafe.Pointer(consolas)),
	)
	if w.editHWND != 0 && w.fontMain != 0 {
		_, _, _ = procSendMessageW.Call(w.editHWND, wmSetFont, w.fontMain, 1)
	}
}

func (w *Window) destroyFonts() {
	for _, f := range []*uintptr{&w.fontMain, &w.fontBold, &w.fontSmall, &w.fontMono} {
		if *f != 0 {
			_, _, _ = procDeleteObject.Call(*f)
			*f = 0
		}
	}
}

func (w *Window) handleMessage(hwnd uintptr, msg uint32, wParam, lParam uintptr) (bool, uintptr) {
	switch msg {
	case wmEraseBkgnd:
		return true, 1 // double-buffered WM_PAINT draws full background

	case wmCtlColorEdit:
		if lParam == w.editHWND && w.editBrush != 0 {
			_, _, _ = procSetTextColor.Call(wParam, uintptr(colTextFinal))
			_, _, _ = procSetBkColor.Call(wParam, uintptr(colSurface))
			return true, w.editBrush
		}
		return false, 0

	case wmDpiChanged:
		newDPI := uint32(wParam & 0xFFFF)
		if newDPI >= 72 && newDPI != w.dpi {
			w.recreateFonts(newDPI)
			w.applySnapshot(hwnd)
		}
		return true, 0

	case wmTimer:
		if wParam == timerClockID && w.visible {
			// Only repaint the tiny clock rect so the edit box never flickers
			_, _, _ = procInvalidateRect.Call(hwnd, uintptr(unsafe.Pointer(&w.clockRect)), 0)
		}
		return true, 0

	case wmAppUpdate:
		w.applySnapshot(hwnd)
		return true, 0

	case wmAppEnterEdit:
		var initText string
		select {
		case initText = <-w.editInitCh:
		default:
		}
		w.enterEditOnUIThread(hwnd, initText)
		return true, 0

	case wmAppExitEdit:
		w.exitEditOnUIThread(hwnd)
		w.applySnapshot(hwnd)
		return true, 0

	case wmAppLevel:
		if w.visible {
			// Invalidate ONLY the audio level meter bar rectangle — never the edit box!
			_, _, _ = procInvalidateRect.Call(hwnd, uintptr(unsafe.Pointer(&w.meterRect)), 0)
		}
		return true, 0

	case wmAppHide:
		w.hideOnUIThread(hwnd)
		return true, 0

	case wmAppReadEdit:
		txt := w.getEditTextOnUIThread()
		select {
		case ch := <-w.readEditReq:
			ch <- txt
		default:
		}
		return true, 0

	case wmAppClose:
		w.hideOnUIThread(hwnd)
		_, _, _ = procDestroyWindow.Call(hwnd)
		return true, 0

	case wmDestroy:
		_, _, _ = procKillTimer.Call(hwnd, timerClockID)
		// PostQuitMessage(0)
		user32.NewProc("PostQuitMessage").Call(0)
		return true, 0

	case wmPaint:
		w.paint(hwnd)
		return true, 0
	}
	return false, 0
}

func (w *Window) setNoActivate(hwnd uintptr, noActivate bool) {
	exStyle, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlExStyle)
	var desired uintptr
	if noActivate {
		desired = exStyle | wsExNoActivate
	} else {
		desired = exStyle &^ wsExNoActivate
	}
	if desired != exStyle {
		_, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlExStyle, desired)
		_, _, _ = procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate|swpFrameChanged)
	}
}

func (w *Window) hideOnUIThread(hwnd uintptr) {
	_, _, _ = procKillTimer.Call(hwnd, timerClockID)
	w.exitEditOnUIThread(hwnd)
	_, _, _ = procShowWindow.Call(hwnd, swHide)
	w.mu.Lock()
	w.visible = false
	w.level = 0
	w.mu.Unlock()
}

func (w *Window) enterEditOnUIThread(hwnd uintptr, initialText string) {
	w.mu.Lock()
	alreadyEditing := w.editingUI
	w.snap.EditActive = true
	w.mu.Unlock()

	// Guard against overwriting live user edits if edit mode is already active!
	if alreadyEditing {
		w.applySnapshot(hwnd)
		return
	}

	w.setNoActivate(hwnd, false)
	if w.editHWND != 0 {
		crlf := strings.ReplaceAll(strings.ReplaceAll(initialText, "\r\n", "\n"), "\n", "\r\n")
		pTxt, _ := syscall.UTF16PtrFromString(crlf)
		_, _, _ = procSetWindowTextW.Call(w.editHWND, uintptr(unsafe.Pointer(pTxt)))
		_, _, _ = procShowWindow.Call(w.editHWND, swShow)
	}
	w.mu.Lock()
	w.editingUI = true
	w.editLayoutW = 0 // force MoveWindow layout inside applySnapshot
	w.mu.Unlock()

	w.applySnapshot(hwnd)

	_, _, _ = procSetForegroundWindow.Call(hwnd)
	if w.editHWND != 0 {
		_, _, _ = procSetFocus.Call(w.editHWND)
		endPos, _, _ := procGetWindowTextLengthW.Call(w.editHWND)
		_, _, _ = procSendMessageW.Call(w.editHWND, emSetSel, endPos, endPos)
		_, _, _ = procSendMessageW.Call(w.editHWND, emScrollCaret, 0, 0)
	}
}

func (w *Window) exitEditOnUIThread(hwnd uintptr) {
	if w.editHWND != 0 {
		_, _, _ = procShowWindow.Call(w.editHWND, swHide)
		empty, _ := syscall.UTF16PtrFromString("")
		_, _, _ = procSetWindowTextW.Call(w.editHWND, uintptr(unsafe.Pointer(empty)))
	}
	w.setNoActivate(hwnd, true)
	w.mu.Lock()
	w.editingUI = false
	w.editLayoutW = 0
	w.editLayoutD = 0
	w.mu.Unlock()
}

func (w *Window) getEditTextOnUIThread() string {
	if w.editHWND == 0 {
		return ""
	}
	n, _, _ := procGetWindowTextLengthW.Call(w.editHWND)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	_, _, _ = procGetWindowTextW.Call(w.editHWND, uintptr(unsafe.Pointer(&buf[0])), n+1)
	txt := syscall.UTF16ToString(buf)
	return strings.ReplaceAll(txt, "\r\n", "\n")
}

func (w *Window) getMonitorMetrics(targetHWND uintptr) (work rect, dpi uint32) {
	var hMon uintptr
	if targetHWND != 0 {
		hMon, _, _ = procMonitorFromWindow.Call(targetHWND, monitorDefaultToNearest)
	}
	if hMon == 0 {
		var pt point
		_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		// MonitorFromPoint takes POINT by value packed in uintptr on amd64: low 32=X, high 32=Y
		packed := uintptr(uint32(pt.X)) | (uintptr(uint32(pt.Y)) << 32)
		hMon, _, _ = procMonitorFromPoint.Call(packed, monitorDefaultToNearest)
	}
	dpi = 96
	if hMon != 0 {
		var mi monitorInfo
		mi.CbSize = uint32(unsafe.Sizeof(mi))
		if r, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); r != 0 {
			work = mi.RcWork
		}
		if procGetDpiForMonitor.Find() == nil {
			var dx, dy uint32
			hr, _, _ := procGetDpiForMonitor.Call(hMon, mdtEffectiveDPI, uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy)))
			if int32(hr) >= 0 && dx >= 72 {
				dpi = dx
			}
		}
	}
	if work.Right <= work.Left {
		work = rect{Left: 0, Top: 0, Right: 1920, Bottom: 1080}
	}
	return work, dpi
}

func (w *Window) applySnapshot(hwnd uintptr) {
	w.mu.Lock()
	snap := w.snap
	wasEditing := w.editingUI
	wasVisible := w.visible
	w.mu.Unlock()

	if snap.Kind == KindHidden {
		w.hideOnUIThread(hwnd)
		return
	}

	work, monDPI := w.getMonitorMetrics(snap.TargetHWND)
	if monDPI != w.dpi {
		w.recreateFonts(monDPI)
	}

	// Update Windows 11 border accent color to match state
	borderCol := borderColorForKind(snap.Kind)
	_, _, _ = procDwmSetWindowAttribute.Call(hwnd, dwmwaBorderColor, uintptr(unsafe.Pointer(&borderCol)), unsafe.Sizeof(borderCol))

	// Compute dynamic dimensions scaled to monitor DPI
	winW := w.dip(560)
	maxWorkW := (work.Right - work.Left) - w.dip(32)
	if winW > maxWorkW && maxWorkW > w.dip(280) {
		winW = maxWorkW
	}

	padX := w.dip(16)
	headerH := w.dip(36)
	footerH := w.dip(26)
	lineH := w.dip(23)
	textW := winW - padX*2 - w.dip(8)

	var bodyH int32
	isEditing := w.editingUI || snap.EditActive || snap.Kind == KindEditing

	// Apply semi-transparent window opacity (gently boosted +10% in Edit Mode for crisp caret readability)
	opPct := snap.OpacityPercent
	if opPct <= 0 {
		opPct = 82
	}
	if opPct < 40 {
		opPct = 40
	}
	if opPct > 100 {
		opPct = 100
	}
	if isEditing && opPct < 96 {
		opPct += 10
		if opPct > 96 {
			opPct = 96
		}
	}
	targetAlpha := byte(math.Round(float64(opPct) * 255.0 / 100.0))
	if targetAlpha != w.curAlpha {
		w.curAlpha = targetAlpha
		_, _, _ = procSetLayeredWindowAttrs.Call(hwnd, 0, uintptr(targetAlpha), lwaAlpha)
	}

	if isEditing {
		editH := w.dip(106)
		bodyH = editH + w.dip(8)
		if strings.TrimSpace(snap.PostEditFinal+snap.PostEditNonFinal) != "" {
			bodyH += lineH*2 + w.dip(10)
		} else {
			bodyH += lineH + w.dip(6)
		}
	} else {
		lines := w.wrapStyledText(snap.FinalText, snap.NonFinalText, textW)
		visLines := len(lines)
		if snap.ErrorBanner != "" && visLines == 0 {
			visLines = 1
		}
		if visLines < 1 {
			visLines = 1
		}
		if visLines > 5 {
			visLines = 5
		}
		bodyH = int32(visLines)*lineH + w.dip(10)
	}

	winH := headerH + bodyH + footerH + w.dip(8)
	x := work.Left + ((work.Right - work.Left) - winW)/2
	y := work.Top + w.dip(24)

	w.winWidth = winW
	w.winHeight = winH

	if isEditing {
		// Relayout editHWND whenever width or DPI changes WITHOUT overwriting user text!
		if w.editHWND != 0 && (w.editLayoutW != winW || w.editLayoutD != w.dpi) {
			editH := w.dip(106)
			_, _, _ = procMoveWindow.Call(w.editHWND, uintptr(padX), uintptr(headerH+w.dip(4)), uintptr(winW-padX*2), uintptr(editH), 1)
			w.editLayoutW = winW
			w.editLayoutD = w.dpi
		}
		_, _, _ = procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(winW), uintptr(winH), swpNoActivate|swpShowWindow)
		w.mu.Lock()
		w.visible = true
		w.mu.Unlock()
	} else {
		if wasEditing {
			w.exitEditOnUIThread(hwnd)
		}
		w.setNoActivate(hwnd, true)
		_, _, _ = procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(winW), uintptr(winH), swpNoActivate|swpShowWindow)
		if !wasVisible {
			_, _, _ = procShowWindow.Call(hwnd, swShowNoActivate)
		}
		w.mu.Lock()
		w.visible = true
		w.mu.Unlock()
	}

	if !wasVisible {
		_, _, _ = procSetTimer.Call(hwnd, timerClockID, 500, 0)
	}

	// Repaint non-edit area cleanly (WS_CLIPCHILDREN ensures the EDIT box never flickers!)
	_, _, _ = procInvalidateRect.Call(hwnd, 0, 0)
}

func borderColorForKind(k Kind) uint32 {
	switch k {
	case KindListening:
		return colTeal
	case KindConnecting, KindReconnecting, KindEditing:
		return colAmber
	case KindError:
		return colRed
	case KindNotice:
		return colTealBright
	default:
		return colBorderIdle
	}
}

func statusDotColor(k Kind) uint32 {
	switch k {
	case KindListening:
		return colCoral
	case KindConnecting, KindReconnecting, KindEditing:
		return colAmber
	case KindError:
		return colRed
	case KindNotice:
		return colTeal
	default:
		return colTeal
	}
}

func (w *Window) measureUTF16Width(hdc uintptr, u16 []uint16) int32 {
	if len(u16) == 0 {
		return 0
	}
	var sz sizeStruct
	_, _, _ = procGetTextExtentPoint32W.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)), uintptr(unsafe.Pointer(&sz)))
	return sz.CX
}

// wrapStyledText performs Unicode-safe (rune-based, never byte-sliced) word wrapping
// across final + non-final text using the real Win32 GDI font metrics.
func (w *Window) wrapStyledText(finalText, nonFinalText string, maxWidth int32) []styledLine {
	if finalText == "" && nonFinalText == "" {
		return nil
	}
	if maxWidth < 40 {
		maxWidth = 40
	}

	// Build a temporary measurement DC with fontMain
	hdc, _, _ := procCreateCompatibleDC.Call(0)
	if hdc == 0 {
		return []styledLine{{segs: []styledSegment{{text: finalText, draft: false}, {text: nonFinalText, draft: true}}}}
	}
	defer procDeleteDC.Call(hdc)
	oldFont, _, _ := procSelectObject.Call(hdc, w.fontMain)
	defer procSelectObject.Call(hdc, oldFont)

	type styledRune struct {
		r     rune
		draft bool
	}

	var all []styledRune
	for _, r := range finalText {
		all = append(all, styledRune{r: r, draft: false})
	}
	for _, r := range nonFinalText {
		all = append(all, styledRune{r: r, draft: true})
	}

	// Tokenize into words/spaces/newlines preserving draft flag per rune
	type wordRun struct {
		runes   []styledRune
		isSpace bool
		isNL    bool
	}
	var words []wordRun
	for i := 0; i < len(all); {
		r := all[i].r
		if r == '\r' {
			i++
			continue
		}
		if r == '\n' {
			words = append(words, wordRun{isNL: true})
			i++
			continue
		}
		if r == ' ' || r == '\t' {
			j := i + 1
			for j < len(all) && (all[j].r == ' ' || all[j].r == '\t') {
				j++
			}
			words = append(words, wordRun{runes: all[i:j], isSpace: true})
			i = j
			continue
		}
		j := i + 1
		for j < len(all) && all[j].r != ' ' && all[j].r != '\t' && all[j].r != '\n' && all[j].r != '\r' {
			j++
		}
		words = append(words, wordRun{runes: all[i:j]})
		i = j
	}

	measureRunes := func(rs []styledRune) int32 {
		if len(rs) == 0 {
			return 0
		}
		raw := make([]rune, len(rs))
		for i, sr := range rs {
			raw[i] = sr.r
		}
		return w.measureUTF16Width(hdc, utf16.Encode(raw))
	}

	var rawLines [][]styledRune
	var cur []styledRune
	var curW int32

	flushLine := func() {
		rawLines = append(rawLines, cur)
		cur = nil
		curW = 0
	}

	for _, wd := range words {
		if wd.isNL {
			flushLine()
			continue
		}
		if wd.isSpace && len(cur) == 0 {
			continue // don't start wrapped line with leading space
		}
		wdW := measureRunes(wd.runes)
		if curW > 0 && curW+wdW > maxWidth {
			flushLine()
			if wd.isSpace {
				continue
			}
		}
		// If a single very long token exceeds maxWidth, split safely on grapheme cluster boundaries
		// (keeping combining marks, variation selectors, and ZWJ emoji sequences intact!).
		if wdW > maxWidth {
			for i := 0; i < len(wd.runes); {
				end := i + 1
				for end < len(wd.runes) && isClusterContinuation(wd.runes[end-1].r, wd.runes[end].r) {
					end++
				}
				cluster := wd.runes[i:end]
				cw := measureRunes(cluster)
				if curW > 0 && curW+cw > maxWidth {
					flushLine()
				}
				cur = append(cur, cluster...)
				curW += cw
				i = end
			}
			continue
		}
		cur = append(cur, wd.runes...)
		curW += wdW
	}
	if len(cur) > 0 || len(rawLines) == 0 {
		flushLine()
	}

	// Merge adjacent runes with same draft flag into clean styledSegments
	lines := make([]styledLine, 0, len(rawLines))
	for _, rl := range rawLines {
		var segs []styledSegment
		var sb strings.Builder
		var curDraft bool
		for i, sr := range rl {
			if i == 0 {
				curDraft = sr.draft
			} else if sr.draft != curDraft {
				segs = append(segs, styledSegment{text: sb.String(), draft: curDraft})
				sb.Reset()
				curDraft = sr.draft
			}
			sb.WriteRune(sr.r)
		}
		if sb.Len() > 0 {
			segs = append(segs, styledSegment{text: sb.String(), draft: curDraft})
		}
		lines = append(lines, styledLine{segs: segs})
	}
	return lines
}

func (w *Window) drawTextUTF16(hdc uintptr, x, y int32, text string, colorRef uint32, font uintptr) int32 {
	if text == "" {
		return 0
	}
	u16 := utf16.Encode([]rune(text))
	if len(u16) == 0 {
		return 0
	}
	if font != 0 {
		_, _, _ = procSelectObject.Call(hdc, font)
	}
	_, _, _ = procSetTextColor.Call(hdc, uintptr(colorRef))
	_, _, _ = procTextOutW.Call(hdc, uintptr(x), uintptr(y), uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)))
	return w.measureUTF16Width(hdc, u16)
}

func (w *Window) fillRoundedRect(hdc uintptr, x1, y1, x2, y2, radius int32, fillCol uint32) {
	br, _, _ := procCreateSolidBrush.Call(uintptr(fillCol))
	pen, _, _ := procCreatePen.Call(psNull, 0, 0)
	oldBr, _, _ := procSelectObject.Call(hdc, br)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	_, _, _ = procRoundRect.Call(hdc, uintptr(x1), uintptr(y1), uintptr(x2), uintptr(y2), uintptr(radius), uintptr(radius))
	_, _, _ = procSelectObject.Call(hdc, oldBr)
	_, _, _ = procSelectObject.Call(hdc, oldPen)
	_, _, _ = procDeleteObject.Call(br)
	_, _, _ = procDeleteObject.Call(pen)
}

func isClusterContinuation(prev, cur rune) bool {
	if prev == 0x200D { // Zero-Width Joiner connects next emoji
		return true
	}
	if cur == 0x200D || (cur >= 0xFE00 && cur <= 0xFE0F) || (cur >= 0x1F3FB && cur <= 0x1F3FF) {
		return true
	}
	return unicode.Is(unicode.Mn, cur) || unicode.Is(unicode.Me, cur)
}

func rectInside(inner, outer rect) bool {
	return outer.Right > outer.Left && outer.Bottom > outer.Top &&
		inner.Left >= outer.Left && inner.Top >= outer.Top &&
		inner.Right <= outer.Right && inner.Bottom <= outer.Bottom
}

func (w *Window) drawMeterBar(hdc uintptr, left, top, right, h int32, level float64) {
	w.fillRoundedRect(hdc, left, top, right, top+h, h, colTrack)
	fillW := int32(math.Round(float64(right-left) * level))
	if fillW > w.dip(4) {
		meterCol := colTeal
		if level > 0.82 {
			meterCol = colAmber
		} else if level > 0.55 {
			meterCol = colTealBright
		}
		w.fillRoundedRect(hdc, left, top, left+fillW, top+h, h, meterCol)
	}
}

func (w *Window) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var rc rect
	_, _, _ = procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	width := rc.Right - rc.Left
	height := rc.Bottom - rc.Top
	if width <= 0 || height <= 0 {
		return
	}

	w.mu.Lock()
	snap := w.snap
	level := w.level
	isEditing := w.editingUI || snap.EditActive || snap.Kind == KindEditing
	w.mu.Unlock()

	// Fast path: 20ms level-meter updates repaint ONLY the small meter bar (zero text re-wrapping!)
	if rectInside(ps.RcPaint, w.meterRect) && w.meterRect.Right-w.meterRect.Left > 8 {
		mw := w.meterRect.Right - w.meterRect.Left
		mh := w.meterRect.Bottom - w.meterRect.Top
		memDC, _, _ := procCreateCompatibleDC.Call(hdc)
		memBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(mw), uintptr(mh))
		oldBmp, _, _ := procSelectObject.Call(memDC, memBmp)
		bgBrush, _, _ := procCreateSolidBrush.Call(uintptr(colBg))
		localRC := rect{0, 0, mw, mh}
		_, _, _ = procFillRect.Call(memDC, uintptr(unsafe.Pointer(&localRC)), bgBrush)
		_, _, _ = procDeleteObject.Call(bgBrush)
		meterH := w.dip(8)
		w.drawMeterBar(memDC, 2, 2, mw-2, meterH, level)
		_, _, _ = procBitBlt.Call(hdc, uintptr(w.meterRect.Left), uintptr(w.meterRect.Top), uintptr(mw), uintptr(mh), memDC, 0, 0, srcCopy)
		_, _, _ = procSelectObject.Call(memDC, oldBmp)
		_, _, _ = procDeleteObject.Call(memBmp)
		_, _, _ = procDeleteDC.Call(memDC)
		return
	}

	// Double-buffer into offscreen bitmap for zero flicker
	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	memBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(width), uintptr(height))
	oldBmp, _, _ := procSelectObject.Call(memDC, memBmp)
	defer func() {
		_, _, _ = procSelectObject.Call(memDC, oldBmp)
		_, _, _ = procDeleteObject.Call(memBmp)
		_, _, _ = procDeleteDC.Call(memDC)
	}()

	// Fill card background
	bgBrush, _, _ := procCreateSolidBrush.Call(uintptr(colBg))
	_, _, _ = procFillRect.Call(memDC, uintptr(unsafe.Pointer(&rc)), bgBrush)
	_, _, _ = procDeleteObject.Call(bgBrush)
	_, _, _ = procSetBkMode.Call(memDC, transparentBk)

	padX := w.dip(16)
	headerH := w.dip(36)
	lineH := w.dip(23)

	// --- Header: status dot + status label + audio level meter + elapsed clock ---
	dotSize := w.dip(9)
	dotX := padX
	dotY := (headerH - dotSize) / 2
	dotBr, _, _ := procCreateSolidBrush.Call(uintptr(statusDotColor(snap.Kind)))
	nullPen, _, _ := procCreatePen.Call(psNull, 0, 0)
	oldBr, _, _ := procSelectObject.Call(memDC, dotBr)
	oldPen, _, _ := procSelectObject.Call(memDC, nullPen)
	_, _, _ = procEllipse.Call(memDC, uintptr(dotX), uintptr(dotY), uintptr(dotX+dotSize), uintptr(dotY+dotSize))
	_, _, _ = procSelectObject.Call(memDC, oldBr)
	_, _, _ = procSelectObject.Call(memDC, oldPen)
	_, _, _ = procDeleteObject.Call(dotBr)
	_, _, _ = procDeleteObject.Call(nullPen)

	statusLabel := snap.StatusText
	if statusLabel == "" {
		statusLabel = defaultStatusLabel(snap.Kind)
	}
	labelW := w.drawTextUTF16(memDC, dotX+dotSize+w.dip(8), w.dip(9), statusLabel, colTextFinal, w.fontBold)

	// Elapsed time at top-right (when actively recording/editing/connecting)
	clockW := w.dip(44)
	clockX := width - padX - clockW
	w.clockRect = rect{Left: clockX - w.dip(4), Top: w.dip(4), Right: width - padX + w.dip(4), Bottom: headerH}
	if snap.Kind == KindConnecting || snap.Kind == KindListening || snap.Kind == KindEditing || snap.Kind == KindReconnecting {
		var secs int
		if !snap.StartTime.IsZero() {
			secs = int(time.Since(snap.StartTime).Seconds())
		}
		if secs < 0 {
			secs = 0
		}
		clockStr := fmt.Sprintf("%d:%02d", secs/60, secs%60)
		w.drawTextUTF16(memDC, clockX, w.dip(10), clockStr, colTextHint, w.fontMono)
	}

	// Audio level meter bar between status label and elapsed clock
	meterLeft := dotX + dotSize + w.dip(18) + labelW
	meterRight := clockX - w.dip(14)
	meterH := w.dip(8)
	meterTop := (headerH - meterH) / 2
	if meterRight-meterLeft >= w.dip(50) && (snap.Kind == KindConnecting || snap.Kind == KindListening || snap.Kind == KindEditing || snap.Kind == KindReconnecting) {
		w.meterRect = rect{Left: meterLeft - 2, Top: meterTop - 2, Right: meterRight + 2, Bottom: meterTop + meterH + 2}
		w.drawMeterBar(memDC, meterLeft, meterTop, meterRight, meterH, level)
	}

	// --- Body ---
	if isEditing {
		// Edit box occupies [headerH+4 .. headerH+4+editH]; draw subtle border & live continuation below it!
		editH := w.dip(106)
		contTop := headerH + editH + w.dip(10)
		postFinal := strings.TrimSpace(snap.PostEditFinal)
		postDraft := strings.TrimSpace(snap.PostEditNonFinal)
		if postFinal == "" && postDraft == "" {
			w.drawTextUTF16(memDC, padX, contTop, "Mic stays live — keep speaking and new words will append here…", colTextHint, w.fontSmall)
		} else {
			prefixW := w.drawTextUTF16(memDC, padX, contTop, "+ ", colTeal, w.fontBold)
			availW := width - padX*2 - prefixW
			cLines := w.wrapStyledText(snap.PostEditFinal, snap.PostEditNonFinal, availW)
			// Show up to the latest 2 lines of live continuation
			startIdx := 0
			if len(cLines) > 2 {
				startIdx = len(cLines) - 2
			}
			y := contTop
			for i := startIdx; i < len(cLines); i++ {
				x := padX
				if i == startIdx {
					x += prefixW
				}
				for _, seg := range cLines[i].segs {
					col := colTealBright
					if seg.draft {
						col = colTextDraft
					}
					x += w.drawTextUTF16(memDC, x, y, seg.text, col, w.fontMain)
				}
				y += lineH
			}
		}
	} else {
		bodyTop := headerH + w.dip(4)
		textW := width - padX*2 - w.dip(8)

		if snap.ErrorBanner != "" && snap.FinalText == "" && snap.NonFinalText == "" {
			errCol := colRed
			if snap.Kind == KindNotice {
				errCol = colTealBright
			}
			w.drawTextUTF16(memDC, padX, bodyTop, snap.ErrorBanner, errCol, w.fontMain)
		} else if snap.FinalText == "" && snap.NonFinalText == "" {
			placeholder := "Speak now…"
			if snap.ClearedOnce {
				placeholder = "Cleared — speak again, or press Esc once more to close…"
			} else if snap.Kind == KindConnecting {
				placeholder = "Listening immediately (connecting to Soniox)…"
			} else if snap.Kind == KindReconnecting {
				placeholder = "Reconnecting to Soniox — audio is buffered…"
			}
			w.drawTextUTF16(memDC, padX, bodyTop, placeholder, colTextDraft, w.fontMain)
		} else {
			lines := w.wrapStyledText(snap.FinalText, snap.NonFinalText, textW)
			totalLines := len(lines)
			startIdx := 0
			if totalLines > 5 {
				startIdx = totalLines - 5 // Auto-scroll to ALWAYS show latest 5 lines!
			}
			y := bodyTop
			for i := startIdx; i < totalLines; i++ {
				x := padX
				for _, seg := range lines[i].segs {
					col := colTextFinal
					if seg.draft {
						col = colTextDraft
					}
					x += w.drawTextUTF16(memDC, x, y, seg.text, col, w.fontMain)
				}
				y += lineH
			}
			// Draw subtle scrollbar indicator on right when scrolled past 5 lines
			if totalLines > 5 {
				barX := width - w.dip(8)
				barTop := bodyTop
				barH := 5 * lineH
				w.fillRoundedRect(memDC, barX, barTop, barX+w.dip(3), barTop+barH, w.dip(3), colTrack)
				thumbH := int32(math.Max(float64(w.dip(16)), float64(barH)*5.0/float64(totalLines)))
				w.fillRoundedRect(memDC, barX, barTop+barH-thumbH, barX+w.dip(3), barTop+barH, w.dip(3), colTeal)
			}
		}
	}

	// --- Footer hint bar ---
	hk := snap.HotkeyLabel
	if hk == "" {
		hk = "F9"
	}
	var hint string
	switch snap.Kind {
	case KindEditing:
		hint = fmt.Sprintf("%s / Enter insert all  ·  Shift+Enter newline  ·  Esc clear (×2 close)", hk)
	case KindError:
		hint = snap.ErrorBanner
		if hint == "" {
			hint = fmt.Sprintf("Press %s to retry  ·  Esc to dismiss", hk)
		}
	case KindNotice:
		hint = "Saved in Recent Transcripts tray menu"
	case KindReconnecting:
		if snap.ClearedOnce {
			hint = fmt.Sprintf("Cleared — speak again  ·  Esc close  ·  %s / Enter insert", hk)
		} else {
			hint = fmt.Sprintf("Reconnecting (keeping text)  ·  %s / Enter insert now", hk)
		}
	default:
		if snap.ClearedOnce {
			hint = fmt.Sprintf("Cleared — speak again  ·  Esc close  ·  %s / Enter insert", hk)
		} else {
			hint = fmt.Sprintf("%s / Enter insert  ·  Esc edit  ·  Esc×2 clear  ·  Esc×3 close", hk)
		}
	}
	footerY := height - w.dip(22)
	w.drawTextUTF16(memDC, padX, footerY, hint, colTextHint, w.fontSmall)

	_, _, _ = procBitBlt.Call(hdc, 0, 0, uintptr(width), uintptr(height), memDC, 0, 0, srcCopy)
}

func defaultStatusLabel(k Kind) string {
	switch k {
	case KindConnecting:
		return "Connecting"
	case KindListening:
		return "Listening"
	case KindEditing:
		return "Edit Mode (Mic Live)"
	case KindReconnecting:
		return "Reconnecting…"
	case KindError:
		return "Error"
	case KindNotice:
		return "VoiceType"
	default:
		return "VoiceType"
	}
}
