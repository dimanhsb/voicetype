# VoiceType

Lightweight, single-executable Windows 11 system-tray voice-typing app powered by the **Soniox Real-Time Speech-to-Text WebSocket API (`stt-rt-v6`)** with native English + Greek code-switching.

## Quick Start

1. Place your Soniox API key in **`api_key.txt`** next to `VoiceType.exe` (see [`api_key.example.txt`](./api_key.example.txt)), set the `SONIOX_API_KEY` environment variable, or enter it in the first-run prompt dialog.
2. Run **`VoiceType.exe`** (starts silently in the Windows system tray with the teal microphone icon — no console window, no installer, no runtime required).
3. Press **`F9`** anywhere to start speaking (English, Greek, or mixed in the same sentence).
4. Press **`F9`** or **`Enter`** to insert the transcript immediately into the focused app.
5. Press **`Esc`** once to enter **Edit Mode** (edit text while the microphone keeps streaming new words below; press `Enter` or `F9` to insert everything).
6. Press **`Esc`** twice to clear everything and start over.
7. Press **`Esc`** a third time to cancel and close the listening overlay completely without inserting anything.

## Building & Distribution

- Run `.\build.ps1 -Test` to run all unit tests and build `dist\VoiceType.exe` (no API keys are embedded in source or binaries).
- Copy [`api_key.example.txt`](./api_key.example.txt) to `dist\api_key.txt` (gitignored) with your Soniox API key to launch immediately.

## Tray Menu Controls

Right-click the teal microphone icon in the system tray to configure:
- **Microphone** (Windows Default + every active capture device; hot-plug safe)
- **Input Gain** (`50%` – `300%`)
- **Overlay Opacity** (`100% Solid`, `90%`, `82% Semi-transparent`, `72%`, `60%`, `50%` — see behind the overlay while speaking; gently boosts opacity in Edit Mode for caret readability)
- **Insertion Method** (`Clipboard Paste (Fast, Ctrl+V)` or `Type Unicode (SendInput)`; automatically preserves non-text/image/large clipboards!)
- **Dictation Mode** (`Toggle` or `Push-to-Talk`)
- **Recent Transcripts** (last 10 saved transcripts — click any item to copy to clipboard)
- **Start with Windows**, **Sound Cues**, **Save Debug WAVs**, **AI Clean-up (Claude)**
- **Open Settings File** (`%APPDATA%\VoiceType\config.json`) — changes hot-reload automatically
- **Open Log File** (`%APPDATA%\VoiceType\logs\voicetype.log`) — API keys are always redacted

## Building & Testing

See [AGENTS.md](./AGENTS.md) for architecture details and portable Go build commands (`.\build.ps1 -Test -Selftest`).
