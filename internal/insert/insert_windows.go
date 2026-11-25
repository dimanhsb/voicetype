//go:build windows

package insert

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"voicetype/internal/config"
	"voicetype/internal/logging"
)

var pasteOpGen atomic.Uint64

// Status describes the outcome of inserting text into the target application.
type Status int

const (
	StatusOK Status = iota
	StatusElevatedCopied // Target window is elevated (UIPI blocked); text left on clipboard
	StatusFailedCopied   // Insertion failed; text left on clipboard
	StatusFailed         // Insertion and clipboard fallback both failed
)

// Result reports what happened during insertion.
type Result struct {
	Status  Status
	Message string
	Err     error
}

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	advapi32                     = windows.NewLazySystemDLL("advapi32.dll")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procIsWindow                 = user32.NewProc("IsWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procPeekMessageW             = user32.NewProc("PeekMessageW")
	procSendInput                = user32.NewProc("SendInput")
	procGetAsyncKeyState         = user32.NewProc("GetAsyncKeyState")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procOpenClipboard            = user32.NewProc("OpenClipboard")
	procCloseClipboard           = user32.NewProc("CloseClipboard")
	procEmptyClipboard           = user32.NewProc("EmptyClipboard")
	procGetClipboardData         = user32.NewProc("GetClipboardData")
	procSetClipboardData         = user32.NewProc("SetClipboardData")
	procEnumClipboardFormats     = user32.NewProc("EnumClipboardFormats")
	procGetClipboardFormatNameW  = user32.NewProc("GetClipboardFormatNameW")
	procRegisterClipboardFormatW = user32.NewProc("RegisterClipboardFormatW")
	procGetClipboardSequenceNum  = user32.NewProc("GetClipboardSequenceNumber")

	procGlobalAlloc        = kernel32.NewProc("GlobalAlloc")
	procGlobalFree         = kernel32.NewProc("GlobalFree")
	procGlobalLock         = kernel32.NewProc("GlobalLock")
	procGlobalUnlock       = kernel32.NewProc("GlobalUnlock")
	procGlobalSize         = kernel32.NewProc("GlobalSize")
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")

	procGetSidSubAuthorityCount = advapi32.NewProc("GetSidSubAuthorityCount")
	procGetSidSubAuthority      = advapi32.NewProc("GetSidSubAuthority")
)

const (
	cfText        = 1
	cfBitmap      = 2
	cfMetafile    = 3
	cfOEMText     = 7
	cfDIB         = 8
	cfPalette     = 9
	cfUnicodeText = 13
	cfEnhMetafile = 14
	cfHDrop       = 15
	cfLocale      = 16
	cfDIBV5       = 17

	gmemMoveable = 0x0002

	hwndMessage = ^uintptr(2) // (HWND)-3

	inputKeyboard    = 1
	keyeventfKeyUp   = 0x0002
	keyeventfUnicode = 0x0004

	vkControl  = 0x11
	vkLControl = 0xA2
	vkRControl = 0xA3
	vkShift    = 0x10
	vkLShift   = 0xA0
	vkRShift   = 0xA1
	vkMenu     = 0x12
	vkLMenu    = 0xA4
	vkRMenu    = 0xA5
	vkLWin     = 0x5B
	vkRWin     = 0x5C
	vkReturn   = 0x0D
	vkV        = 0x56
)

type keybdInput struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// inputEvent matches Win32 INPUT on amd64 (40 bytes total: 4-byte type + 4-byte pad + 32-byte union).
type inputEvent struct {
	Type uint32
	_    uint32
	KI   keybdInput
	_    [8]byte
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

// GetForegroundWindow returns the currently focused top-level window HWND.
func GetForegroundWindow() uintptr {
	h, _, _ := procGetForegroundWindow.Call()
	return h
}

// IsWindowValid checks whether hwnd still refers to a live window.
func IsWindowValid(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procIsWindow.Call(hwnd)
	return r != 0
}

func getProcessIntegrityRID(procHandle windows.Handle) (uint32, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(procHandle, windows.TOKEN_QUERY, &token); err != nil {
		return 0, err
	}
	defer token.Close()

	var needed uint32
	_ = windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &needed)
	if needed == 0 {
		return 0, fmt.Errorf("TokenIntegrityLevel size=0")
	}
	buf := make([]byte, needed)
	if err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buf[0], needed, &needed); err != nil {
		return 0, err
	}
	tml := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0]))
	if tml.Label.Sid == nil {
		return 0, fmt.Errorf("nil integrity SID")
	}
	cntPtr, _, _ := procGetSidSubAuthorityCount.Call(uintptr(unsafe.Pointer(tml.Label.Sid)))
	if cntPtr == 0 {
		return 0, fmt.Errorf("GetSidSubAuthorityCount failed")
	}
	cnt := *(*byte)(unsafe.Pointer(cntPtr))
	if cnt == 0 {
		return 0, fmt.Errorf("empty integrity SID")
	}
	ridPtr, _, _ := procGetSidSubAuthority.Call(uintptr(unsafe.Pointer(tml.Label.Sid)), uintptr(cnt-1))
	if ridPtr == 0 {
		return 0, fmt.Errorf("GetSidSubAuthority failed")
	}
	return *(*uint32)(unsafe.Pointer(ridPtr)), nil
}

