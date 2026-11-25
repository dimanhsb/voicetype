//go:build windows

package main

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	procAllocConsole  = kernel32.NewProc("AllocConsole")
)

const attachParentProcess = ^uintptr(0) // (DWORD)-1

// enableDebugConsole preserves piped stdout if redirected, otherwise attaches to the
// parent terminal or allocates a new console window for live -console logging.
func enableDebugConsole() io.Writer {
	if h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil && h != 0 && h != windows.InvalidHandle {
		if ft, err := windows.GetFileType(h); err == nil && ft != windows.FILE_TYPE_UNKNOWN {
			return os.Stdout
		}
	}
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	if r == 0 {
		_, _, _ = procAllocConsole.Call()
	}
	conOut, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return os.Stderr
	}
	os.Stdout = conOut
	os.Stderr = conOut
	return conOut
}
