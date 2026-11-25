param(
    [switch]$Test,
    [switch]$Selftest,
    [string]$Out = "dist\VoiceType.exe",
    [string]$Version = "1.0.0",
    [string]$Tags = ""
)

$ErrorActionPreference = "Stop"
$RepoRoot = $PSScriptRoot
$GoExe = Join-Path $RepoRoot ".tools\go\bin\go.exe"
$WinresExe = Join-Path $RepoRoot ".tools\bin\go-winres.exe"

if (-not (Test-Path $GoExe)) {
    throw "Portable Go not found at $GoExe. Download go windows-amd64 zip into .tools\go."
}

$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:PATH = "$(Join-Path $RepoRoot '.tools\go\bin');$(Join-Path $RepoRoot '.tools\bin');$env:PATH"

if (-not (Test-Path (Join-Path $RepoRoot "assets\icon_256.png"))) {
    Write-Host "Generating icons..."
    & $GoExe run ./cmd/genicons
}

if ($Test) {
    Write-Host "Running unit tests..."
    & $GoExe test -v ./internal/... ./cmd/...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

if (Test-Path $WinresExe) {
    Write-Host "Embedding Windows resources (icon, version info, Per-Monitor DPI v2 manifest)..."
    & $WinresExe make --in (Join-Path $RepoRoot "winres\winres.json") --out (Join-Path $RepoRoot "cmd\voicetype\rsrc") --arch amd64
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} else {
    Write-Warning "go-winres.exe not found at $WinresExe; building without embedded .syso resources."
}

$OutDir = Split-Path -Parent (Join-Path $RepoRoot $Out)
if ($OutDir -and -not (Test-Path $OutDir)) {
    New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
}

Write-Host "Building $Out (version $Version, tags='$Tags')..."
$buildArgs = @("build", "-trimpath", "-ldflags=-H windowsgui -s -w -X main.appVersion=$Version", "-o", (Join-Path $RepoRoot $Out))
if ($Tags -ne "") {
    $buildArgs += @("-tags", $Tags)
}
$buildArgs += "./cmd/voicetype"
& $GoExe @buildArgs
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

$fi = Get-Item (Join-Path $RepoRoot $Out)
Write-Host ("Built {0} ({1:N2} MB)" -f $fi.FullName, ($fi.Length / 1MB))

if ($Selftest) {
    Write-Host "Running live Soniox WebSocket self-test..."
    $p = Start-Process -FilePath (Join-Path $RepoRoot $Out) -ArgumentList "-console","-selftest",(Join-Path $RepoRoot "testdata\sample_en.wav") -Wait -PassThru
    $logPath = Join-Path $env:APPDATA "VoiceType\logs\voicetype.log"
    if (Test-Path $logPath) {
        Get-Content $logPath -Tail 12
    }
    if ($p.ExitCode -ne 0) { exit $p.ExitCode }
}