func isProcessElevated(procHandle windows.Handle) (bool, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(procHandle, windows.TOKEN_QUERY, &token); err != nil {
		return false, err
	}
	defer token.Close()

	var elevation uint32
	var outLen uint32
	err := windows.GetTokenInformation(
		token,
		windows.TokenElevation,
		(*byte)(unsafe.Pointer(&elevation)),
		uint32(unsafe.Sizeof(elevation)),
		&outLen,
	)
	if err != nil {
		return false, err
	}
	return elevation != 0, nil
}

// IsTargetBlockedByUIPI reports true if targetHWND runs at a higher Mandatory Integrity Level
// or elevated token compared to VoiceType (meaning Windows UIPI will drop SendInput).
func IsTargetBlockedByUIPI(targetHWND uintptr) bool {
	if !IsWindowValid(targetHWND) {
		return false
	}

	var pid uint32
	_, _, _ = procGetWindowThreadProcessId.Call(targetHWND, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 || pid == uint32(windows.GetCurrentProcessId()) {
		return false
	}

	selfRID, selfRIDErr := getProcessIntegrityRID(windows.CurrentProcess())
	selfElev, _ := isProcessElevated(windows.CurrentProcess())

	hProc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED && !selfElev
	}
	defer windows.CloseHandle(hProc)

	if targetRID, err := getProcessIntegrityRID(hProc); err == nil && selfRIDErr == nil {
		if targetRID > selfRID {
			return true
		}
	} else if err == windows.ERROR_ACCESS_DENIED && !selfElev {
		return true
	}

	if !selfElev {
		if targetElev, err := isProcessElevated(hProc); err == nil && targetElev {
			return true
		} else if err == windows.ERROR_ACCESS_DENIED {
			return true
		}
	}
	return false
}

