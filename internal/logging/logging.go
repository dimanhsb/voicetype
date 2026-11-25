package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxLogFileSize = 2 * 1024 * 1024 // 2 MiB per log file
	maxLogBackups  = 5
)

var (
	reSonioxKey = regexp.MustCompile(`snx_[A-Za-z0-9_.\-]{12,}`)
	reBearer    = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9_.\-]{12,}`)
	reAnthropic = regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{12,}`)
	reJSONKey   = regexp.MustCompile(`(?i)("?(?:api_key|apikey|authorization|x-api-key)"?\s*[:=]\s*"?)([^"\s,})]{8,})("?)`)
)

// MaskSecret redacts an API key or secret while preserving a short prefix/suffix for diagnostics.
func MaskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	r := []rune(s)
	if len(r) <= 10 {
		return "****"
	}
	return string(r[:6]) + "****" + string(r[len(r)-4:])
}

// Redact scrubs known API key patterns and any explicitly registered secrets from a log line.
func Redact(msg string, extraSecrets []string) string {
	out := msg
	for _, sec := range extraSecrets {
		if len(sec) >= 8 {
			out = strings.ReplaceAll(out, sec, MaskSecret(sec))
		}
	}
	out = reSonioxKey.ReplaceAllStringFunc(out, MaskSecret)
	out = reAnthropic.ReplaceAllStringFunc(out, MaskSecret)
	out = reBearer.ReplaceAllStringFunc(out, func(m string) string {
		idx := strings.IndexByte(m, ' ')
		if idx < 0 {
			return "Bearer ****"
		}
		return m[:idx+1] + MaskSecret(strings.TrimSpace(m[idx+1:]))
	})
	out = reJSONKey.ReplaceAllStringFunc(out, func(m string) string {
		parts := reJSONKey.FindStringSubmatch(m)
		if len(parts) != 4 {
			return m
		}
		if parts[2] == "(empty)" || strings.Contains(parts[2], "****") {
			return m
		}
		return parts[1] + MaskSecret(parts[2]) + parts[3]
	})
	return out
}

// Logger provides thread-safe, rotating file logging with secret redaction.
type Logger struct {
	mu          sync.Mutex
	filePath    string
	file        *os.File
	curSize     int64
	reopenAfter time.Time
	consoleOut  io.Writer
	secrets     []string
	closed      bool
}

var (
	defaultMu     sync.RWMutex
	defaultLogger *Logger
)

// Init creates and sets the default logger writing to logDir/voicetype.log.
// If consoleWriter is non-nil, log lines are also written there.
func Init(logDir string, consoleWriter io.Writer) (*Logger, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	p := filepath.Join(logDir, "voicetype.log")
	l := &Logger{
		filePath:   p,
		consoleOut: consoleWriter,
	}
	if err := l.openFileLocked(); err != nil {
		return nil, err
	}
	defaultMu.Lock()
	defaultLogger = l
	defaultMu.Unlock()
	return l, nil
}

// Default returns the package-level logger (may be nil before Init).
func Default() *Logger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultLogger
}

// RegisterSecret ensures the given raw secret is always redacted if it ever appears in logs.
func (l *Logger) RegisterSecret(secret string) {
	if l == nil {
		return
	}
	secret = strings.TrimSpace(secret)
	if len(secret) < 8 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.secrets {
		if existing == secret {
			return
		}
	}
	l.secrets = append(l.secrets, secret)
}

// FilePath returns the active log file path.
func (l *Logger) FilePath() string {
	if l == nil {
		return ""
	}
	return l.filePath
}

func (l *Logger) openFileLocked() error {
	f, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.file = f
	l.curSize = st.Size()
	return nil
}

func (l *Logger) rotateLocked() {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	// Shift backups: .4 -> .5, ..., .1 -> .2, active -> .1
	for i := maxLogBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.filePath, i)
		dst := fmt.Sprintf("%s.%d", l.filePath, i+1)
		_ = os.Remove(dst)
		_ = os.Rename(src, dst)
	}
	dst1 := l.filePath + ".1"
	_ = os.Remove(dst1)
	_ = os.Rename(l.filePath, dst1)
	_ = l.openFileLocked()
}

func (l *Logger) logf(level, format string, args ...any) {
	if l == nil {
		return
	}
	raw := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	clean := Redact(raw, l.secrets)
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	line := fmt.Sprintf("%s [%s] %s\n", ts, level, clean)

	if l.file == nil && time.Now().After(l.reopenAfter) {
		if err := l.openFileLocked(); err != nil {
			l.reopenAfter = time.Now().Add(5 * time.Second)
		}
	}
	if l.file != nil {
		if l.curSize+int64(len(line)) > maxLogFileSize {
			l.rotateLocked()
		}
		if l.file != nil {
			n, _ := l.file.WriteString(line)
			l.curSize += int64(n)
		} else {
			l.reopenAfter = time.Now().Add(5 * time.Second)
		}
	}
	if l.consoleOut != nil {
		_, _ = io.WriteString(l.consoleOut, line)
	}
}

func (l *Logger) Infof(format string, args ...any)  { l.logf("INFO", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf("WARN", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf("ERROR", format, args...) }
func (l *Logger) Debugf(format string, args ...any) { l.logf("DEBUG", format, args...) }

// Close flushes and closes the underlying log file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

// Package-level convenience helpers.
func Infof(format string, args ...any) {
	if l := Default(); l != nil {
		l.Infof(format, args...)
	}
}

func Warnf(format string, args ...any) {
	if l := Default(); l != nil {
		l.Warnf(format, args...)
	}
}

func Errorf(format string, args ...any) {
	if l := Default(); l != nil {
		l.Errorf(format, args...)
	}
}

func Debugf(format string, args ...any) {
	if l := Default(); l != nil {
		l.Debugf(format, args...)
	}
}
