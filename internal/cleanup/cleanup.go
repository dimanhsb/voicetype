package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"voicetype/internal/logging"
)

const (
	defaultEndpoint = "https://api.anthropic.com/v1/messages"
	defaultModel    = "claude-haiku-4-5"
	requestTimeout  = 4 * time.Second
)

// noCrossOriginClient prevents leaking x-api-key across cross-origin redirects.
var noCrossOriginClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		orig := via[0].URL
		if !strings.EqualFold(req.URL.Scheme, orig.Scheme) || !strings.EqualFold(req.URL.Host, orig.Host) {
			return fmt.Errorf("blocked cross-origin redirect to %s", req.URL.Host)
		}
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

const systemPrompt = `You clean up raw voice-dictation transcripts before insertion.
Rules:
1. Remove filler words (um, uh, like, εεε, μμμ) and false starts/stutters.
2. Fix punctuation and capitalization naturally.
3. Preserve the exact mix of Greek and English words as spoken — NEVER translate between Greek and English.
4. Keep the speaker's meaning, tone, names, and technical terms intact.
5. Output ONLY the cleaned text with no quotes, markdown, or commentary.`

type Options struct {
	Enabled  bool
	APIKey   string
	Model    string
	Endpoint string
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func isAllowedEndpoint(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Scheme, "https") {
		return u.Host != ""
	}
	if strings.EqualFold(u.Scheme, "http") {
		h := strings.ToLower(u.Hostname())
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

// Clean optionally polishes dictated text using Claude when enabled and configured.
// On any error, timeout, or truncated response (stop_reason != "end_turn"), it returns
// the original text unchanged so dictation is never blocked, truncated, or lost.
func Clean(ctx context.Context, text string, opts Options) string {
	trimmed := strings.TrimSpace(text)
	if !opts.Enabled || trimmed == "" || strings.TrimSpace(opts.APIKey) == "" {
		return text
	}
	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if !isAllowedEndpoint(endpoint) {
		logging.Warnf("cleanup: refusing non-HTTPS remote endpoint %q; using raw text", endpoint)
		return text
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = defaultModel
	}

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	payload := anthropicRequest{
		Model:     model,
		MaxTokens: 4096,
		System:    systemPrompt,
		Messages: []anthropicMessage{
			{Role: "user", Content: trimmed},
		},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return text
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return text
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", strings.TrimSpace(opts.APIKey))
	req.Header.Set("anthropic-version", "2023-06-01")

	start := time.Now()
	resp, err := noCrossOriginClient.Do(req)
	if err != nil {
		logging.Warnf("cleanup: request failed after %v (%v); using raw text", time.Since(start), err)
		return text
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return text
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logging.Warnf("cleanup: HTTP %d from %s; using raw text", resp.StatusCode, endpoint)
		return text
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		logging.Warnf("cleanup: invalid JSON response (%v); using raw text", err)
		return text
	}
	if parsed.Error != nil {
		logging.Warnf("cleanup: API error %s: %s; using raw text", parsed.Error.Type, parsed.Error.Message)
		return text
	}
	if parsed.StopReason != "" && parsed.StopReason != "end_turn" {
		logging.Warnf("cleanup: incomplete response (stop_reason=%q); keeping raw text to prevent truncation", parsed.StopReason)
		return text
	}

	var out strings.Builder
	for _, blk := range parsed.Content {
		if blk.Type == "text" {
			out.WriteString(blk.Text)
		}
	}
	cleaned := strings.TrimSpace(out.String())
	if cleaned == "" {
		return text
	}
	logging.Infof("cleanup: polished %d -> %d runes in %v (%s)", len([]rune(trimmed)), len([]rune(cleaned)), time.Since(start), model)
	return cleaned
}

var _ = fmt.Sprintf