// RestoreFocus brings targetHWND back to the foreground on a locked OS thread with an
// initialized message queue so AttachThreadInput succeeds reliably.
func RestoreFocus(targetHWND uintptr) bool {
	if !IsWindowValid(targetHWND) {
		return false
	}
	if GetForegroundWindow() == targetHWND {
		return true
	}

	// Ensure calling thread has a Win32 message queue before AttachThreadInput
	var dummy winMsg
	_, _, _ = procPeekMessageW.Call(uintptr(unsafe.Pointer(&dummy)), 0, 0, 0, 0)

	curTID, _, _ := procGetCurrentThreadID.Call()
	var fgPID uint32
	fgHWND := GetForegroundWindow()
	fgTID, _, _ := procGetWindowThreadProcessId.Call(fgHWND, uintptr(unsafe.Pointer(&fgPID)))
	var targetPID uint32
	targetTID, _, _ := procGetWindowThreadProcessId.Call(targetHWND, uintptr(unsafe.Pointer(&targetPID)))

	attachedFg := false
	attachedTarget := false
	if fgTID != 0 && fgTID != curTID {
		r, _, _ := procAttachThreadInput.Call(curTID, fgTID, 1)
		attachedFg = r != 0
	}
	if targetTID != 0 && targetTID != curTID && targetTID != fgTID {
		r, _, _ := procAttachThreadInput.Call(curTID, targetTID, 1)
		attachedTarget = r != 0
	}

	_, _, _ = procBringWindowToTop.Call(targetHWND)
	_, _, _ = procSetForegroundWindow.Call(targetHWND)

	if attachedTarget {
		_, _, _ = procAttachThreadInput.Call(curTID, targetTID, 0)
	}
	if attachedFg {
		_, _, _ = procAttachThreadInput.Call(curTID, fgTID, 0)
	}

	deadline := time.Now().Add(110 * time.Millisecond)
	for time.Now().Before(deadline) {
		if GetForegroundWindow() == targetHWND {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return GetForegroundWindow() == targetHWND
}

// --- Win32-contract-compliant Clipboard helpers (app-owned HWND on locked OS thread) ---

type clipboardItem struct {
	format uint32
	data   []byte
}

func createClipboardOwnerWindow() uintptr {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	cls, _ := syscall.UTF16PtrFromString("STATIC")
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		0,
		0,
		0, 0, 0, 0,
		hwndMessage, 0, hInst, 0,
	)
	return hwnd
}

func openClipboardRetry(ownerHWND uintptr) bool {
	for i := 0; i < 15; i++ {
		r, _, _ := procOpenClipboard.Call(ownerHWND)
		if r != 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func allocUnicodeGlobal(text string) (uintptr, error) {
	u16 := utf16.Encode([]rune(text))
	u16 = append(u16, 0)
	byteLen := uintptr(len(u16) * 2)

	hMem, _, err := procGlobalAlloc.Call(gmemMoveable, byteLen)
	if hMem == 0 {
		return 0, fmt.Errorf("GlobalAlloc failed: %v", err)
	}
	ptr, _, err := procGlobalLock.Call(hMem)
	if ptr == 0 {
		_, _, _ = procGlobalFree.Call(hMem)
		return 0, fmt.Errorf("GlobalLock failed: %v", err)
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(u16)), u16)
	_, _, _ = procGlobalUnlock.Call(hMem)
	return hMem, nil
}

// SetClipboardText places text onto the Windows clipboard as CF_UNICODETEXT using an app-owned HWND.
func SetClipboardText(text string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hMem, err := allocUnicodeGlobal(text)
	if err != nil {
		return err
	}

	owner := createClipboardOwnerWindow()
	if owner == 0 {
		_, _, _ = procGlobalFree.Call(hMem)
		return fmt.Errorf("CreateWindowExW(HWND_MESSAGE) failed")
	}
	defer procDestroyWindow.Call(owner)

	if !openClipboardRetry(owner) {
		_, _, _ = procGlobalFree.Call(hMem)
		return fmt.Errorf("OpenClipboard failed")
	}
	defer procCloseClipboard.Call()

	if r, _, err := procEmptyClipboard.Call(); r == 0 {
		_, _, _ = procGlobalFree.Call(hMem)
		return fmt.Errorf("EmptyClipboard failed: %v", err)
	}
	if r, _, err := procSetClipboardData.Call(cfUnicodeText, hMem); r == 0 {
		_, _, _ = procGlobalFree.Call(hMem)
		return fmt.Errorf("SetClipboardData failed: %v", err)
	}
	// Ownership transferred to Windows.
	return nil
}

func isSafeHGlobalFormat(fmtID uint32) bool {
	switch fmtID {
	case cfUnicodeText, cfText, cfOEMText, cfLocale, cfHDrop:
		return true
	}
	// Preserve registered HTML / Rich Text HGLOBAL formats
	if fmtID >= 0xC000 && fmtID <= 0xFFFF {
		var nameBuf [64]uint16
		n, _, _ := procGetClipboardFormatNameW.Call(uintptr(fmtID), uintptr(unsafe.Pointer(&nameBuf[0])), uintptr(len(nameBuf)))
		if n > 0 {
			name := syscall.UTF16ToString(nameBuf[:n])
			switch name {
			case "HTML Format", "Rich Text Format", "Rich Text Format Without Objects":
				return true
			}
		}
	}
	return false
}

// isNonRestorableRichPayload identifies genuine image / GDI / OLE payloads (like screenshots
// or Excel cells) that should NOT be wiped by EmptyClipboard. Harmless metadata tags added by
// Chrome, VS Code, or Windows Clipboard History alongside normal text do NOT trigger fallback!
func isNonRestorableRichPayload(fmtID uint32) bool {
	switch fmtID {
	case cfBitmap, cfDIB, cfDIBV5, cfMetafile, cfEnhMetafile, cfPalette:
		return true
	}
	if fmtID >= 0xC000 && fmtID <= 0xFFFF {
		var nameBuf [64]uint16
		n, _, _ := procGetClipboardFormatNameW.Call(uintptr(fmtID), uintptr(unsafe.Pointer(&nameBuf[0])), uintptr(len(nameBuf)))
		if n > 0 {
			name := syscall.UTF16ToString(nameBuf[:n])
			switch name {
			case "PNG", "JFIF", "GIF", "image/png", "image/jpeg", "Biff8", "Biff12", "XML Spreadsheet", "Embed Source", "Object Descriptor":
				return true
			}
		}
	}
	return false
}

// markExcludeFromClipboardHistory marks a temporary VoiceType paste with Microsoft's
// official Windows 11 clipboard flags so it does not clutter Win+V Clipboard History or Cloud Sync.
func markExcludeFromClipboardHistoryLocked() {
	excludeFormats := []string{
		"ExcludeClipboardContentFromMonitorProcessing",
		"CanIncludeInClipboardHistory",
		"CanUploadToCloudClipboard",
	}
	for _, name := range excludeFormats {
		ptr, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			continue
		}
		fmtID, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(ptr)))
		if fmtID == 0 {
			continue
		}
		hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, 4)
		if hMem == 0 {
			continue
		}
		p, _, _ := procGlobalLock.Call(hMem)
		if p == 0 {
			_, _, _ = procGlobalFree.Call(hMem)
			continue
		}
		*(*uint32)(unsafe.Pointer(p)) = 0
		_, _, _ = procGlobalUnlock.Call(hMem)
		if r, _, _ := procSetClipboardData.Call(fmtID, hMem); r == 0 {
			_, _, _ = procGlobalFree.Call(hMem)
		}
	}
}

