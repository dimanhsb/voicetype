package soniox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultModelsURL is the Soniox REST metadata endpoint returning available models (free, no audio billing).
const DefaultModelsURL = "https://api.soniox.com/v1/models"

var reRealtimeVersion = regexp.MustCompile(`^stt-rt-v(\d+)$`)

// ModelInfo describes a model returned by GET https://api.soniox.com/v1/models.
type ModelInfo struct {
	ID                string `json:"id"`
	AliasedModelID    string `json:"aliased_model_id"`
	Name              string `json:"name"`
	TranscriptionMode string `json:"transcription_mode"`
	ContextVersion    int    `json:"context_version"`
}

type modelsResponse struct {
	Models []ModelInfo `json:"models"`
}

var modelsHTTPClient = &http.Client{
	Timeout: 8 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 {
			orig := via[0].URL
			if !strings.EqualFold(req.URL.Scheme, orig.Scheme) || !strings.EqualFold(req.URL.Host, orig.Host) {
				return fmt.Errorf("blocked cross-origin redirect to %s", req.URL.Host)
			}
		}
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

// FetchModels queries Soniox's GET /v1/models endpoint using the provided API key.
func FetchModels(ctx context.Context, apiKey string, endpoint string) ([]ModelInfo, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("empty Soniox API key")
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultModelsURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := modelsHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET /v1/models returned HTTP %d", resp.StatusCode)
	}

	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse /v1/models JSON: %w", err)
	}
	return parsed.Models, nil
}

// SelectLatestRealtimeModel picks the highest-version canonical (non-alias) real-time STT model
// from a Soniox /v1/models list (e.g. stt-rt-v6, stt-rt-v7...).
func SelectLatestRealtimeModel(models []ModelInfo) (ModelInfo, bool) {
	bestVer := -1
	var best ModelInfo
	var firstCanonical ModelInfo
	hasCanonical := false

	for _, m := range models {
		if m.TranscriptionMode != "real_time" || strings.TrimSpace(m.AliasedModelID) != "" {
			continue
		}
		if !hasCanonical {
			firstCanonical = m
			hasCanonical = true
		}
		if sub := reRealtimeVersion.FindStringSubmatch(strings.TrimSpace(m.ID)); len(sub) == 2 {
			if v, err := strconv.Atoi(sub[1]); err == nil && v > bestVer {
				bestVer = v
				best = m
			}
		}
	}
	if bestVer >= 0 {
		return best, true
	}
	if hasCanonical {
		return firstCanonical, true
	}
	return ModelInfo{}, false
}

// IsModelOlder reports true if currentModel is older than (or an alias behind) latestID.
func IsModelOlder(currentModel, latestID string) bool {
	cur := strings.TrimSpace(currentModel)
	lat := strings.TrimSpace(latestID)
	if lat == "" || cur == lat {
		return false
	}
	curSub := reRealtimeVersion.FindStringSubmatch(cur)
	latSub := reRealtimeVersion.FindStringSubmatch(lat)
	if len(curSub) == 2 && len(latSub) == 2 {
		cv, err1 := strconv.Atoi(curSub[1])
		lv, err2 := strconv.Atoi(latSub[1])
		if err1 == nil && err2 == nil {
			return lv > cv
		}
	}
	// If current is an alias/preview (e.g. "stt-rt-preview") and latest is a canonical "stt-rt-vN"
	return len(latSub) == 2 && strings.HasPrefix(cur, "stt-rt-")
}
