//go:build windows

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"voicetype/internal/audio"
	"voicetype/internal/autostart"
	"voicetype/internal/cleanup"
	"voicetype/internal/config"
	"voicetype/internal/hotkey"
	"voicetype/internal/insert"
	"voicetype/internal/logging"
	"voicetype/internal/overlay"
	"voicetype/internal/soniox"
	"voicetype/internal/sound"
	"voicetype/internal/state"
	"voicetype/internal/transcript"
	"voicetype/internal/tray"
)

// Controller coordinates the explicit state machine, audio capture, Soniox streaming,
// overlay UI, keyboard hook, text insertion, settings persistence, and system tray.
// All state mutations run strictly on a single serialized event loop — eliminating races.
type Controller struct {
	version string
	paths   config.Paths

	cfgMu       sync.RWMutex
	cfg         config.Config
	cfgExists   bool
	cfgModTime  time.Time
	keyExists   bool
	keyModTime  time.Time
	apiKey      string
	apiKeySrc   string
	devices     []audio.DeviceInfo

	// Owned exclusively by the event loop goroutine:
	sm           *state.Machine
	buf          transcript.Buffer
	targetHWND   uintptr
	sessionStart time.Time
	wsClient     *soniox.Client
	wsCancel     context.CancelFunc
	streamGen    uint64
	reconnectTry int
	reconnecting bool
	pendingAudio [][]byte // bounded 16s audio backlog preserved across reconnect backoff!
	wavWriter    *audio.WAVWriter
	noticeTimer    *time.Timer
	promptOpen     bool
	modelCheckOpen bool

	// Subsystems:
	capturer *audio.Capturer
	hk       *hotkey.Manager
	ov       *overlay.Window
	tr       *tray.Icon
	history  *config.HistoryStore

	evCh    chan func()
	audioCh chan audioPacket
	quitCh  chan struct{}
	doneCh  chan struct{}
}

type audioPacket struct {
	sessionID uint64
	pcm16     []byte
}

// New initializes all VoiceType subsystems (microphone capture initialized at startup!).
func New(version string, paths config.Paths, cfg config.Config, apiKey, apiKeySrc string) (*Controller, error) {
	c := &Controller{
		version:   version,
		paths:     paths,
		cfg:       cfg,
		apiKey:    apiKey,
		apiKeySrc: apiKeySrc,
		sm:        state.New(),
		history:   config.NewHistoryStore(paths.HistoryFile),
		evCh:      make(chan func(), 256),
		audioCh:   make(chan audioPacket, 512),
		quitCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}

	c.recordFileModTimes()

	// Sync autostart registry with config on startup if enabled
	if cfg.StartWithWindows && !autostart.IsEnabled() {
		if err := autostart.SetEnabled(true); err != nil {
			logging.Warnf("autostart: sync failed: %v", err)
		}
	}

	// Enumerate audio capture devices up front
	if devs, err := audio.ListCaptureDevices(); err == nil {
		c.devices = devs
	}

	// Initialize microphone capture AT STARTUP (instant recording on F9!)
	c.capturer = audio.NewCapturer(cfg.MicrophoneID, cfg.GainPercent, func() {
		devs, err := audio.ListCaptureDevices()
		if err == nil {
			c.cfgMu.Lock()
			c.devices = devs
			c.cfgMu.Unlock()
		}
	})
	c.capturer.Start()

	// Overlay window (per-monitor DPI v2, hidden until dictation begins)
	ov, err := overlay.New()
	if err != nil {
		c.capturer.Close()
		return nil, fmt.Errorf("init overlay: %w", err)
	}
	c.ov = ov

	// Low-level keyboard hook (WH_KEYBOARD_LL)
	hk, err := hotkey.NewManager(cfg.Hotkey, hotkey.Callbacks{
		OnHotkeyDown: func() { c.post(c.handleHotkeyDown) },
		OnHotkeyUp:   func() { c.post(c.handleHotkeyUp) },
		OnEnter:      func() { c.post(c.handleEnter) },
		OnEscape:     func() { c.post(c.handleEscape) },
	})
	if err != nil {
		c.ov.Close()
		c.capturer.Close()
		return nil, fmt.Errorf("init hotkey: %w", err)
	}
	c.hk = hk

	// System tray icon + dynamic menu
	tr, err := tray.Start(tray.Callbacks{
		GetMenuState:         c.getMenuState,
		OnLeftClick:          func() { c.post(c.handleTrayToggle) },
		OnSelectMic:          c.onSelectMic,
		OnSelectGain:         c.onSelectGain,
		OnSelectOpacity:      c.onSelectOpacity,
		OnSelectInsertMethod: c.onSelectInsertMethod,
		OnSelectPushToTalk:   c.onSelectPushToTalk,
		OnCopyHistory:        c.onCopyHistory,
		OnClearHistory:       c.onClearHistory,
		OnToggleStartWithWin: c.onToggleStartWithWin,
		OnToggleSoundCues:    c.onToggleSoundCues,
		OnToggleDebugWAVs:    c.onToggleDebugWAVs,
		OnToggleAICleanup:    c.onToggleAICleanup,
		OnSetAPIKey:          func() { c.post(c.onSetAPIKey) },
		OnCheckModelUpdate:   func() { c.post(func() { c.checkModelUpdate(true) }) },
		OnOpenSettings:       func() { tray.OpenPathInShell(c.paths.ConfigFile) },
		OnOpenLog:            func() { tray.OpenPathInShell(logging.Default().FilePath()) },
		OnQuit:               c.RequestQuit,
	})
	if err != nil {
		c.hk.Close()
		c.ov.Close()
		c.capturer.Close()
		return nil, fmt.Errorf("init tray: %w", err)
	}
	c.tr = tr
	c.updateTrayState(tray.IconIdle, fmt.Sprintf("VoiceType — Idle (%s)", cfg.Hotkey))

	return c, nil
}