// snapshotClipboardLocked captures restorable HGLOBAL formats (text, HTML, RTF, file drops)
// and reports canRestore=true unless the clipboard holds a genuine image/screenshot/OLE payload
// or data larger than 4 MiB (in which case we preserve the clipboard intact via SendInput).
func snapshotClipboardLocked() (items []clipboardItem, canRestore bool) {
	const maxTotalBytes = 4 * 1024 * 1024
	var totalBytes int
	canRestore = true

	var fmtID uint32
	for {
		next, _, _ := procEnumClipboardFormats.Call(uintptr(fmtID))
		fmtID = uint32(next)
		if fmtID == 0 {
			break
		}
		if isNonRestorableRichPayload(fmtID) {
			canRestore = false
			continue
		}
		if !isSafeHGlobalFormat(fmtID) {
			// Harmless browser/editor metadata tags alongside text (e.g. vscode-editor-data, Chromium tokens)
			continue
		}
		hData, _, _ := procGetClipboardData.Call(uintptr(fmtID))
		if hData == 0 {
			canRestore = false
			continue
		}
		sz, _, _ := procGlobalSize.Call(hData)
		if sz == 0 || totalBytes+int(sz) > maxTotalBytes {
			canRestore = false
			continue
		}
		ptr, _, _ := procGlobalLock.Call(hData)
		if ptr == 0 {
			canRestore = false
			continue
		}
		buf := make([]byte, sz)
		copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(ptr)), sz))
		_, _, _ = procGlobalUnlock.Call(hData)
		items = append(items, clipboardItem{format: fmtID, data: buf})
		totalBytes += int(sz)
	}
	return items, canRestore
}

func restoreClipboard(items []clipboardItem, expectedSeq uint32, opGen uint64) {
	if len(items) == 0 {
		return
	}
	if opGen != 0 && pasteOpGen.Load() != opGen {
		// Another VoiceType paste started; do not overwrite it with an older snapshot!
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	owner := createClipboardOwnerWindow()
	if owner == 0 {
		return
	}
	defer procDestroyWindow.Call(owner)

	if !openClipboardRetry(owner) {
		return
	}
	defer procCloseClipboard.Call()

	if opGen != 0 && pasteOpGen.Load() != opGen {
		return
	}
	// Check sequence number WHILE holding the clipboard open — eliminates TOCTOU race!
	seqNow, _, _ := procGetClipboardSequenceNum.Call()
	if uint32(seqNow) != expectedSeq {
		return
	}

	if r, _, _ := procEmptyClipboard.Call(); r == 0 {
		return
	}
	for _, item := range items {
		if len(item.data) == 0 {
			continue
		}
		hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(len(item.data)))
		if hMem == 0 {
			continue
		}
		ptr, _, _ := procGlobalLock.Call(hMem)
		if ptr == 0 {
			_, _, _ = procGlobalFree.Call(hMem)
			continue
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(item.data)), item.data)
		_, _, _ = procGlobalUnlock.Call(hMem)
		if r, _, _ := procSetClipboardData.Call(uintptr(item.format), hMem); r == 0 {
			_, _, _ = procGlobalFree.Call(hMem)
		}
	}
}

