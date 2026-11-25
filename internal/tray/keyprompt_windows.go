//go:build windows

package tray

import (
	"math"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const (
	// SonioxConsoleURL is the official Soniox Console where users add credits and create API keys.
	SonioxConsoleURL = "https://console.soniox.com"

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
	emSetSel       = 0x00B1
	bsDefPushBtn   = 0x0001
	bsPushBtn      = 0x0000
	wmKeyDown      = 0x0100
	wmCommand      = 0x0111
	wmClose        = 0x0010
	wmSetFont      = 0x0030
	idOK           = 1
	idCancel       = 2
	idKeyEdit      = 100
	idOpenSoniox   = 101
	swShow         = 5
	defaultGuiFont = 17
)

var (
	gdi32                = syscall.NewLazyDLL("gdi32.dll")
	procGetStockObject   = gdi32.NewProc("GetStockObject")
	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	procGetWindowTextLen = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW   = user32.NewProc("GetWindowTextW")
	procSetFocus         = user32.NewProc("SetFocus")
	procShowWindow       = user32.NewProc("ShowWindow")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procIsDialogMessageW = user32.NewProc("IsDialogMessageW")
	procGetDpiForSystem  = user32.NewProc("GetDpiForSystem")
	procGetKeyState      = user32.NewProc("GetKeyState")
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
				if cmdID == idOpenSoniox {
					go OpenPathInShell(SonioxConsoleURL)
					return 0
				}
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

func promptDPI() float64 {
	if procGetDpiForSystem.Find() == nil {
		if d, _, _ := procGetDpiForSystem.Call(); d >= 72 {
			return float64(d) / 96.0
		}
	}
	return 1.0
}

func makePromptFont(pt float64, weight int32, scale float64) uintptr {
	h := -int32(math.Round(pt * (96.0 * scale) / 72.0))
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	f, _, _ := procCreateFontW.Call(
		uintptr(h), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5, 0,
		uintptr(unsafe.Pointer(face)),
	)
	if f == 0 {
		f, _, _ = procGetStockObject.Call(defaultGuiFont)
	}
	return f
}

// PromptAPIKey opens a DPI-aware native Windows setup dialog guiding the user to get
// a Soniox API key + small amount of credit from https://console.soniox.com and paste it in.
// Returns (key, true) if submitted, or ("", false) if cancelled.
func PromptAPIKey(reason string) (string, bool) {
	ch := make(chan struct {
		key string
		ok  bool
	}, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		scale := promptDPI()
		sc := func(v int) uintptr {
			return uintptr(int(math.Round(float64(v) * scale)))
		}

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

		title, _ := syscall.UTF16PtrFromString("VoiceType — Set Up Soniox API Key")
		st := &keyPromptState{}
		activePrompt = st
		defer func() { activePrompt = nil }()

		sw, _, _ := procGetSystemMetrics.Call(0)
		sh, _, _ := procGetSystemMetrics.Call(1)
		winW, winH := sc(548), sc(316)
		x := uintptr(0)
		if sw > winW {
			x = (sw - winW) / 2
		}
		y := uintptr(0)
		if sh > winH {
			y = (sh - winH) / 2
		}

		hwnd, _, _ := procCreateWindowExW.Call(
			uintptr(wsExTopmost|wsExDlgModal),
			uintptr(unsafe.Pointer(clsName)),
			uintptr(unsafe.Pointer(title)),
			uintptr(wsOverlapped|wsCaption|wsSysMenu),
			x, y, winW, winH,
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

		fontReg := makePromptFont(9.5, 400, scale)
		fontBold := makePromptFont(9.5, 600, scale)
		defer func() {
			if fontReg != 0 {
				_, _, _ = procDeleteObject.Call(fontReg)
			}
			if fontBold != 0 {
				_, _, _ = procDeleteObject.Call(fontBold)
			}
		}()

		if strings.TrimSpace(reason) == "" {
			reason = "Welcome to VoiceType! A Soniox API key is required for real-time speech recognition."
		}

		staticCls, _ := syscall.UTF16PtrFromString("STATIC")

		// 1. Header / reason
		hdrText, _ := syscall.UTF16PtrFromString(reason)
		hdr, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(hdrText)),
			uintptr(wsChild|wsVisible),
			sc(18), sc(14), sc(496), sc(40), hwnd, 0, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(hdr, wmSetFont, fontBold, 1)

		// 2. Step-by-step instructions for getting a Soniox API key & small prepaid credit
		guide := "How to get your Soniox API key:\r\n" +
			"1. Click \"Open Soniox Console\" below (https://console.soniox.com) and sign in.\r\n" +
			"2. Open Organization -> Billing and add a small amount of Soniox credit\r\n" +
			"    (real-time speech is pay-as-you-go at ~$0.12/hour, so $5 lasts 40+ hours).\r\n" +
			"3. Open Project -> API keys, create a key, and paste it below:"
		guideText, _ := syscall.UTF16PtrFromString(guide)
		guideLbl, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(guideText)),
			uintptr(wsChild|wsVisible),
			sc(18), sc(60), sc(496), sc(100), hwnd, 0, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(guideLbl, wmSetFont, fontReg, 1)

		// 3. Input label + masked API key field
		keyLblTxt, _ := syscall.UTF16PtrFromString("Soniox API Key (saved locally to %APPDATA%\\VoiceType\\config.json):")
		keyLbl, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(keyLblTxt)),
			uintptr(wsChild|wsVisible),
			sc(18), sc(166), sc(496), sc(20), hwnd, 0, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(keyLbl, wmSetFont, fontReg, 1)

		editCls, _ := syscall.UTF16PtrFromString("EDIT")
		empty, _ := syscall.UTF16PtrFromString("")
		edit, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(editCls)), uintptr(unsafe.Pointer(empty)),
			uintptr(wsChild|wsVisible|wsBorder|wsTabStop|esAutoHScroll|esPassword),
			sc(18), sc(190), sc(496), sc(28), hwnd, idKeyEdit, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(edit, wmSetFont, fontReg, 1)
		st.editHWND = edit

		// 4. Action buttons: Open Soniox Console (left) | Save Key & Cancel (right)
		btnCls, _ := syscall.UTF16PtrFromString("BUTTON")

		openTxt, _ := syscall.UTF16PtrFromString("Open Soniox Console")
		openBtn, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(openTxt)),
			uintptr(wsChild|wsVisible|wsTabStop|bsPushBtn),
			sc(18), sc(232), sc(168), sc(32), hwnd, idOpenSoniox, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(openBtn, wmSetFont, fontReg, 1)

		okTxt, _ := syscall.UTF16PtrFromString("Save Key")
		okBtn, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(okTxt)),
			uintptr(wsChild|wsVisible|wsTabStop|bsDefPushBtn),
			sc(324), sc(232), sc(92), sc(32), hwnd, idOK, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(okBtn, wmSetFont, fontBold, 1)

		cancelTxt, _ := syscall.UTF16PtrFromString("Cancel")
		cancelBtn, _, _ := procCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(cancelTxt)),
			uintptr(wsChild|wsVisible|wsTabStop|bsPushBtn),
			sc(422), sc(232), sc(92), sc(32), hwnd, idCancel, hInst, 0,
		)
		_, _, _ = procSendMessageW.Call(cancelBtn, wmSetFont, fontReg, 1)

		_, _, _ = procShowWindow.Call(hwnd, swShow)
		_, _, _ = procSetForegroundWindow.Call(hwnd)
		_, _, _ = procSetFocus.Call(edit)

		var msg winMsg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			// Support Ctrl+A inside the API key edit box
			if msg.HWND == edit && msg.Message == wmKeyDown && msg.WParam == 'A' {
				ctrlState, _, _ := procGetKeyState.Call(0x11)
				if int16(ctrlState) < 0 {
					_, _, _ = procSendMessageW.Call(edit, emSetSel, 0, ^uintptr(0))
					continue
				}
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