// Run processes all serialized controller events until RequestQuit is called.
func (c *Controller) Run() {
	defer close(c.doneCh)
	reloadTicker := time.NewTicker(3 * time.Second)
	defer reloadTicker.Stop()

	// First-run convenience: if no Soniox API key is configured yet, guide the user immediately!
	if strings.TrimSpace(c.getAPIKey()) == "" && !c.promptOpen {
		c.promptOpen = true
		go c.promptForNewAPIKey(0, "", "")
	} else if strings.TrimSpace(c.getAPIKey()) != "" {
		// Quiet background check for a newer Soniox real-time model (free GET /v1/models metadata call)
		time.AfterFunc(3*time.Second, func() {
			c.post(func() { c.checkModelUpdate(false) })
		})
	}

	for {
		select {
		case <-c.quitCh:
			c.shutdown()
			return
		case fn := <-c.evCh:
			if fn != nil {
				fn()
			}
		case pkt := <-c.audioCh:
			c.handleAudioPacket(pkt)
		case <-reloadTicker.C:
			c.checkConfigReload()
		}
	}
}

// RequestQuit signals the controller to shut down cleanly.
func (c *Controller) RequestQuit() {
	select {
	case <-c.quitCh:
	default:
		close(c.quitCh)
	}
}

func (c *Controller) post(fn func()) {
	select {
	case c.evCh <- fn:
	case <-c.quitCh:
	}
}

func (c *Controller) shutdown() {
	logging.Infof("app: shutting down")
	c.cancelNoticeTimer()
	c.stopActiveStream()
	c.capturer.EndRecording()
	c.closeDebugWAV()
	c.hk.Close()
	c.ov.Close()
	c.tr.Close()
	c.capturer.Close()
}

func (c *Controller) getConfig() config.Config {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.cfg
}

func (c *Controller) getAPIKey() string {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.apiKey
}

func (c *Controller) saveConfig(mutator func(*config.Config)) {
	c.cfgMu.Lock()
	mutator(&c.cfg)
	c.cfg.Normalize()
	cfgCopy := c.cfg
	c.cfgMu.Unlock()

	if err := config.Save(c.paths.ConfigFile, cfgCopy); err != nil {
		logging.Errorf("config: save failed: %v", err)
	} else {
		c.recordFileModTimes()
		logging.Infof("config: saved (%s)", cfgCopy.SafeSummary())
	}
}

func (c *Controller) recordFileModTimes() {
	c.cfgMu.Lock()
	defer c.cfgMu.Unlock()
	if st, err := os.Stat(c.paths.ConfigFile); err == nil {
		c.cfgExists = true
		c.cfgModTime = st.ModTime()
	} else {
		c.cfgExists = false
		c.cfgModTime = time.Time{}
	}
	if st, err := os.Stat(filepath.Join(c.paths.ExeDir, "api_key.txt")); err == nil {
		c.keyExists = true
		c.keyModTime = st.ModTime()
	} else {
		c.keyExists = false
		c.keyModTime = time.Time{}
	}
}

// checkConfigReload automatically picks up manual edits or deletions of config.json or api_key.txt.
func (c *Controller) checkConfigReload() {
	c.cfgMu.RLock()
	prevCfgEx, prevCfgMt := c.cfgExists, c.cfgModTime
	prevKeyEx, prevKeyMt := c.keyExists, c.keyModTime
	c.cfgMu.RUnlock()

	curCfgEx := false
	var curCfgMt time.Time
	if st, err := os.Stat(c.paths.ConfigFile); err == nil {
		curCfgEx = true
		curCfgMt = st.ModTime()
	}
	curKeyEx := false
	var curKeyMt time.Time
	if st, err := os.Stat(filepath.Join(c.paths.ExeDir, "api_key.txt")); err == nil {
		curKeyEx = true
		curKeyMt = st.ModTime()
	}

	changed := (curCfgEx != prevCfgEx) || (curCfgEx && !curCfgMt.Equal(prevCfgMt)) ||
		(curKeyEx != prevKeyEx) || (curKeyEx && !curKeyMt.Equal(prevKeyMt))
	if !changed {
		return
	}

	newCfg, err := config.Load(c.paths.ConfigFile)
	if err != nil {
		logging.Warnf("config: reload failed (keeping current): %v", err)
		return
	}
	newKey, src := config.ResolveAPIKey(newCfg, c.paths.ExeDir)
	if l := logging.Default(); l != nil {
		l.RegisterSecret(newKey)
		l.RegisterSecret(newCfg.AICleanupAPIKey)
	}

	oldCfg := c.getConfig()
	c.cfgMu.Lock()
	c.cfg = newCfg
	c.apiKey = newKey
	c.apiKeySrc = src
	c.cfgMu.Unlock()
	c.recordFileModTimes()

	if newCfg.Hotkey != oldCfg.Hotkey {
		if err := c.hk.SetHotkey(newCfg.Hotkey); err != nil {
			logging.Warnf("config: invalid reloaded hotkey %q: %v", newCfg.Hotkey, err)
		}
	}
	if newCfg.GainPercent != oldCfg.GainPercent {
		c.capturer.SetGain(newCfg.GainPercent)
	}
	if newCfg.MicrophoneID != oldCfg.MicrophoneID {
		c.capturer.SetDevice(newCfg.MicrophoneID)
	}
	logging.Infof("config: hot-reloaded (%s, keySource=%s)", newCfg.SafeSummary(), src)
}

