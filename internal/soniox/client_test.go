package soniox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"voicetype/internal/transcript"
)

func TestClientProtocolHeaderConfigBufferingAndTokens(t *testing.T) {
	var gotAuth string
	var gotConfig map[string]any
	var gotAudioBytes int
	var mu sync.Mutex

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// 1. Read start JSON config
		mt, data, err := conn.ReadMessage()
		if err != nil || mt != websocket.TextMessage {
			return
		}
		var cfg map[string]any
		_ = json.Unmarshal(data, &cfg)
		mu.Lock()
		gotConfig = cfg
		mu.Unlock()

		// 2. Read buffered binary audio frames
		for i := 0; i < 3; i++ {
			mt, bin, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				mu.Lock()
				gotAudioBytes += len(bin)
				mu.Unlock()
			}
		}

		// 3. Emit tokens including <end> marker and Greek + English
		resp := wsResponse{
			Tokens: []transcript.Token{
				{Text: "Hello ", IsFinal: true},
				{Text: "Ελλάδα", IsFinal: true},
				{Text: "<end>", IsFinal: true},
				{Text: "!", IsFinal: false},
			},
		}
		b, _ := json.Marshal(resp)
		_ = conn.WriteMessage(websocket.TextMessage, b)

		fin := wsResponse{Finished: true}
		bf, _ := json.Marshal(fin)
		_ = conn.WriteMessage(websocket.TextMessage, bf)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var buf transcript.Buffer
	done := make(chan struct{})
	client := Start(ctx, SessionConfig{
		Endpoint:         wsURL,
		APIKey:           "snx_test_token_123456",
		Model:            "stt-rt-v6",
		LanguageHints:    []string{"en", "el"},
		CustomVocabulary: []string{"VoiceType", "Soniox"},
	}, Callbacks{
		OnTokens: func(tokens []transcript.Token) {
			buf.ApplyTokens(tokens)
		},
		OnFinished: func() {
			close(done)
		},
		OnError: func(err error, fatal bool) {
			t.Errorf("unexpected error (fatal=%v): %v", fatal, err)
		},
	})
	defer client.CloseImmediate()

	// Send audio IMMEDIATELY before WebSocket finishes connecting (tests pre-connect buffering!)
	client.SendAudio(make([]byte, 640))
	client.SendAudio(make([]byte, 640))
	client.SendAudio(make([]byte, 640))

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for mock Soniox server")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer snx_test_token_123456" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
	if gotConfig["model"] != "stt-rt-v6" || gotConfig["audio_format"] != "pcm_s16le" {
		t.Fatalf("unexpected start config: %+v", gotConfig)
	}
	if _, hasInlineKey := gotConfig["api_key"]; hasInlineKey {
		t.Fatalf("deprecated inline api_key should not be sent in JSON payload")
	}
	if gotAudioBytes != 640*3 {
		t.Fatalf("buffered audio bytes = %d, want %d", gotAudioBytes, 640*3)
	}
	if got := buf.FullText(); got != "Hello Ελλάδα!" {
		t.Fatalf("transcript = %q, want %q", got, "Hello Ελλάδα!")
	}
}

func TestAPIErrorFatalClassification(t *testing.T) {
	if !IsFatalError(&APIError{Code: 401, Type: "unauthorized"}) {
		t.Fatal("401 must be fatal")
	}
	if IsFatalError(&APIError{Code: 503, Type: "service_unavailable"}) {
		t.Fatal("503 should be transient (reconnectable)")
	}
}
