package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"voicetype/internal/logging"
)

const (
	// DefaultSonioxModel is the current recommended Soniox real-time STT model.
	DefaultSonioxModel = "stt-rt-v6"

	InsertMethodPaste     = "paste"
	InsertMethodSendInput = "sendinput"

	MinGainPercent = 50
	MaxGainPercent = 300

	MinOverlayOpacity     = 40
	MaxOverlayOpacity     = 100
	DefaultOverlayOpacity = 82

	MaxHistoryEntries = 50
	TrayHistoryCount  = 10
)

// Config holds all persistent user settings stored in %APPDATA%\VoiceType\config.json.
type Config struct {
	APIKey               string   `json:"api_key"`
	Model                string   `json:"model"`
	DismissedModelUpdate string   `json:"dismissed_model_update,omitempty"`
	MicrophoneID         string   `json:"microphone_id"`
	MicrophoneName      string   `json:"microphone_name"`
	GainPercent         int      `json:"gain_percent"`
	OverlayOpacity      int      `json:"overlay_opacity"`
	Hotkey              string   `json:"hotkey"`
	PushToTalk          bool     `json:"push_to_talk"`
	InsertionMethod     string   `json:"insertion_method"`
	LanguageHints       []string `json:"language_hints"`
	LanguageHintsStrict bool     `json:"language_hints_strict"`
	CustomVocabulary    []string `json:"custom_vocabulary"`
	ContextText         string   `json:"context_text"`
	HistoryEnabled      bool     `json:"history_enabled"`
	SoundCues           bool     `json:"sound_cues"`
	SaveDebugWAVs       bool     `json:"save_debug_wavs"`
	StartWithWindows    bool     `json:"start_with_windows"`
	AICleanupEnabled    bool     `json:"ai_cleanup_enabled"`
	AICleanupAPIKey     string   `json:"ai_cleanup_api_key"`
	AICleanupModel      string   `json:"ai_cleanup_model"`
	AICleanupEndpoint   string   `json:"ai_cleanup_endpoint"`
}

// DefaultConfig returns a Config initialized with sensible defaults.
func DefaultConfig() Config {
	return Config{
		APIKey:              "",
		Model:               DefaultSonioxModel,
		MicrophoneID:        "",
		MicrophoneName:      "Default",
		GainPercent:         100,
		OverlayOpacity:      DefaultOverlayOpacity,
		Hotkey:              "F9",
		PushToTalk:          false,
		InsertionMethod:     InsertMethodPaste,
		LanguageHints:       []string{"en", "el"},
		LanguageHintsStrict: false,
		CustomVocabulary:    []string{},
		ContextText:         "",
		HistoryEnabled:      true,
		SoundCues:           true,
		SaveDebugWAVs:       false,
		StartWithWindows:    false,
		AICleanupEnabled:    false,
		AICleanupAPIKey:     "",
		AICleanupModel:      "claude-haiku-4-5",
		AICleanupEndpoint:   "https://api.anthropic.com/v1/messages",
	}
}

// Normalize clamps and fixes invalid or empty fields in place.
func (c *Config) Normalize() {
	if strings.TrimSpace(c.Model) == "" {
		c.Model = DefaultSonioxModel
	}
	if c.GainPercent < MinGainPercent {
		c.GainPercent = MinGainPercent
	}
	if c.GainPercent > MaxGainPercent {
		c.GainPercent = MaxGainPercent
	}
	if c.OverlayOpacity == 0 {
		c.OverlayOpacity = DefaultOverlayOpacity
	}
	if c.OverlayOpacity < MinOverlayOpacity {
		c.OverlayOpacity = MinOverlayOpacity
	}
	if c.OverlayOpacity > MaxOverlayOpacity {
		c.OverlayOpacity = MaxOverlayOpacity
	}
	if strings.TrimSpace(c.Hotkey) == "" {
		c.Hotkey = "F9"
	}
	switch strings.ToLower(strings.TrimSpace(c.InsertionMethod)) {
	case InsertMethodSendInput:
		c.InsertionMethod = InsertMethodSendInput
	default:
		c.InsertionMethod = InsertMethodPaste
	}
	if len(c.LanguageHints) == 0 {
		c.LanguageHints = []string{"en", "el"}
	}
	if c.CustomVocabulary == nil {
		c.CustomVocabulary = []string{}
	}
	if c.MicrophoneID == "" && c.MicrophoneName == "" {
		c.MicrophoneName = "Default"
	}
	if strings.TrimSpace(c.AICleanupModel) == "" {
		c.AICleanupModel = "claude-haiku-4-5"
	}
	if strings.TrimSpace(c.AICleanupEndpoint) == "" {
		c.AICleanupEndpoint = "https://api.anthropic.com/v1/messages"
	}
}

// SafeSummary returns a log-safe string representation with API keys masked.
func (c Config) SafeSummary() string {
	return fmt.Sprintf(
		"model=%s mic=%q(%s) gain=%d%% hotkey=%s ptt=%v insert=%s langs=%v vocab=%d history=%v sounds=%v debugWAV=%v autostart=%v aiCleanup=%v apiKey=%s",
		c.Model, c.MicrophoneName, c.MicrophoneID, c.GainPercent, c.Hotkey, c.PushToTalk,
		c.InsertionMethod, c.LanguageHints, len(c.CustomVocabulary), c.HistoryEnabled,
		c.SoundCues, c.SaveDebugWAVs, c.StartWithWindows, c.AICleanupEnabled,
		logging.MaskSecret(c.APIKey),
	)
}