// --- State machine event handlers (run ONLY on event loop goroutine) ---

func (c *Controller) handleHotkeyDown() {
	cfg := c.getConfig()
	tr := c.sm.OnHotkeyDown(cfg.PushToTalk)
	if tr.Ignored {
		return
	}
	logging.Infof("state: %s -> %s (session=%d, action=%d)", tr.From, tr.To, tr.SessionID, tr.Action)
	switch tr.Action {
	case state.ActionStartSession:
		c.startSession(tr.SessionID)
	case state.ActionSubmit:
		c.submitSession(tr.SessionID)
	}
}

func (c *Controller) handleHotkeyUp() {
	cfg := c.getConfig()
	tr := c.sm.OnHotkeyUp(cfg.PushToTalk)
	if tr.Ignored {
		return
	}
	logging.Infof("state: %s -> %s (session=%d, ptt-release)", tr.From, tr.To, tr.SessionID)
	if tr.Action == state.ActionSubmit {
		c.submitSession(tr.SessionID)
	}
}

func (c *Controller) handleEnter() {
	tr := c.sm.OnEnter()
	if tr.Ignored {
		return
	}
	logging.Infof("state: %s -> %s (session=%d, enter)", tr.From, tr.To, tr.SessionID)
	if tr.Action == state.ActionSubmit {
		c.submitSession(tr.SessionID)
	}
}

func (c *Controller) handleTrayToggle() {
	// Tray click always acts as toggle (even when push_to_talk is enabled) so it never strands a session.
	tr := c.sm.OnHotkeyDown(false)
	if tr.Ignored {
		return
	}
	logging.Infof("state: %s -> %s (session=%d, tray-toggle, action=%d)", tr.From, tr.To, tr.SessionID, tr.Action)
	switch tr.Action {
	case state.ActionStartSession:
		c.startSession(tr.SessionID)
	case state.ActionSubmit:
		c.submitSession(tr.SessionID)
	}
}

func (c *Controller) handleEscape() {
	tr := c.sm.OnEscape()
	if tr.Ignored {
		return
	}
	logging.Infof("state: %s -> %s (session=%d, escape, action=%d)", tr.From, tr.To, tr.SessionID, tr.Action)
	cfg := c.getConfig()
	switch tr.Action {
	case state.ActionEnterEditMode:
		sound.Play(sound.CueEdit, cfg.SoundCues)
		editInitial := c.buf.EnterEditMode()
		c.hk.SetMode(hotkey.ModeEditing)
		c.updateTrayState(tray.IconEditing, "VoiceType — Edit Mode (Mic Live)")
		c.renderOverlay()
		c.ov.EnterEditMode(editInitial)

	case state.ActionClearAndRestart:
		sound.Play(sound.CueClear, cfg.SoundCues)
		c.buf.Reset()
		c.pendingAudio = nil
		c.reconnecting = false
		c.reconnectTry = 0
		c.ov.ExitEditMode()
		c.hk.SetMode(hotkey.ModeRecording)
		c.updateTrayState(tray.IconRecording, "VoiceType — Listening")
		if c.targetHWND != 0 {
			go insert.RestoreFocus(c.targetHWND)
		}
		// Restart fresh Soniox session cleanly while keeping capture active so cleared audio doesn't bleed back!
		c.startSonioxStream(tr.SessionID)
		c.renderOverlay()

	case state.ActionCancelToIdle:
		// 3rd Esc (or dismissing Error): exit listening mode completely without inserting anything!
		sound.Play(sound.CueClear, cfg.SoundCues)
		c.reconnecting = false
		c.pendingAudio = nil
		c.buf.Reset()
		c.capturer.EndRecording()
		c.stopActiveStream()
		c.closeDebugWAV()
		c.ov.ExitEditMode()
		c.hk.SetMode(hotkey.ModeIdle)
		c.ov.Hide()
		if tr.From != state.Error && c.targetHWND != 0 {
			go insert.RestoreFocus(c.targetHWND)
		}
		c.updateTrayState(tray.IconIdle, fmt.Sprintf("VoiceType — Idle (%s)", cfg.Hotkey))
	}
}

