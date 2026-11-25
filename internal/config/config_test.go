package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voicetype/internal/logging"
)

func TestConfigLoadSaveAndNormalize(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// First load creates default config
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if cfg.Model != DefaultSonioxModel || cfg.Hotkey != "F9" || cfg.InsertionMethod != InsertMethodPaste {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	// Mutate with out-of-range values and verify normalization + persistence
	cfg.GainPercent = 999
	cfg.Hotkey = "Ctrl+Shift+Space"
	cfg.MicrophoneID = "mic-123"
	cfg.MicrophoneName = "USB Mic"
	cfg.CustomVocabulary = []string{"Soniox", "VoiceType", "Θεσσαλονίκη"}
	cfg.APIKey = "snx_proj_secret_example_1234567890"
	if err := Save(cfgPath, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if loaded.GainPercent != MaxGainPercent {
		t.Fatalf("GainPercent = %d, want clamped %d", loaded.GainPercent, MaxGainPercent)
	}
	if loaded.MicrophoneID != "mic-123" || loaded.Hotkey != "Ctrl+Shift+Space" || len(loaded.CustomVocabulary) != 3 {
		t.Fatalf("reloaded mismatch: %+v", loaded)
	}

	// Ensure SafeSummary NEVER contains the raw API key!
	summary := loaded.SafeSummary()
	if strings.Contains(summary, "snx_proj_secret_example") {
		t.Fatalf("SafeSummary leaked raw API key: %s", summary)
	}
}

func TestAPIKeyResolutionPriorityAndLogRedaction(t *testing.T) {
	exeDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.APIKey = "snx_proj_from_config_999999"

	t.Setenv("SONIOX_API_KEY", "")
	// 1. config.json when no env or api_key.txt
	k, src := ResolveAPIKey(cfg, exeDir)
	if k != cfg.APIKey || src != "config.json" {
		t.Fatalf("got (%q, %q), want config.json", k, src)
	}

	// 2. api_key.txt overrides config.json
	keyFile := filepath.Join(exeDir, "api_key.txt")
	if err := os.WriteFile(keyFile, []byte("# comment\nsnx_proj_from_txt_file_888888\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, src = ResolveAPIKey(cfg, exeDir)
	if k != "snx_proj_from_txt_file_888888" || !strings.HasPrefix(src, "file:") {
		t.Fatalf("api_key.txt priority failed: got (%q, %q)", k, src)
	}

	// 3. env var overrides api_key.txt
	t.Setenv("SONIOX_API_KEY", "snx_proj_from_env_777777")
	k, src = ResolveAPIKey(cfg, exeDir)
	if k != "snx_proj_from_env_777777" || src != "env:SONIOX_API_KEY" {
		t.Fatalf("env priority failed: got (%q, %q)", k, src)
	}

	// Verify logging.Redact masks API keys in arbitrary strings
	redacted := logging.Redact("Authorization: Bearer snx_proj_from_env_777777 failed", []string{"snx_proj_from_env_777777"})
	if strings.Contains(redacted, "from_env") {
		t.Fatalf("Redact leaked key: %s", redacted)
	}
}

func TestHistoryStorePersistenceAndCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history.json")
	hs := NewHistoryStore(p)

	for i := 0; i < 60; i++ {
		if err := hs.Add("Transcript Ελληνικά "+string(rune('A'+(i%26)))+string(rune('0'+(i%10))), true); err != nil {
			t.Fatal(err)
		}
	}
	// Reload from disk
	hs2 := NewHistoryStore(p)
	all := hs2.Recent(100)
	if len(all) != MaxHistoryEntries {
		t.Fatalf("persisted history len = %d, want %d", len(all), MaxHistoryEntries)
	}
	top10 := hs2.Recent(TrayHistoryCount)
	if len(top10) != TrayHistoryCount {
		t.Fatalf("Recent(10) len = %d", len(top10))
	}

	// Verify private entries added with persist=false NEVER leak to disk when history is later re-enabled!
	if err := hs2.Add("private secret dictation", false); err != nil {
		t.Fatal(err)
	}
	if err := hs2.Add("public dictation", true); err != nil {
		t.Fatal(err)
	}
	hs3 := NewHistoryStore(p)
	for _, e := range hs3.Recent(MaxHistoryEntries) {
		if e.Text == "private secret dictation" {
			t.Fatalf("ephemeral private entry leaked to disk!")
		}
	}
}
