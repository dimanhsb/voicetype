package soniox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"voicetype/internal/audio"
	"voicetype/internal/logging"
	"voicetype/internal/transcript"
)

const (
	DefaultEndpoint = "wss://stt-rt.soniox.com/transcribe-websocket"
	keepalivePeriod = 10 * time.Second
	writeTimeout    = 5 * time.Second
	readTimeout     = 40 * time.Second
	dialTimeout     = 10 * time.Second
)

// SessionConfig configures a Soniox real-time WebSocket transcription session.
type SessionConfig struct {
	Endpoint            string
	APIKey              string
	Model               string
	LanguageHints       []string
	LanguageHintsStrict bool
	CustomVocabulary    []string
	ContextText         string
}

type startPayload struct {
	Model                        string          `json:"model"`
	AudioFormat                  string          `json:"audio_format"`
	SampleRate                   int             `json:"sample_rate"`
	NumChannels                  int             `json:"num_channels"`
	LanguageHints                []string        `json:"language_hints,omitempty"`
	LanguageHintsStrict          bool            `json:"language_hints_strict,omitempty"`
	EnableEndpointDetection      bool            `json:"enable_endpoint_detection"`
	EnableLanguageIdentification bool            `json:"enable_language_identification"`
	Context                      *contextPayload `json:"context,omitempty"`
}

type contextPayload struct {
	Text  string   `json:"text,omitempty"`
	Terms []string `json:"terms,omitempty"`
}

type wsResponse struct {
	Tokens           []transcript.Token `json:"tokens"`
	FinalAudioProcMs int64              `json:"final_audio_proc_ms"`
	TotalAudioProcMs int64              `json:"total_audio_proc_ms"`
	Finished         bool               `json:"finished"`
	ErrorCode        int                `json:"error_code"`
	ErrorType        string             `json:"error_type"`
	ErrorMessage     string             `json:"error_message"`
	RequestID        string             `json:"request_id"`
}

// APIError represents a structured error returned by Soniox over WebSocket or HTTP upgrade.
type APIError struct {
	Code      int
	Type      string
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("Soniox error %d (%s): %s", e.Code, e.Type, e.Message)
	}
	return fmt.Sprintf("Soniox error %d: %s", e.Code, e.Message)
}

// IsFatal reports whether this error should stop automatic reconnection (e.g. auth/config errors).
func (e *APIError) IsFatal() bool {
	switch e.Code {
	case 400, 401, 402, 403, 404, 422:
		return true
	}
	switch strings.ToLower(e.Type) {
	case "invalid_api_key", "unauthorized", "authentication_error", "invalid_request", "model_not_available", "insufficient_credits":
		return true
	}
	return false
}

// IsFatalError checks whether err is a non-recoverable Soniox API error.
func IsFatalError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsFatal()
	}
	return false
}

// Callbacks receives asynchronous events from an active Soniox WebSocket session.
type Callbacks struct {
	OnConnected func()
	OnTokens    func(tokens []transcript.Token)
	OnFinished  func()
	OnError     func(err error, fatal bool)
}

type writeItem struct {
	msgType int
	data    []byte
}

// Client manages a single Soniox real-time WebSocket stream.
// Audio can be queued immediately before Dial completes; once connected, the backlog
// drains in order with zero lost speech.
type Client struct {
	cfg       SessionConfig
	cb        Callbacks
	writeCh   chan writeItem
	doneCh    chan struct{}
	closeOnce sync.Once
	errOnce   sync.Once

	connMu sync.Mutex
	conn   *websocket.Conn
}

func (c *Client) reportError(err error, fatal bool) {
	if c.isClosed() {
		return
	}
	c.errOnce.Do(func() {
		if c.cb.OnError != nil {
			c.cb.OnError(err, fatal)
		}
	})
}

// Start creates and launches a Soniox WebSocket client dialing in the background.
// Call SendAudio immediately after Start; audio buffers safely until the socket connects.
func Start(ctx context.Context, cfg SessionConfig, cb Callbacks) *Client {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.Model == "" {
		cfg.Model = "stt-rt-v6"
	}
	c := &Client{
		cfg: cfg,
		cb:  cb,
		// Buffer up to ~16 seconds of 20ms frames (800 chunks) while connecting or reconnecting!
		writeCh: make(chan writeItem, 800),
		doneCh:  make(chan struct{}),
	}
	go c.run(ctx)
	return c
}

// SendAudio enqueues a 16 kHz mono s16le PCM chunk for streaming. Non-blocking;
// drops oldest only if the 16-second buffer overflows.
func (c *Client) SendAudio(pcm16 []byte) {
	if len(pcm16) == 0 {
		return
	}
	cp := make([]byte, len(pcm16))
	copy(cp, pcm16)
	item := writeItem{msgType: websocket.BinaryMessage, data: cp}
	select {
	case <-c.doneCh:
		return
	case c.writeCh <- item:
	default:
		// Drop oldest frame to keep latency bounded if completely stalled
		select {
		case <-c.writeCh:
		default:
		}
		select {
		case c.writeCh <- item:
		default:
		}
	}
}