func (c *Controller) startSession(sessionID uint64) {
	c.cancelNoticeTimer()
	c.checkConfigReload()
	cfg := c.getConfig()

	// If no Soniox API key is set yet, open the guided setup dialog right away instead of failing with 401!
	if strings.TrimSpace(c.getAPIKey()) == "" {
		c.sm.OnCancel()
		c.hk.SetMode(hotkey.ModeIdle)
		c.updateTrayState(tray.IconIdle, fmt.Sprintf("VoiceType — Idle (%s)", cfg.Hotkey))
		if !c.promptOpen {
			c.promptOpen = true
			go c.promptForNewAPIKey(0, "", "")
		}
		return
	}

	// Remember target window BEFORE anything can touch focus!
	c.targetHWND = insert.GetForegroundWindow()
	c.sessionStart = time.Now()
	c.reconnectTry = 0
	c.reconnecting = false
	c.pendingAudio = nil
	c.buf.Reset()

	sound.Play(sound.CueStart, cfg.SoundCues)
	c.hk.SetMode(hotkey.ModeRecording)
	c.updateTrayState(tray.IconRecording, "VoiceType — Listening")

	// Optional debug WAV recorder (millisecond-unique filename so rapid sessions never overwrite)
	c.closeDebugWAV()
	if cfg.SaveDebugWAVs {
		fname := fmt.Sprintf("rec_%s.wav", time.Now().Format("20060102_150405.000"))
		w, err := audio.CreateWAV(filepath.Join(c.paths.RecordingsDir, fname))
		if err != nil {
			logging.Warnf("debug-wav: failed to create: %v", err)
		} else {
			c.wavWriter = w
			logging.Infof("debug-wav: recording to %s", fname)
		}
	}

	// 1. Start dialing Soniox in parallel (client buffers audio immediately in writeCh!)
	c.startSonioxStream(sessionID)

	// 2. IMMEDIATELY activate audio capture sink via non-blocking audioCh (cannot deadlock EndRecording!)
	c.capturer.BeginRecording(func(pcm16 []byte, level float64) {
		c.ov.SetLevel(level)
		cp := make([]byte, len(pcm16))
		copy(cp, pcm16)
		pkt := audioPacket{sessionID: sessionID, pcm16: cp}
		select {
		case c.audioCh <- pkt:
		default:
			// Drop oldest queued packet under extreme backpressure instead of blocking WASAPI thread
			select {
			case <-c.audioCh:
			default:
			}
			select {
			case c.audioCh <- pkt:
			default:
			}
		}
	})

	// 3. Show overlay immediately (non-activating, semi-transparent, on focused window's monitor!)
	c.renderOverlay()
}

func (c *Controller) handleAudioPacket(pkt audioPacket) {
	if c.sm.SessionID() != pkt.sessionID || !c.sm.State().IsActive() {
		return
	}
	if c.wsClient != nil {
		c.wsClient.SendAudio(pkt.pcm16)
	} else {
		// Preserve audio captured during reconnect backoff (up to 16 seconds)
		if len(c.pendingAudio) >= 800 {
			c.pendingAudio = c.pendingAudio[1:]
		}
		c.pendingAudio = append(c.pendingAudio, pkt.pcm16)
	}
	if c.wavWriter != nil {
		if err := c.wavWriter.WritePCM16(pkt.pcm16); err != nil {
			logging.Warnf("debug-wav: write failed: %v", err)
		}
	}
}

func (c *Controller) startSonioxStream(sessionID uint64) {
	c.stopActiveStream()
	c.streamGen++
	gen := c.streamGen
	cfg := c.getConfig()
	apiKey := c.getAPIKey()

	ctx, cancel := context.WithCancel(context.Background())
	c.wsCancel = cancel

	scfg := soniox.SessionConfig{
		APIKey:              apiKey,
		Model:               cfg.Model,
		LanguageHints:       cfg.LanguageHints,
		LanguageHintsStrict: cfg.LanguageHintsStrict,
		CustomVocabulary:    cfg.CustomVocabulary,
		ContextText:         cfg.ContextText,
	}

	client := soniox.Start(ctx, scfg, soniox.Callbacks{
		OnConnected: func() {
			c.post(func() {
				if c.streamGen != gen {
					return
				}
				c.reconnecting = false
				c.reconnectTry = 0
				tr := c.sm.OnConnected(sessionID)
				if tr.Ignored {
					return
				}
				if c.buf.IsEditing() || c.sm.State() == state.Editing {
					c.updateTrayState(tray.IconEditing, "VoiceType — Edit Mode (Mic Live)")
				} else {
					c.updateTrayState(tray.IconRecording, "VoiceType — Listening")
				}
				c.renderOverlay()
			})
		},
		OnTokens: func(tokens []transcript.Token) {
			c.post(func() {
				if c.streamGen != gen || c.sm.SessionID() != sessionID || !c.sm.State().IsActive() {
					return
				}
				c.buf.ApplyTokens(tokens)
				if strings.TrimSpace(c.buf.FullText()) != "" {
					c.sm.OnSpeechReceived()
				}
				c.renderOverlay()
			})
		},
		OnError: func(err error, fatal bool) {
			c.post(func() {
				if c.streamGen != gen {
					return
				}
				c.handleStreamError(sessionID, gen, err, fatal)
			})
		},
	})
	c.wsClient = client

	// Drain any audio buffered during reconnect backoff immediately into the new socket!
	if len(c.pendingAudio) > 0 {
		logging.Infof("soniox: draining %d audio chunks buffered during reconnect backoff", len(c.pendingAudio))
		for _, ch := range c.pendingAudio {
			client.SendAudio(ch)
		}
		c.pendingAudio = nil
	}
}

