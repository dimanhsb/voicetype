//go:build windows

package insert

import (
	"os"
	"testing"
)

func TestSetClipboardTextWithOwnedHWND(t *testing.T) {
	if os.Getenv("VOICETYPE_DESKTOP_TESTS") != "1" {
		t.Skip("skipping desktop clipboard mutation test (set VOICETYPE_DESKTOP_TESTS=1 to run)")
	}
	sample := "VoiceType clipboard test — Καλημέρα 🎤"
	if err := SetClipboardText(sample); err != nil {
		t.Fatalf("SetClipboardText failed: %v", err)
	}
}
