//go:build windows

package tray

import (
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const (
	wsOverlapped   = 0x00000000
	wsCaption      = 0x00C00000
	wsSysMenu      = 0x00080000
	wsVisible      = 0x10000000
	wsChild        = 0x40000000
	wsTabStop      = 0x00010000
	wsBorder       = 0x00800000
	wsExTopmost    = 0x00000008
	wsExDlgModal   = 0x00000001
	esPassword     = 0x0020
	esAutoHScroll  = 0x0080
	bsDefPushBtn   = 0x0001
	bsPushBtn      = 0x0000
	wmCommand      = 0x0111
	wmClose        = 0x0010
	wmSetFont      = 0x0030
	idOK           = 1
	idCancel       = 2
	idKeyEdit      = 100
	swShow         = 5
	defaultGuiFont = 17
)

var (
	gdi32                = syscall.NewLazyDLL("gdi32.dll")
	procGetStockObject   = gdi32.NewProc("GetStockObject")
	procGetWindowTextLen = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW   = user32.NewProc("GetWindowTextW")
	procSetFocus         = user32.NewProc("SetFocus")
	procShowWindow       = user32.NewProc("ShowWindow")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procIsDialogMessageW = user32.NewProc("IsDialogMessageW")
)

type keyPromptState struct {
	hwnd     uintptr
	editHWND uintptr
	result   string
	accepted bool
	done     bool
}

var activePrompt *keyPromptState
var promptProc uintptr

func init() {
	promptProc = syscall.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
		st := activePrompt
		if st != nil && st.hwnd == hwnd {
			switch uint32(msg) {
			case wmCommand:
				cmdID := uint16(wParam & 0xFFFF)
				if cmdID == idOK {
					n, _, _ := procGetWindowTextLen.Call(st.editHWND)
					if n > 0 {
						buf := make([]uint16, n+1)
						_, _, _ = procGetWindowTextW.Call(st.editHWND, uintptr(unsafe.Pointer(&buf[0])), n+1)
						st.result = strings.TrimSpace(syscall.UTF16ToString(buf))
					}
					st.accepted = true
					st.done = true
					_, _, _ = procDestroyWindow.Call(hwnd)
					return 0
				}
				if cmdID == idCancel {
					st.done = true
					_, _, _ = procDestroyWindow.Call(hwnd)
					return 0
				}
			case wmClose:
				st.done = true
				_, _, _ = procDestroyWindow.Call(hwnd)
				return 0
			case wmDestroy:
				_, _, _ = procPostQuitMessage.Call(0)
				return 0
			}
		}
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return r
	})
}

// PromptAPIKey opens a simple native Windows dialog asking the user for a Soniox API key.
// Returns (key, true) if submitted, or ("", false) if cancelled.
func PromptAPIKey(reason string) (string, bool) {
	ch := make(chan struct {
		key string
		ok  bool
	}, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hInst, _, _ := procGetModuleHandleW.Call(0)
		clsName, _ := syscall.UTF16PtrFromString("VoiceTypeAPIKeyPrompt")
		wc := wndClassExW{
			CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			LpfnWndProc:   promptProc,
			HInstance:     hInst,
			HbrBackground: 16, // COLOR_BTNFACE + 1
			LpszClassName: clsName,
		}
		_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

		title, _ := syscall.UTF16PtrFromString("VoiceType — Soniox API Key")
		st := &keyPromptState{}
		activePrompt = st
		defer func() { activePrompt = nil }()

		sw, _, _ := procGetSystemMetrics.Call(0)
		sh, _, _ := procGetSystemMetrics.Call(1)
		w, h := uintptr(480), uintptr(185)
		x := (sw - w) / 2
		y := (sh - h) / 2

		hwnd, _, _ := procCreateWindowExW.Call(
			uintptr(wsExTopmost|wsExDlgModal),
			uintptr(unsafe.Pointer(clsName)),
			uintptr(unsafe.Pointer(title)),
			uintptr(wsOverlapped|wsCaption|wsSysMenu),
			x, y, w, h,
			0, 0, hInst, 0,
		)
		if hwnd == 0 {
			ch <- struct {
				key string
				ok  bool
			}{}
			return
		}
		st.hwnd = hwnd

		font, _, _ := procGetStockObject.Call(defaultGuiFont)

		if reason == "" {
			reason = "Enter your Soniox API key to enable speech recognition:"
		}
		staticCls, _ := syscall.UTF16PtrFromString("STATIC")
		lblText, _ := syscall.UTF16PtrFromString(reason)
		lbl, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(lblText)),
			uintptr(wsChild|wsVisible),
			16, 14, 434, 38, hwnd, 0, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(lbl, wmSetFont, font, 1)

		editCls, _ := syscall.UTF16PtrFromString("EDIT")
		empty, _ := syscall.UTF16PtrFromString("")
		edit, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(editCls)), uintptr(unsafe.Pointer(empty)),
			uintptr(wsChild|wsVisible|wsBorder|wsTabStop|esAutoHScroll|esPassword),
			16, 58, 434, 28, hwnd, idKeyEdit, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(edit, wmSetFont, font, 1)
		st.editHWND = edit

		btnCls, _ := syscall.UTF16PtrFromString("BUTTON")
		okTxt, _ := syscall.UTF16PtrFromString("Save Key")
		okBtn, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(okTxt)),
			uintptr(wsChild|wsVisible|wsTabStop|bsDefPushBtn),
			266, 102, 88, 30, hwnd, idOK, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(okBtn, wmSetFont, font, 1)

		cancelTxt, _ := syscall.UTF16PtrFromString("Cancel")
		cancelBtn, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(cancelTxt)),
			uintptr(wsChild|wsVisible|wsTabStop|bsPushBtn),
			362, 102, 88, 30, hwnd, idCancel, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(cancelBtn, wmSetFont, font, 1)

		_, _, _ = procShowWindow.Call(hwnd, swShow)
		_, _, _ = procSetForegroundWindow.Call(hwnd)
		_, _, _ = procSetFocus.Call(edit)

		var msg winMsg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			handled, _, _ := procIsDialogMessageW.Call(hwnd, uintptr(unsafe.Pointer(&msg)))
			if handled == 0 {
				_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
				_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
			}
		}

		ch <- struct {
			key string
			ok  bool
		}{key: st.result, ok: st.accepted && st.result != ""}
	}()

	res := <-ch
	return res.key, res.ok
}