func (c *Controller) handleStreamError(sessionID, gen uint64, err error, fatal bool) {
	// Keep all text transcribed so far!
	c.buf.CommitProvisional()
	tr := c.sm.OnConnectionLost(sessionID, err.Error(), fatal)
	if tr.Ignored {
		return
	}
	logging.Errorf("state: stream error (session=%d, fatal=%v): %v", sessionID, fatal, err)
	cfg := c.getConfig()

	if tr.Action == state.ActionShowError {
		// Preserve any manual edits or transcript BEFORE closing edit mode on fatal error!
		var rescueDraft string
		if c.buf.IsEditing() {
			if edited, rErr := c.ov.ReadEditText(); rErr == nil {
				rescueDraft = c.buf.ComposeFinal(edited)
			} else {
				rescueDraft = strings.TrimSpace(c.buf.FullText())
			}
		} else {
			rescueDraft = strings.TrimSpace(c.buf.FullText())
		}
		if rescueDraft != "" {
			_ = c.history.Add(rescueDraft, cfg.HistoryEnabled)
			_ = insert.SetClipboardText(rescueDraft)
			logging.Infof("state: preserved %d-rune draft to clipboard & history before fatal error teardown", len([]rune(rescueDraft)))
		}

		c.reconnecting = false
		c.pendingAudio = nil
		c.stopActiveStream()
		c.capturer.EndRecording()
		c.closeDebugWAV()
		c.ov.ExitEditMode()
		c.hk.SetMode(hotkey.ModeIdle)
		sound.Play(sound.CueError, cfg.SoundCues)
		c.updateTrayState(tray.IconError, "VoiceType — Error")
		c.renderOverlay()

		errLower := strings.ToLower(err.Error())
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "402") || strings.Contains(err.Error(), "403") ||
			strings.Contains(errLower, "unauthorized") || strings.Contains(errLower, "invalid_api_key") || strings.Contains(errLower, "insufficient_credits") {
			if !c.promptOpen {
				c.promptOpen = true
				c.cfgMu.RLock()
				src := c.apiKeySrc
				c.cfgMu.RUnlock()
				go c.promptForNewAPIKey(sessionID, err.Error(), src)
			}
		} else if strings.Contains(errLower, "model_not_available") || strings.Contains(errLower, "invalid_model") {
			c.scheduleErrorAutoHide(sessionID, 5*time.Second)
			c.checkModelUpdate(true)
		} else {
			c.scheduleErrorAutoHide(sessionID, 5*time.Second)
		}
		return
	}

	if tr.Action == state.ActionStartReconnect {
		// Reclaim any unsent audio frames from the failed wsClient so failed dials never drop speech!
		if c.wsClient != nil {
			unsent := c.wsClient.DrainUnsent()
			if len(unsent) > 0 {
				combined := append(unsent, c.pendingAudio...)
				if len(combined) > 800 {
					combined = combined[len(combined)-800:]
				}
				c.pendingAudio = combined
				logging.Infof("soniox: reclaimed %d unsent audio chunks from failed attempt", len(unsent))
			}
		}
		c.stopActiveStream()
		c.reconnecting = true
		c.reconnectTry++
		delay := time.Duration(c.reconnectTry) * 800 * time.Millisecond
		if delay > 4*time.Second {
			delay = 4 * time.Second
		}
		c.updateTrayState(tray.IconError, "VoiceType — Reconnecting…")
		c.renderOverlay()

		logging.Infof("soniox: scheduling reconnect attempt %d in %v (preserving %d runes, editMode=%v)",
			c.reconnectTry, delay, len([]rune(c.buf.FullText())), c.buf.IsEditing())
		time.AfterFunc(delay, func() {
			c.post(func() {
				// Dial regardless of whether user entered Edit Mode during backoff!
				if c.sm.SessionID() != sessionID || c.streamGen != gen || !c.reconnecting || !c.sm.State().IsActive() {
					return
				}
				c.startSonioxStream(sessionID)
			})
		})
	}
}

