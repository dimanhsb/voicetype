//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"voicetype/internal/app"
	"voicetype/internal/config"
	"voicetype/internal/logging"
	"voicetype/internal/overlay"
	"voicetype/internal/singleinstance"
	"voicetype/internal/soniox"
)

// appVersion is overridden at build time via -ldflags="-X main.appVersion=...".
var appVersion = "1.0.0"

func main() {
	consoleFlag := flag.Bool("console", false, "Attach or allocate a console window for live debug logs")
	selftestWAV := flag.String("selftest", "", "Stream a WAV file to Soniox at real-time speed and print the transcript")
	versionFlag := flag.Bool("version", false, "Print VoiceType version and exit")
	flag.Parse()

	var consoleWriter io.Writer
	if *consoleFlag || *selftestWAV != "" || *versionFlag {
		consoleWriter = enableDebugConsole()
	}

	if *versionFlag {
		fmt.Fprintf(consoleWriter, "VoiceType v%s\n", appVersion)
		return
	}

	// Enable Per-Monitor DPI Awareness v2 before creating any windows
	overlay.EnablePerMonitorDPIv2()

	paths, err := config.ResolvePaths("")
	if err != nil {
		if consoleWriter != nil {
			fmt.Fprintf(consoleWriter, "fatal: resolve paths: %v\n", err)
		}
		os.Exit(1)
	}

	logger, err := logging.Init(paths.LogDir, consoleWriter)
	if err != nil {
		if consoleWriter != nil {
			fmt.Fprintf(consoleWriter, "fatal: init logger: %v\n", err)
		}
		os.Exit(1)
	}
	defer logger.Close()

	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		logging.Warnf("config: %v (using defaults)", err)
	}

	apiKey, keySrc := config.ResolveAPIKey(cfg, paths.ExeDir)
	logger.RegisterSecret(apiKey)
	logger.RegisterSecret(cfg.AICleanupAPIKey)

	logging.Infof("VoiceType v%s starting (keySource=%s, %s)", appVersion, keySrc, cfg.SafeSummary())

	// Self-test mode: bypass single-instance lock, stream WAV to Soniox, verify transcript, and exit!
	if *selftestWAV != "" {
		runSelfTest(cfg, apiKey, *selftestWAV)
		return
	}

	// Single-instance check
	lock, err := singleinstance.Acquire()
	if err != nil {
		if errors.Is(err, singleinstance.ErrAlreadyRunning) {
			logging.Infof("another VoiceType instance is already running; showing notice and exiting")
			singleinstance.ShowAlreadyRunningDialog()
			return
		}
		logging.Errorf("single-instance mutex error: %v", err)
		os.Exit(1)
	}
	defer lock.Release()

	ctrl, err := app.New(appVersion, paths, cfg, apiKey, keySrc)
	if err != nil {
		logging.Errorf("fatal: failed to initialize VoiceType: %v", err)
		os.Exit(1)
	}

	// Handle Ctrl+C / SIGTERM cleanly when running with -console
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		ctrl.RequestQuit()
	}()

	ctrl.Run()
	logging.Infof("VoiceType exited cleanly")
}

func runSelfTest(cfg config.Config, apiKey, wavPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	logging.Infof("selftest: streaming %s to Soniox (%s) at real-time speed...", wavPath, cfg.Model)
	res, err := soniox.StreamWAVSelfTest(ctx, soniox.SessionConfig{
		APIKey:              apiKey,
		Model:               cfg.Model,
		LanguageHints:       cfg.LanguageHints,
		LanguageHintsStrict: cfg.LanguageHintsStrict,
		CustomVocabulary:    cfg.CustomVocabulary,
		ContextText:         cfg.ContextText,
	}, wavPath)
	if err != nil {
		if res != nil && res.Transcript != "" {
			logging.Warnf("selftest partial transcript before failure: %q", res.Transcript)
		}
		logging.Errorf("selftest FAILED: %v", err)
		os.Exit(1)
	}
	if res.Transcript == "" {
		logging.Errorf("selftest FAILED: empty transcript returned")
		os.Exit(1)
	}
	logging.Infof("selftest PASSED!")
	logging.Infof("  Audio duration:  %v (%d chunks)", res.AudioDuration, res.ChunksSent)
	logging.Infof("  Connect latency: %v", res.ConnectLatency)
	logging.Infof("  First token:     %v", res.FirstTokenWait)
	logging.Infof("  Total elapsed:   %v", res.Duration)
	logging.Infof("  Transcript:      %q", res.Transcript)
}