// Finalize sends the Soniox manual finalization control message (used for WAV self-test).
func (c *Client) Finalize() {
	item := writeItem{msgType: websocket.TextMessage, data: []byte(`{"type":"finalize"}`)}
	select {
	case <-c.doneCh:
	case c.writeCh <- item:
	}
}

// SendEOF sends an empty text frame signaling end-of-audio so Soniox returns finished: true.
func (c *Client) SendEOF() {
	item := writeItem{msgType: websocket.TextMessage, data: []byte{}}
	select {
	case <-c.doneCh:
	case c.writeCh <- item:
	}
}

// DrainUnsent returns any binary audio frames still queued in this client that were never
// written to the WebSocket (e.g. when a dial attempt fails), preserving speech across retries.
func (c *Client) DrainUnsent() [][]byte {
	var out [][]byte
	for {
		select {
		case item := <-c.writeCh:
			if item.msgType == websocket.BinaryMessage && len(item.data) > 0 {
				out = append(out, item.data)
			}
		default:
			return out
		}
	}
}

// CloseImmediate terminates the WebSocket right away without waiting for finished: true.
// This ensures instant submission latency.
func (c *Client) CloseImmediate() {
	c.closeOnce.Do(func() {
		close(c.doneCh)
		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})
}

func (c *Client) buildStartJSON() ([]byte, error) {
	p := startPayload{
		Model:                        c.cfg.Model,
		AudioFormat:                  "pcm_s16le",
		SampleRate:                   audio.TargetSampleRate,
		NumChannels:                  audio.TargetChannels,
		LanguageHints:                c.cfg.LanguageHints,
		LanguageHintsStrict:          c.cfg.LanguageHintsStrict,
		EnableEndpointDetection:      true,
		EnableLanguageIdentification: true,
	}
	// Clean vocabulary terms
	var cleanTerms []string
	for _, t := range c.cfg.CustomVocabulary {
		if s := strings.TrimSpace(t); s != "" {
			cleanTerms = append(cleanTerms, s)
		}
	}
	ctxText := strings.TrimSpace(c.cfg.ContextText)
	if len(cleanTerms) > 0 || ctxText != "" {
		p.Context = &contextPayload{
			Text:  ctxText,
			Terms: cleanTerms,
		}
	}
	return json.Marshal(p)
}

func (c *Client) run(ctx context.Context) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	headers := http.Header{}
	if strings.TrimSpace(c.cfg.APIKey) != "" {
		headers.Set("Authorization", "Bearer "+strings.TrimSpace(c.cfg.APIKey))
	}

	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: dialTimeout,
	}

	logging.Infof("soniox: dialing %s (model=%s, langs=%v, vocab=%d)",
		c.cfg.Endpoint, c.cfg.Model, c.cfg.LanguageHints, len(c.cfg.CustomVocabulary))

	conn, resp, err := dialer.DialContext(dialCtx, c.cfg.Endpoint, headers)
	if err != nil {
		var apiErr error = err
		if resp != nil {
			apiErr = &APIError{
				Code:    resp.StatusCode,
				Message: fmt.Sprintf("WebSocket upgrade failed (%s): %v", resp.Status, err),
			}
		}
		if c.isClosed() {
			return
		}
		logging.Errorf("soniox: dial failed: %v", apiErr)
		if c.cb.OnError != nil {
			c.cb.OnError(apiErr, IsFatalError(apiErr))
		}
		return
	}

	// If closed while dialing, tear down cleanly
	if c.isClosed() {
		_ = conn.Close()
		return
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	defer func() {
		_ = conn.Close()
	}()

	// Send initial JSON config frame
	startBytes, err := c.buildStartJSON()
	if err != nil {
		if c.cb.OnError != nil {
			c.cb.OnError(err, true)
		}
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, startBytes); err != nil {
		if !c.isClosed() && c.cb.OnError != nil {
			c.cb.OnError(fmt.Errorf("send start config: %w", err), false)
		}
		return
	}

	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

	logging.Infof("soniox: connected and configured (queued backlog=%d chunks)", len(c.writeCh))
	if c.cb.OnConnected != nil {
		c.cb.OnConnected()
	}

	// Start dedicated writer goroutine (drains backlog, streams live audio, sends keepalives)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop(ctx, conn)
	}()

	// Read transcription responses until finished, error, or CloseImmediate
	c.readLoop(conn)

	// Ensure writer exits cleanly
	c.CloseImmediate()
	<-writerDone
}

func (c *Client) isClosed() bool {
	select {
	case <-c.doneCh:
		return true
	default:
		return false
	}
}