func (c *Controller) checkModelUpdate(interactive bool) {
	if c.modelCheckOpen {
		return
	}
	apiKey := strings.TrimSpace(c.getAPIKey())
	if apiKey == "" {
		if interactive {
			c.onSetAPIKey()
		}
		return
	}
	c.modelCheckOpen = true
	cfg := c.getConfig()

	go func(key, curModel, dismissed string, showUpToDate bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		models, err := soniox.FetchModels(ctx, key, soniox.DefaultModelsURL)
		if err != nil {
			logging.Warnf("soniox: model update check failed: %v", err)
			if showUpToDate {
				tray.ShowInfoDialog("VoiceType — Soniox Model Check", "Could not reach Soniox API to check models:\n"+err.Error())
			}
			c.post(func() { c.modelCheckOpen = false })
			return
		}

		latest, ok := soniox.SelectLatestRealtimeModel(models)
		if !ok {
			if showUpToDate {
				tray.ShowInfoDialog("VoiceType — Soniox Model Check", "No real-time models were returned by Soniox.")
			}
			c.post(func() { c.modelCheckOpen = false })
			return
		}

		logging.Infof("soniox: model check complete (current=%s, latest=%s)", curModel, latest.ID)
		if !soniox.IsModelOlder(curModel, latest.ID) {
			if showUpToDate {
				tray.ShowInfoDialog("VoiceType — Soniox Model Check",
					fmt.Sprintf("You are already using the latest Soniox real-time model:\n\n%s (%s)", latest.ID, latest.Name))
			}
			c.post(func() { c.modelCheckOpen = false })
			return
		}

		// Don't re-prompt automatically if the user already dismissed this exact version
		if !showUpToDate && dismissed == latest.ID {
			logging.Infof("soniox: skipping prompt for previously dismissed model %s", latest.ID)
			c.post(func() { c.modelCheckOpen = false })
			return
		}

		accepted := tray.PromptModelUpdate(curModel, latest.ID, latest.Name)
		c.post(func() {
			c.modelCheckOpen = false
			if accepted {
				c.saveConfig(func(cfg *config.Config) {
					cfg.Model = latest.ID
					cfg.DismissedModelUpdate = ""
				})
				logging.Infof("soniox: upgraded model %s -> %s", curModel, latest.ID)
			} else {
				c.saveConfig(func(cfg *config.Config) {
					cfg.DismissedModelUpdate = latest.ID
				})
				logging.Infof("soniox: user declined upgrade to %s", latest.ID)
			}
		})
	}(apiKey, cfg.Model, cfg.DismissedModelUpdate, interactive)
}

func (c *Controller) onSetAPIKey() {
	if c.promptOpen {
		return
	}
	c.promptOpen = true
	c.cfgMu.RLock()
	src := c.apiKeySrc
	hasKey := strings.TrimSpace(c.apiKey) != ""
	c.cfgMu.RUnlock()
	if hasKey {
		go c.promptForNewAPIKey(0, "manual", src)
	} else {
		go c.promptForNewAPIKey(0, "", "")
	}
}

func (c *Controller) promptForNewAPIKey(originSessionID uint64, errMsg, activeSource string) {
	var msg string
	switch {
	case errMsg == "":
		msg = "Welcome to VoiceType! A Soniox API key is required for real-time speech recognition."
	case errMsg == "manual":
		msg = fmt.Sprintf("Update your Soniox API key (current source: %s).", activeSource)
	case strings.Contains(errMsg, "402") || strings.Contains(strings.ToLower(errMsg), "insufficient_credits"):
		msg = "Your Soniox account appears out of credit. Top up a small balance in Soniox Console, or paste another API key:"
	default:
		msg = fmt.Sprintf("Soniox rejected the API key from %s (%s).",
			activeSource, transcript.TruncateRunes(errMsg, 56))
	}
	key, ok := tray.PromptAPIKey(msg)
	c.post(func() {
		c.promptOpen = false
		if ok && key != "" {
			if l := logging.Default(); l != nil {
				l.RegisterSecret(key)
			}
			c.saveConfig(func(cfg *config.Config) {
				cfg.APIKey = key
			})
			// Re-resolve through the standard priority chain so effective source is always truthful
			effKey, effSrc := config.ResolveAPIKey(c.getConfig(), c.paths.ExeDir)
			c.cfgMu.Lock()
			c.apiKey = effKey
			c.apiKeySrc = effSrc
			c.cfgMu.Unlock()
			logging.Infof("config: updated Soniox API key via prompt (effectiveSource=%s, key=%s)", effSrc, logging.MaskSecret(effKey))
			if effKey != "" {
				c.checkModelUpdate(false)
			}
		}
		// Only dismiss Error state if no newer dictation session started while the prompt was open!
		if originSessionID != 0 && c.sm.SessionID() == originSessionID && c.sm.State() == state.Error {
			c.sm.OnCancel()
			c.ov.Hide()
			c.updateTrayState(tray.IconIdle, fmt.Sprintf("VoiceType — Idle (%s)", c.getConfig().Hotkey))
		}
	})
}

