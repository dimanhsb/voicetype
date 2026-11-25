//go:build windows

package singleinstance

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const mutexName = `Local\VoiceType_SingleInstance_Mutex_v2`

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOK              = 0x00000000
	mbIconInformation = 0x00000040
	mbSetForeground   = 0x00010000
	mbTopmost         = 0x00040000
)

// ErrAlreadyRunning is returned when another VoiceType instance already holds the mutex.
var ErrAlreadyRunning = errors.New("VoiceType is already running")

// Lock represents a held single-instance Windows mutex.
type Lock struct {
	handle windows.Handle
}

// Acquire tries to claim the named Windows mutex. If another instance already holds it,
// ErrAlreadyRunning is returned.
func Acquire() (*Lock, error) {
	namePtr, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(nil, false, namePtr)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			if h != 0 {
				_ = windows.CloseHandle(h)
			}
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return &Lock{handle: h}, nil
}

// Release frees the single-instance mutex.
func (l *Lock) Release() {
	if l == nil || l.handle == 0 {
		return
	}
	_ = windows.CloseHandle(l.handle)
	l.handle = 0
}

// ShowAlreadyRunningDialog displays a friendly native dialog informing the user
// that VoiceType is already running in the system tray.
func ShowAlreadyRunningDialog() {
	title, _ := syscall.UTF16PtrFromString("VoiceType")
	msg, _ := syscall.UTF16PtrFromString("VoiceType is already running in the system tray.\n\nLook for the teal microphone icon near the clock.")
	_, _, _ = procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(msg)),
		uintptr(unsafe.Pointer(title)),
		uintptr(mbOK|mbIconInformation|mbSetForeground|mbTopmost),
	)
}