// AppDataDir returns %APPDATA%\VoiceType (or fallback in user config dir).
func AppDataDir() (string, error) {
	if ad := os.Getenv("APPDATA"); ad != "" {
		return filepath.Join(ad, "VoiceType"), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "VoiceType"), nil
}

// Paths holds standard file locations for VoiceType.
type Paths struct {
	BaseDir       string
	ConfigFile    string
	HistoryFile   string
	LogDir        string
	RecordingsDir string
	ExeDir        string
}

// ResolvePaths computes all application paths. If overrideBaseDir is non-empty (e.g. in tests),
// it is used instead of %APPDATA%\VoiceType.
func ResolvePaths(overrideBaseDir string) (Paths, error) {
	base := overrideBaseDir
	if base == "" {
		var err error
		base, err = AppDataDir()
		if err != nil {
			return Paths{}, err
		}
	}
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	return Paths{
		BaseDir:       base,
		ConfigFile:    filepath.Join(base, "config.json"),
		HistoryFile:   filepath.Join(base, "history.json"),
		LogDir:        filepath.Join(base, "logs"),
		RecordingsDir: filepath.Join(base, "recordings"),
		ExeDir:        exeDir,
	}, nil
}

// Load reads config from path. If the file does not exist, it returns DefaultConfig
// and writes the initial config file so the user can edit it easily.
func Load(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg.Normalize()
			_ = Save(path, cfg)
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultConfig(), fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.Normalize()
	return cfg, nil
}

var fileSaveMu sync.Mutex

// atomicWriteFile safely replaces path using a unique temp file + MOVEFILE_REPLACE_EXISTING
// so concurrent saves can never delete or corrupt config.json or history.json.
func atomicWriteFile(path string, data []byte) error {
	fileSaveMu.Lock()
	defer fileSaveMu.Unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".voicetype-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// Save atomically writes cfg to path as formatted JSON.
func Save(path string, cfg Config) error {
	cfg.Normalize()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(path, data)
}

// ResolveAPIKey returns the effective Soniox API key and its source description.
// Priority:
//  1. SONIOX_API_KEY environment variable
//  2. api_key.txt next to executable (or current working directory)
//  3. config.json api_key field
//  4. DefaultFallbackAPIKey
func ResolveAPIKey(cfg Config, exeDir string) (key string, source string) {
	if v := strings.TrimSpace(os.Getenv("SONIOX_API_KEY")); v != "" {
		return v, "env:SONIOX_API_KEY"
	}
	candidates := []string{}
	if exeDir != "" {
		candidates = append(candidates, filepath.Join(exeDir, "api_key.txt"))
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" && cwd != exeDir {
		candidates = append(candidates, filepath.Join(cwd, "api_key.txt"))
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			if k := parseKeyFile(string(b)); k != "" {
				return k, "file:" + p
			}
		}
	}
	if v := strings.TrimSpace(cfg.APIKey); v != "" {
		return v, "config.json"
	}
	return DefaultFallbackAPIKey, "default-fallback"
}

func parseKeyFile(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "SONIOX_API_KEY=") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "SONIOX_API_KEY="))
		}
		return strings.Trim(line, `"'`)
	}
	return ""
}

// HistoryEntry represents one submitted dictation transcript.
type HistoryEntry struct {
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
	ephemeral bool      // true when captured with history_enabled=false; never serialized to disk
}

// HistoryStore manages persisted dictation history (newest first, up to MaxHistoryEntries).
type HistoryStore struct {
	mu      sync.Mutex
	path    string
	entries []HistoryEntry
}

// NewHistoryStore loads existing history from path (or starts empty).
func NewHistoryStore(path string) *HistoryStore {
	hs := &HistoryStore{path: path}
	if data, err := os.ReadFile(path); err == nil {
		var loaded []HistoryEntry
		if err := json.Unmarshal(data, &loaded); err == nil {
			if len(loaded) > MaxHistoryEntries {
				loaded = loaded[:MaxHistoryEntries]
			}
			hs.entries = loaded
		}
	}
	return hs
}

// Add prepends a non-empty transcript to history and saves if persist is true.
// Entries added with persist=false are marked ephemeral and are NEVER written to disk,
// even if history persistence is later re-enabled.
func (h *HistoryStore) Add(text string, persist bool) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	entry := HistoryEntry{Text: text, Timestamp: time.Now(), ephemeral: !persist}
	// Avoid duplicating identical consecutive entry at the top
	if len(h.entries) > 0 && h.entries[0].Text == text {
		h.entries[0].Timestamp = entry.Timestamp
		if !persist {
			h.entries[0].ephemeral = true
		}
	} else {
		h.entries = append([]HistoryEntry{entry}, h.entries...)
		if len(h.entries) > MaxHistoryEntries {
			h.entries = h.entries[:MaxHistoryEntries]
		}
	}
	if !persist {
		return nil
	}
	return h.saveLocked()
}

// Recent returns up to n most recent entries (copy).
func (h *HistoryStore) Recent(n int) []HistoryEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n <= 0 || len(h.entries) == 0 {
		return nil
	}
	if n > len(h.entries) {
		n = len(h.entries)
	}
	out := make([]HistoryEntry, n)
	copy(out, h.entries[:n])
	return out
}

// Clear removes all history entries and updates disk.
func (h *HistoryStore) Clear() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = nil
	return h.saveLocked()
}

func (h *HistoryStore) saveLocked() error {
	if h.path == "" {
		return nil
	}
	toPersist := make([]HistoryEntry, 0, len(h.entries))
	for _, e := range h.entries {
		if !e.ephemeral {
			toPersist = append(toPersist, e)
		}
	}
	data, err := json.MarshalIndent(toPersist, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(h.path, data)
}