func (c *Controller) submitSession(sessionID uint64) {
	cfg := c.getConfig()

	// 1. Read edited text BEFORE hiding overlay if we were in Edit mode!
	var finalText string
	if c.buf.IsEditing() {
		edited, err := c.ov.ReadEditText()
		if err != nil {
			logging.Errorf("session %d: failed reading edit box (%v); keeping draft safe in buffer", sessionID, err)
			finalText = strings.TrimSpace(c.buf.FullText())
		} else {
			finalText = c.buf.ComposeFinal(edited)
		}
	} else {
		finalText = strings.TrimSpace(c.buf.FullText())
	}

	// 2. IMMEDIATELY stop audio capture & close WebSocket (never wait for Soniox finished:true!)
	c.reconnecting = false
	c.pendingAudio = nil
	c.capturer.EndRecording()
	c.stopActiveStream()
	c.closeDebugWAV()
	c.hk.SetMode(hotkey.ModeIdle)
	c.ov.Hide()

	targetHWND := c.targetHWND
	if finalText == "" {
		logging.Infof("session %d: submitted empty transcript", sessionID)
		c.sm.OnInsertComplete(sessionID)
		c.updateTrayState(tray.IconIdle, fmt.Sprintf("VoiceType — Idle (%s)", cfg.Hotkey))
		if cfg.PushToTalk {
			c.showNotice("No Speech Received", "Hold the hotkey slightly longer while speaking", targetHWND, 2000*time.Millisecond)
		}
		return
	}

	sound.Play(sound.CueSubmit, cfg.SoundCues)

	// 3. Perform optional AI clean-up, save to history FIRST (text is never lost!), then insert
	go func(sid uint64, hwnd uintptr, rawText string, cfgCopy config.Config) {
		textToInsert := rawText
		if cfgCopy.AICleanupEnabled && strings.TrimSpace(cfgCopy.AICleanupAPIKey) != "" {
			textToInsert = cleanup.Clean(context.Background(), rawText, cleanup.Options{
				Enabled:  cfgCopy.AICleanupEnabled,
				APIKey:   cfgCopy.AICleanupAPIKey,
				Model:    cfgCopy.AICleanupModel,
				Endpoint: cfgCopy.AICleanupEndpoint,
			})
		}

		// ALWAYS save to history before inserting so text can never be lost!
		if err := c.history.Add(textToInsert, cfgCopy.HistoryEnabled); err != nil {
			logging.Warnf("history: save failed: %v", err)
		}

		// Give the overlay a brief moment to disappear cleanly before restoring focus
		time.Sleep(25 * time.Millisecond)
		res := insert.Insert(hwnd, textToInsert, cfgCopy.InsertionMethod)

		c.post(func() {
			c.sm.OnInsertComplete(sid)
			c.updateTrayState(tray.IconIdle, fmt.Sprintf("VoiceType — Idle (%s)", c.getConfig().Hotkey))
			if res.Status != insert.StatusOK && res.Message != "" {
				title := "Copied to Clipboard"
				if res.Status == insert.StatusFailed {
					title = "Insertion Failed"
				}
				c.showNotice(title, res.Message, hwnd, 3500*time.Millisecond)
			}
		})
	}(sessionID, targetHWND, finalText, cfg)
}

func (c *Controller) showNotice(title, msg string, targetHWND uintptr, d time.Duration) {
	c.cancelNoticeTimer()
	cfg := c.getConfig()
	c.ov.Update(overlay.Snapshot{
		Kind:           overlay.KindNotice,
		OpacityPercent: cfg.OverlayOpacity,
		StatusText:     title,
		ErrorBanner:    msg,
		HotkeyLabel:    cfg.Hotkey,
		TargetHWND:     targetHWND,
	})
	c.noticeTimer = time.AfterFunc(d, func() {
		c.post(func() {
			if c.sm.State() == state.Idle {
				c.ov.Hide()
			}
		})
	})
}

func (c *Controller) scheduleErrorAutoHide(sessionID uint64, d time.Duration) {
	c.cancelNoticeTimer()
	c.noticeTimer = time.AfterFunc(d, func() {
		c.post(func() {
			tr := c.sm.OnErrorDismissed(sessionID)
			if !tr.Ignored {
				c.ov.Hide()
				c.updateTrayState(tray.IconIdle, fmt.Sprintf("VoiceType — Idle (%s)", c.getConfig().Hotkey))
			}
		})
	})
}

func (c *Controller) cancelNoticeTimer() {
	if c.noticeTimer != nil {
		c.noticeTimer.Stop()
		c.noticeTimer = nil
	}
}

func (c *Controller) stopActiveStream() {
	if c.wsCancel != nil {
		c.wsCancel()
		c.wsCancel = nil
	}
	if c.wsClient != nil {
		c.wsClient.CloseImmediate()
		c.wsClient = nil
	}
}

func (c *Controller) closeDebugWAV() {
	if c.wavWriter != nil {
		if err := c.wavWriter.Close(); err != nil {
			logging.Warnf("debug-wav: close failed: %v", err)
		}
		c.wavWriter = nil
	}
}

func (c *Controller) renderOverlay() {
	cfg := c.getConfig()
	st := c.sm.State()
	finalStr, nonFinalStr := c.buf.Parts()
	postFinal, postNonFinal := c.buf.PostEditParts()
	editActive := c.buf.IsEditing()
	micErr := c.capturer.LastError()

	var kind overlay.Kind
	var statusText string
	var errBanner string

	switch st {
	case state.Connecting:
		kind = overlay.KindConnecting
		statusText = "Connecting…"
		if micErr != "" {
			statusText = "Microphone Error"
			errBanner = micErr
		}
	case state.Recording:
		kind = overlay.KindListening
		statusText = "Listening"
		if micErr != "" {
			statusText = "Microphone Error"
			errBanner = micErr
		}
	case state.Editing:
		if c.reconnecting {
			kind = overlay.KindReconnecting
			statusText = fmt.Sprintf("Edit Mode · Reconnecting (#%d)…", c.reconnectTry)
		} else {
			kind = overlay.KindEditing
			statusText = "Edit Mode (Mic Live)"
		}
	case state.Reconnecting:
		kind = overlay.KindReconnecting
		if editActive {
			statusText = fmt.Sprintf("Edit Mode · Reconnecting (#%d)…", c.reconnectTry)
		} else {
			statusText = fmt.Sprintf("Reconnecting (#%d)…", c.reconnectTry)
		}
		errBanner = c.sm.LastError()
	case state.Error:
		kind = overlay.KindError
		statusText = "Connection Error"
		errBanner = c.sm.LastError()
	default:
		c.ov.Hide()
		return
	}

	c.ov.Update(overlay.Snapshot{
		Kind:             kind,
		EditActive:       editActive,
		ClearedOnce:      c.sm.ClearedOnce(),
		OpacityPercent:   cfg.OverlayOpacity,
		StatusText:       statusText,
		FinalText:        finalStr,
		NonFinalText:     nonFinalStr,
		PostEditFinal:    postFinal,
		PostEditNonFinal: postNonFinal,
		ErrorBanner:      errBanner,
		HotkeyLabel:      cfg.Hotkey,
		StartTime:        c.sessionStart,
		TargetHWND:       c.targetHWND,
	})
}

