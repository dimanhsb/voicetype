//go:build windows

package hotkey

import "testing"

func TestParseSpec(t *testing.T) {
	cases := []struct {
		raw  string
		mods uint32
		vk   uint32
	}{
		{"F9", 0, 0x78},
		{"F8", 0, 0x77},
		{"Ctrl+Shift+Space", ModCtrl | ModShift, 0x20},
		{"Alt+Space", ModAlt, 0x20},
		{"Ctrl+Alt+V", ModCtrl | ModAlt, 'V'},
		{"Pause", 0, 0x13},
	}
	for _, tc := range cases {
		sp, err := ParseSpec(tc.raw)
		if err != nil {
			t.Fatalf("ParseSpec(%q) error: %v", tc.raw, err)
		}
		if sp.Mods != tc.mods || sp.VK != tc.vk {
			t.Fatalf("ParseSpec(%q) = (mods=%x vk=%x), want (mods=%x vk=%x)", tc.raw, sp.Mods, sp.VK, tc.mods, tc.vk)
		}
	}

	if _, err := ParseSpec("NotARealKey"); err == nil {
		t.Fatalf("expected error for invalid hotkey")
	}
}
