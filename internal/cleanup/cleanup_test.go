package cleanup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCleanRejectsTruncatedMaxTokensResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stop_reason": "max_tokens",
			"content":     []map[string]string{{"type": "text", "text": "Truncated output..."}},
		})
	}))
	defer srv.Close()

	orig := "Original complete Greek and English dictation Καλημέρα!"
	got := Clean(context.Background(), orig, Options{
		Enabled:  true,
		APIKey:   "sk-ant-test-key",
		Endpoint: srv.URL,
	})
	if got != orig {
		t.Fatalf("expected raw fallback on max_tokens truncation, got %q", got)
	}
}

func TestCleanAcceptsEndTurnAndBlocksHTTPNonLocalhost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stop_reason": "end_turn",
			"content":     []map[string]string{{"type": "text", "text": "Clean text, Καλημέρα!"}},
		})
	}))
	defer srv.Close()

	got := Clean(context.Background(), "um clean text καλημέρα", Options{
		Enabled:  true,
		APIKey:   "sk-ant-test-key",
		Endpoint: srv.URL,
	})
	if got != "Clean text, Καλημέρα!" {
		t.Fatalf("got %q", got)
	}

	if isAllowedEndpoint("http://evil.example.com/v1/messages") {
		t.Fatal("non-HTTPS remote endpoint must be rejected")
	}
	if !isAllowedEndpoint("https://api.anthropic.com/v1/messages") {
		t.Fatal("HTTPS endpoint should be allowed")
	}
}