func (c *Client) writeLoop(ctx context.Context, conn *websocket.Conn) {
	keepaliveTicker := time.NewTicker(keepalivePeriod)
	defer keepaliveTicker.Stop()

	lastSend := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.doneCh:
			return
		case item := <-c.writeCh:
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := conn.WriteMessage(item.msgType, item.data); err != nil {
				if !c.isClosed() {
					logging.Warnf("soniox: write failed: %v", err)
					// Close conn to unblock readLoop immediately and route error once!
					_ = conn.Close()
					c.reportError(fmt.Errorf("write failed: %w", err), false)
				}
				return
			}
			lastSend = time.Now()
		case <-keepaliveTicker.C:
			if time.Since(lastSend) >= keepalivePeriod-time.Second {
				_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"keepalive"}`)); err != nil {
					if !c.isClosed() {
						logging.Warnf("soniox: keepalive failed: %v", err)
						_ = conn.Close()
						c.reportError(fmt.Errorf("keepalive failed: %w", err), false)
					}
					return
				}
				lastSend = time.Now()
			}
		}
	}
}

func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		_, data, err := conn.ReadMessage()
		if err != nil {
			if c.isClosed() {
				return
			}
			logging.Warnf("soniox: read closed: %v", err)
			c.reportError(err, false)
			return
		}
		var resp wsResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			logging.Warnf("soniox: invalid JSON response: %v", err)
			continue
		}
		if resp.ErrorCode != 0 || resp.ErrorMessage != "" {
			apiErr := &APIError{
				Code:      resp.ErrorCode,
				Type:      resp.ErrorType,
				Message:   resp.ErrorMessage,
				RequestID: resp.RequestID,
			}
			logging.Errorf("soniox: %v (req=%s)", apiErr, resp.RequestID)
			c.reportError(apiErr, apiErr.IsFatal())
			return
		}
		if len(resp.Tokens) > 0 && c.cb.OnTokens != nil {
			c.cb.OnTokens(resp.Tokens)
		}
		if resp.Finished {
			logging.Infof("soniox: stream finished (final_ms=%d total_ms=%d)", resp.FinalAudioProcMs, resp.TotalAudioProcMs)
			if c.cb.OnFinished != nil {
				c.cb.OnFinished()
			}
			return
		}
	}
}

// SelfTestResult holds the outcome of streaming a WAV file to Soniox at real-time speed.
type SelfTestResult struct {
	Transcript     string
	Duration       time.Duration
	AudioDuration  time.Duration
	ConnectLatency time.Duration
	FirstTokenWait time.Duration
	ChunksSent     int
}

// StreamWAVSelfTest reads wavPath, resamples it through VoiceType's real audio resampler
// to 16 kHz mono pcm_s16le, streams it in 20 ms chunks at real-time speed to Soniox,
// and returns the transcribed text and timing telemetry.
func StreamWAVSelfTest(ctx context.Context, cfg SessionConfig, wavPath string) (*SelfTestResult, error) {
	wav, err := audio.ReadWAV(wavPath)
	if err != nil {
		return nil, err
	}
	resampler := audio.NewResampler(wav.Format)
	pcm16, _ := resampler.Process(wav.Data, 100)
	if tail := resampler.Flush(100); len(tail) > 0 {
		pcm16 = append(pcm16, tail...)
	}
	var chunker audio.Chunker
	chunks := chunker.Push(pcm16)
	if rem := chunker.Flush(); len(rem) > 0 {
		chunks = append(chunks, rem)
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("wav file %s produced 0 PCM chunks", wavPath)
	}

	var bufMu sync.Mutex
	var buf transcript.Buffer
	startAt := time.Now()
	var connectLat, firstTokLat time.Duration
	var gotFirstTok bool

	doneCh := make(chan error, 1)
	client := Start(ctx, cfg, Callbacks{
		OnConnected: func() {
			connectLat = time.Since(startAt)
		},
		OnTokens: func(tokens []transcript.Token) {
			bufMu.Lock()
			if !gotFirstTok {
				gotFirstTok = true
				firstTokLat = time.Since(startAt)
			}
			buf.ApplyTokens(tokens)
			bufMu.Unlock()
		},
		OnFinished: func() {
			select {
			case doneCh <- nil:
			default:
			}
		},
		OnError: func(err error, _ bool) {
			select {
			case doneCh <- err:
			default:
			}
		},
	})
	defer client.CloseImmediate()

	// Stream at real-time speed (20 ms per chunk), starting immediately (verifies pre-connect buffering too!)
	ticker := time.NewTicker(time.Duration(audio.ChunkDurationMs) * time.Millisecond)
	defer ticker.Stop()
	for i, ch := range chunks {
		client.SendAudio(ch)
		if i < len(chunks)-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case err := <-doneCh:
				if err != nil {
					return nil, err
				}
			case <-ticker.C:
			}
		}
	}

	// Signal end-of-stream so Soniox finalizes cleanly
	client.Finalize()
	client.SendEOF()

	var waitErr error
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-doneCh:
		if err != nil {
			return nil, err
		}
	case <-time.After(10 * time.Second):
		waitErr = fmt.Errorf("timed out waiting for Soniox finished=true")
	}

	bufMu.Lock()
	text := strings.TrimSpace(buf.FullText())
	bufMu.Unlock()

	res := &SelfTestResult{
		Transcript:     text,
		Duration:       time.Since(startAt),
		AudioDuration:  time.Duration(len(chunks)*audio.ChunkDurationMs) * time.Millisecond,
		ConnectLatency: connectLat,
		FirstTokenWait: firstTokLat,
		ChunksSent:     len(chunks),
	}
	if waitErr != nil {
		return res, waitErr
	}
	return res, nil
}