func sendInputs(events []inputEvent) error {
	if len(events) == 0 {
		return nil
	}
	n, _, err := procSendInput.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		unsafe.Sizeof(events[0]),
	)
	if int(n) != len(events) {
		return fmt.Errorf("SendInput partial/failed (%d/%d): %v", n, len(events), err)
	}
	return nil
}

func releaseStuckModifiers() {
	mods := []uint16{vkLControl, vkRControl, vkLShift, vkRShift, vkLMenu, vkRMenu, vkLWin, vkRWin}
	var ups []inputEvent
	for _, vk := range mods {
		st, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
		if (uint16(st) & 0x8000) != 0 {
			ups = append(ups, inputEvent{
				Type: inputKeyboard,
				KI:   keybdInput{WVk: vk, DwFlags: keyeventfKeyUp},
			})
		}
	}
	if len(ups) > 0 {
		_ = sendInputs(ups)
		time.Sleep(15 * time.Millisecond)
	}
}

func sendCtrlV() error {
	events := []inputEvent{
		{Type: inputKeyboard, KI: keybdInput{WVk: vkControl}},
		{Type: inputKeyboard, KI: keybdInput{WVk: vkV}},
		{Type: inputKeyboard, KI: keybdInput{WVk: vkV, DwFlags: keyeventfKeyUp}},
		{Type: inputKeyboard, KI: keybdInput{WVk: vkControl, DwFlags: keyeventfKeyUp}},
	}
	return sendInputs(events)
}

// typeUnicode sends text via KEYEVENTF_UNICODE in small paced batches, keeping surrogate pairs
// together, re-verifying focus before each batch, and using Shift+Enter for newlines so chat
// apps do not prematurely submit messages.
func typeUnicode(targetHWND uintptr, text string) error {
	const batchRunes = 16
	var batch []inputEvent
	runesInBatch := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if targetHWND != 0 && GetForegroundWindow() != targetHWND {
			return fmt.Errorf("target window lost focus during typing")
		}
		err := sendInputs(batch)
		batch = batch[:0]
		runesInBatch = 0
		return err
	}

	for _, r := range text {
		if r == '\r' {
			continue
		}
		if r == '\n' {
			if err := flush(); err != nil {
				return err
			}
			if targetHWND != 0 && GetForegroundWindow() != targetHWND {
				return fmt.Errorf("target window lost focus during typing")
			}
			// Shift+Enter inserts a soft newline in chat apps, browsers, and editors cleanly
			ev := []inputEvent{
				{Type: inputKeyboard, KI: keybdInput{WVk: vkShift}},
				{Type: inputKeyboard, KI: keybdInput{WVk: vkReturn}},
				{Type: inputKeyboard, KI: keybdInput{WVk: vkReturn, DwFlags: keyeventfKeyUp}},
				{Type: inputKeyboard, KI: keybdInput{WVk: vkShift, DwFlags: keyeventfKeyUp}},
			}
			if err := sendInputs(ev); err != nil {
				return err
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}

		// Encode complete Unicode scalar (1 or 2 UTF-16 code units; surrogate pairs stay in the same batch!)
		r1, r2 := utf16.EncodeRune(r)
		if r1 == '\uFFFD' && r2 == '\uFFFD' {
			u := uint16(r)
			batch = append(batch,
				inputEvent{Type: inputKeyboard, KI: keybdInput{WScan: u, DwFlags: keyeventfUnicode}},
				inputEvent{Type: inputKeyboard, KI: keybdInput{WScan: u, DwFlags: keyeventfUnicode | keyeventfKeyUp}},
			)
		} else {
			batch = append(batch,
				inputEvent{Type: inputKeyboard, KI: keybdInput{WScan: uint16(r1), DwFlags: keyeventfUnicode}},
				inputEvent{Type: inputKeyboard, KI: keybdInput{WScan: uint16(r1), DwFlags: keyeventfUnicode | keyeventfKeyUp}},
				inputEvent{Type: inputKeyboard, KI: keybdInput{WScan: uint16(r2), DwFlags: keyeventfUnicode}},
				inputEvent{Type: inputKeyboard, KI: keybdInput{WScan: uint16(r2), DwFlags: keyeventfUnicode | keyeventfKeyUp}},
			)
		}
		runesInBatch++
		if runesInBatch >= batchRunes {
			if err := flush(); err != nil {
				return err
			}
			time.Sleep(4 * time.Millisecond)
		}
	}
	return flush()
}

