//go:build windows

package autostart

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName  = "VoiceType"
)

// IsEnabled checks whether VoiceType is currently registered to start with Windows.
func IsEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	val, _, err := k.GetStringValue(valueName)
	return err == nil && val != ""
}

// SetEnabled adds or removes VoiceType from HKCU\Software\Microsoft\Windows\CurrentVersion\Run.
func SetEnabled(enabled bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open Run registry key: %w", err)
	}
	defer k.Close()

	if !enabled {
		err := k.DeleteValue(valueName)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	return k.SetStringValue(valueName, fmt.Sprintf(`"%s"`, exePath))
}