func (c *Controller) updateTrayState(ic tray.IconState, tooltip string) {
	if c.tr != nil {
		c.tr.SetState(ic, tooltip)
	}
}

// --- Tray menu callbacks ---

func (c *Controller) getMenuState() tray.MenuState {
	// Refresh active microphones dynamically when opening the tray menu
	if devs, err := audio.ListCaptureDevices(); err == nil {
		c.cfgMu.Lock()
		c.devices = devs
		c.cfgMu.Unlock()
	}

	c.cfgMu.RLock()
	cfg := c.cfg
	devs := make([]audio.DeviceInfo, len(c.devices))
	copy(devs, c.devices)
	c.cfgMu.RUnlock()

	_, activeMic := c.capturer.ActiveDevice()
	if activeMic == "" {
		activeMic = "No Mic"
	}

	return tray.MenuState{
		StatusLine:    fmt.Sprintf("VoiceType v%s  ·  %s  ·  %s", c.version, cfg.Hotkey, transcript.TruncateRunes(activeMic, 28)),
		Version:       c.version,
		Config:        cfg,
		Devices:       devs,
		RecentHistory: c.history.Recent(config.TrayHistoryCount),
		StartWithWin:  autostart.IsEnabled(),
	}
}

// All tray settings mutations are serialized through c.post(...) so concurrent tray clicks
// can never race or persist an older snapshot over a newer one!
func (c *Controller) onSelectMic(deviceID, deviceName string) {
	c.post(func() {
		c.capturer.SetDevice(deviceID)
		c.saveConfig(func(cfg *config.Config) {
			cfg.MicrophoneID = deviceID
			cfg.MicrophoneName = deviceName
		})
	})
}

func (c *Controller) onSelectGain(gainPercent int) {
	c.post(func() {
		c.capturer.SetGain(gainPercent)
		c.saveConfig(func(cfg *config.Config) {
			cfg.GainPercent = gainPercent
		})
	})
}

func (c *Controller) onSelectOpacity(opacityPercent int) {
	c.post(func() {
		c.saveConfig(func(cfg *config.Config) {
			cfg.OverlayOpacity = opacityPercent
		})
		if c.sm.State().IsActive() {
			c.renderOverlay()
		}
	})
}

func (c *Controller) onSelectInsertMethod(method string) {
	c.post(func() {
		c.saveConfig(func(cfg *config.Config) {
			cfg.InsertionMethod = method
		})
	})
}

func (c *Controller) onSelectPushToTalk(ptt bool) {
	c.post(func() {
		c.saveConfig(func(cfg *config.Config) {
			cfg.PushToTalk = ptt
		})
	})
}

func (c *Controller) onCopyHistory(text string) {
	if err := insert.SetClipboardText(text); err != nil {
		logging.Errorf("tray: failed copying history item: %v", err)
		return
	}
	logging.Infof("tray: copied history transcript (%d runes) to clipboard", len([]rune(text)))
	c.post(func() {
		if c.sm.State() == state.Idle {
			c.showNotice("Copied to Clipboard", "Transcript copied to clipboard!", insert.GetForegroundWindow(), 1800*time.Millisecond)
		}
	})
}

func (c *Controller) onClearHistory() {
	c.post(func() {
		if err := c.history.Clear(); err != nil {
			logging.Errorf("history: clear failed: %v", err)
		}
	})
}

func (c *Controller) onToggleStartWithWin(enabled bool) {
	c.post(func() {
		if err := autostart.SetEnabled(enabled); err != nil {
			logging.Errorf("autostart: set(%v) failed: %v", enabled, err)
			return
		}
		c.saveConfig(func(cfg *config.Config) {
			cfg.StartWithWindows = enabled
		})
	})
}

func (c *Controller) onToggleSoundCues(enabled bool) {
	c.post(func() {
		c.saveConfig(func(cfg *config.Config) {
			cfg.SoundCues = enabled
		})
	})
}

func (c *Controller) onToggleDebugWAVs(enabled bool) {
	c.post(func() {
		c.saveConfig(func(cfg *config.Config) {
			cfg.SaveDebugWAVs = enabled
		})
	})
}

func (c *Controller) onToggleAICleanup(enabled bool) {
	c.post(func() {
		c.saveConfig(func(cfg *config.Config) {
			cfg.AICleanupEnabled = enabled
		})
	})
}