func fallbackCopy(text, okMsg, failMsg string, cause error, elevated bool) Result {
	if err := SetClipboardText(text); err != nil {
		logging.Errorf("insert: fallback SetClipboardText failed: %v", err)
		return Result{Status: StatusFailed, Message: failMsg, Err: err}
	}
	if elevated {
		return Result{Status: StatusElevatedCopied, Message: okMsg, Err: cause}
	}
	return Result{Status: StatusFailedCopied, Message: okMsg, Err: cause}
}

// Insert restores focus to targetHWND and inserts text using the configured method
// ("paste" default with Win32-safe clipboard snapshot/restore, or "sendinput" Unicode typing).
// Never blasts input into the wrong window; if target is elevated or lost focus, copies to clipboard.
func Insert(targetHWND uintptr, text string, method string) Result {
	if text == "" {
		return Result{Status: StatusOK}
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 1. Verify target window exists
	if targetHWND == 0 || !IsWindowValid(targetHWND) {
		logging.Warnf("insert: no valid target window (0x%X); copying to clipboard", targetHWND)
		return fallbackCopy(text, "Target window closed — copied to clipboard (Ctrl+V)", "Target window closed (saved in History)", nil, false)
	}

	// 2. Check UIPI elevation / integrity level before attempting synthetic input
	if IsTargetBlockedByUIPI(targetHWND) {
		logging.Warnf("insert: target window 0x%X is elevated (UIPI); copying text to clipboard", targetHWND)
		return fallbackCopy(text, "Admin window — copied to clipboard (press Ctrl+V)", "Admin window — saved in History (clipboard busy)", nil, true)
	}

	// 3. Restore focus and confirm the exact target window is foreground
	if !RestoreFocus(targetHWND) {
		logging.Warnf("insert: could not restore focus to 0x%X; refusing to type into wrong app", targetHWND)
		return fallbackCopy(text, "Target window lost focus — copied to clipboard (Ctrl+V)", "Target window lost focus (saved in History)", nil, false)
	}
	time.Sleep(20 * time.Millisecond)
	if GetForegroundWindow() != targetHWND {
		logging.Warnf("insert: focus shifted away from 0x%X before injection", targetHWND)
		return fallbackCopy(text, "Focus changed — copied to clipboard (Ctrl+V)", "Focus changed (saved in History)", nil, false)
	}

	releaseStuckModifiers()

	// 4. Execute insertion method
	if method == config.InsertMethodSendInput {
		if err := typeUnicode(targetHWND, text); err != nil {
			logging.Errorf("insert: SendInput Unicode failed (%v); copying to clipboard", err)
			return fallbackCopy(text, "Could not type into app — copied to clipboard (Ctrl+V)", "Typing failed (saved in History)", err, false)
		}
		logging.Infof("insert: typed %d runes via SendInput", len([]rune(text)))
		return Result{Status: StatusOK}
	}

	// Default: Clipboard Paste (prepare HGLOBAL -> open owned HWND -> snapshot -> empty -> set -> seq -> close -> Ctrl+V -> restore)
	hMem, err := allocUnicodeGlobal(text)
	if err != nil {
		logging.Errorf("insert: allocUnicodeGlobal failed (%v); trying SendInput", err)
		if err2 := typeUnicode(targetHWND, text); err2 != nil {
			return Result{Status: StatusFailed, Message: "Insertion failed (saved in History)", Err: err2}
		}
		return Result{Status: StatusOK}
	}

	owner := createClipboardOwnerWindow()
	if owner == 0 || !openClipboardRetry(owner) {
		if owner != 0 {
			_, _, _ = procDestroyWindow.Call(owner)
		}
		_, _, _ = procGlobalFree.Call(hMem)
		logging.Warnf("insert: clipboard busy; falling back to SendInput Unicode")
		if err2 := typeUnicode(targetHWND, text); err2 != nil {
			return Result{Status: StatusFailed, Message: "Clipboard busy & typing failed (saved in History)", Err: err2}
		}
		return Result{Status: StatusOK}
	}

	saved, snapComplete := snapshotClipboardLocked()
	if !snapComplete {
		// Clipboard holds an image/screenshot, spreadsheet, or large selection we cannot losslessly restore:
		// preserve the user's clipboard completely by typing via SendInput Unicode instead!
		_, _, _ = procCloseClipboard.Call()
		_, _, _ = procDestroyWindow.Call(owner)
		_, _, _ = procGlobalFree.Call(hMem)
		logging.Infof("insert: preserving non-text/large clipboard via SendInput Unicode")
		if err2 := typeUnicode(targetHWND, text); err2 != nil {
			return Result{Status: StatusFailed, Message: "Typing failed (preserved clipboard; text saved in History)", Err: err2}
		}
		return Result{Status: StatusOK}
	}

	myOpGen := pasteOpGen.Add(1)
	var seqAfterSet uint32
	if r, _, empErr := procEmptyClipboard.Call(); r == 0 {
		_, _, _ = procCloseClipboard.Call()
		_, _, _ = procDestroyWindow.Call(owner)
		_, _, _ = procGlobalFree.Call(hMem)
		logging.Errorf("insert: EmptyClipboard failed (%v); falling back to SendInput", empErr)
		if err2 := typeUnicode(targetHWND, text); err2 != nil {
			return Result{Status: StatusFailed, Message: "Clipboard error (saved in History)", Err: err2}
		}
		return Result{Status: StatusOK}
	}
	if r, _, setErr := procSetClipboardData.Call(cfUnicodeText, hMem); r == 0 {
		_, _, _ = procCloseClipboard.Call()
		_, _, _ = procDestroyWindow.Call(owner)
		_, _, _ = procGlobalFree.Call(hMem)
		logging.Errorf("insert: SetClipboardData failed (%v); restoring snapshot & falling back to SendInput", setErr)
		if len(saved) > 0 {
			seqTmp, _, _ := procGetClipboardSequenceNum.Call()
			restoreClipboard(saved, uint32(seqTmp), 0)
		}
		if err2 := typeUnicode(targetHWND, text); err2 != nil {
			return Result{Status: StatusFailed, Message: "Clipboard error (saved in History)", Err: err2}
		}
		return Result{Status: StatusOK}
	}
	// Mark temporary paste excluded from Windows 11 Win+V Clipboard History & Cloud Sync
	markExcludeFromClipboardHistoryLocked()
	// Capture sequence number WHILE STILL HOLDING CLIPBOARD OPEN (eliminates race!)
	seqRaw, _, _ := procGetClipboardSequenceNum.Call()
	seqAfterSet = uint32(seqRaw)
	_, _, _ = procCloseClipboard.Call()
	_, _, _ = procDestroyWindow.Call(owner)

	// Settle briefly BEFORE final focus check so no gap exists between verification and SendInput!
	time.Sleep(15 * time.Millisecond)
	if GetForegroundWindow() != targetHWND {
		logging.Warnf("insert: focus changed right before Ctrl+V; leaving text on clipboard")
		return Result{Status: StatusFailedCopied, Message: "Focus changed — text left on clipboard (Ctrl+V)"}
	}

	if err := sendCtrlV(); err != nil {
		logging.Errorf("insert: Ctrl+V failed (%v); text left on clipboard", err)
		return Result{
			Status:  StatusFailedCopied,
			Message: "Paste blocked — text left on clipboard (Ctrl+V)",
			Err:     err,
		}
	}

	logging.Infof("insert: pasted %d runes via Ctrl+V", len([]rune(text)))

	// Restore previous clipboard after slow targets (Electron/browsers/Terminal) finish reading Ctrl+V
	if len(saved) > 0 {
		go func(items []clipboardItem, seq uint32, opGen uint64) {
			time.Sleep(350 * time.Millisecond)
			restoreClipboard(items, seq, opGen)
		}(saved, seqAfterSet, myOpGen)
	}

	return Result{Status: StatusOK}
}
