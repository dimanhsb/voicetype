# VoiceType — Architecture & Developer Guide

VoiceType is a lightweight, single-executable Windows 11 system-tray voice-typing app powered by the Soniox Real-Time Speech-to-Text WebSocket API (`stt-rt-v6`).

## Build & Run (Portable Go — No System Installation Required)

This repository uses a workspace-local portable Go toolchain in `.tools/go` (gitignored) so nothing needs to be installed globally.

```powershell
# Run all unit tests
.\build.ps1 -Test

# Build the Windows GUI executable (no console window, embedded icon + Per-Monitor DPI v2 manifest)
.\build.ps1

# Run with live console logging for debugging
.\dist\VoiceType.exe -console

# Run the live Soniox WebSocket self-test against the generated English WAV
.\dist\VoiceType.exe -console -selftest .\testdata\sample_en.wav
```

## API Key Resolution Order

VoiceType resolves the Soniox API key in this priority order (and **never** logs it in plain text or embeds any key in source/releases):
1. `SONIOX_API_KEY` environment variable (recommended for CI/tests)
2. `api_key.txt` next to `VoiceType.exe` (trimmed single-line text file for quick launch; see `api_key.example.txt`)
3. `%APPDATA%\VoiceType\config.json` (`api_key` field)
4. Interactive Win32 prompt dialog on first run / auth failure

## Package Structure

- `cmd/voicetype/` — Entry point, CLI flags (`-console`, `-selftest`, `-version`), single-instance lock, and main app wiring.
- `cmd/genicons/` — Pure-Go SDF icon generator producing 16–256 px PNGs and multi-resolution `.ico` files.
- `internal/state/` — Explicit session state machine (`Idle → Connecting → Recording ⇄ Editing ⇄ Reconnecting → Inserting → Idle`) with generation IDs so stale async events cannot wedge the app.
- `internal/transcript/` — Final + non-final token merger, special marker filtering (`<end>`, `<fin>`), edit-mode continuation buffer (edits never overwritten; new speech appended on submit), and Unicode-safe rune truncation.
- `internal/audio/` — Pure-Go WASAPI capture over direct Win32 COM (`IMMDeviceEnumerator`, `IAudioClient`, `IAudioCaptureClient`, `IMMNotificationClient`), startup initialization, zero-loss pre-roll/ring buffering, windowed-sinc resampling to 16 kHz mono `pcm_s16le`, gain (50–300%), RMS/peak level metering, WAV reader/writer, and hot-plug/default-device recovery.
- `internal/soniox/` — Soniox real-time WebSocket client (`wss://stt-rt.soniox.com/transcribe-websocket`, `Authorization: Bearer` header, `stt-rt-v6`, `pcm_s16le`, `language_hints`, `enable_endpoint_detection`, `enable_language_identification`, `context.terms`, keepalive, structured errors, mid-dictation reconnect).
- `internal/hotkey/` — Dedicated Win32 `WH_KEYBOARD_LL` hook thread supporting configurable toggle & push-to-talk hotkeys (default `F9`), swallowing `Enter` and `Esc` during recording/editing without leaking to target apps.
- `internal/overlay/` — Per-monitor DPI v2 topmost semi-transparent overlay (`WS_EX_LAYERED` + `SetLayeredWindowAttributes`, configurable `overlay_opacity` 40–100%, default 82%, +10% readability bump in edit mode) rendered on the focused window's monitor, non-focus-stealing during recording, smooth 1–5 line auto-grow + bottom-pinned scroll, isolated level-meter invalidation (no edit-box blinking), edit mode with live continuation strip, and clear status/error banners.
- `internal/insert/` — Focus restoration (`SetForegroundWindow` + fallback), UIPI elevation check, clipboard-preserving fast paste (`Ctrl+V` with sequence-number-checked restore), and `SendInput` `KEYEVENTF_UNICODE` fallback/option.
- `internal/tray/` — Pure Win32 `Shell_NotifyIconW` + dynamic popup menu (Microphone selector, Gain 50–300%, Insertion Method toggle, Toggle vs Push-to-Talk, Last 10 Transcripts, Start with Windows, Sound Cues, Debug WAVs, AI Clean-up, Open Settings, Open Log, Quit).
- `internal/config/` — Persistent JSON settings in `%APPDATA%\VoiceType\config.json` and history in `%APPDATA%\VoiceType\history.json`.
- `internal/logging/` — Rotating file logger in `%APPDATA%\VoiceType\logs\voicetype.log` with automatic API key redaction and optional `-console` tee.
- `internal/sound/` — Synthesized in-memory PCM chimes played via Win32 `PlaySoundW` (zero external assets).
- `internal/cleanup/` — Optional AI transcript clean-up (off by default; preserves Greek/English code-switching).
- `internal/singleinstance/` — Named Win32 mutex preventing duplicate instances.
- `internal/autostart/` — Windows Registry `HKCU\...\Run` integration for Start with Windows.

## Key Engineering Decisions

1. **Zero-loss capture on hotkey**: WASAPI capture opens at startup (not on hotkey) and runs an event-driven loop. Pressing the hotkey immediately queues PCM frames (including a 160 ms pre-roll ring) while Soniox dials in parallel; once connected, the backlog drains first.
2. **Pure Go Win32 + COM (`CGO_ENABLED=0`)**: No C compiler or runtime dependencies. Direct COM vtable bindings for WASAPI give full access to `IMMNotificationClient` hot-plug notifications without third-party COM wrapper quirks.
3. **Edit mode without caret disruption**: Entering edit mode opens a focused Win32 `EDIT` box prefilled with the current transcript. Audio continues streaming; new speech appears in a dedicated live strip directly below the edit box so user edits and caret position are never clobbered. Submitting joins the edited text and live continuation cleanly.
4. **Soniox `stt-rt-v6` protocol**: Uses `Authorization: Bearer` header (replacing deprecated inline `api_key`), `audio_format: "pcm_s16le"`, filters `<end>` and `<fin>` control tokens, sends `{"type": "keepalive"}` during silence pauses, and closes immediately on submit for instant insertion without waiting for server `finished: true`.
5. **No warm idle WebSocket**: Because the shared API key has a limited budget and idle sockets time out, VoiceType never opens a WebSocket or streams audio while idle. Zero-loss pre-roll buffering achieves instant start without idle socket cost.

## Known Limitations

- Target windows running elevated (Administrator) block synthetic input via Windows UIPI unless VoiceType is also run elevated. VoiceType detects elevated target windows, places the text on the clipboard, saves it to history, and shows a clear banner in the overlay (`Admin window — copied to clipboard (Ctrl+V)`).
