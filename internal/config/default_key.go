package config

// DefaultFallbackAPIKey is intentionally empty in source/releases so no credentials
// are ever committed or shipped in binaries. Provide your Soniox API key via:
//   1. SONIOX_API_KEY environment variable
//   2. api_key.txt next to VoiceType.exe
//   3. %APPDATA%\VoiceType\config.json ("api_key")
//   4. The built-in first-run API key prompt dialog
const DefaultFallbackAPIKey = ""

