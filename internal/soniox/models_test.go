package soniox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSelectLatestRealtimeModelAndComparison(t *testing.T) {
	models := []ModelInfo{
		{ID: "stt-async-v7", TranscriptionMode: "async"},
		{ID: "stt-rt-v5", Name: "Speech-to-Text Real-time v5", TranscriptionMode: "real_time"},
		{ID: "stt-rt-v6", Name: "Speech-to-Text Real-time v6", TranscriptionMode: "real_time"},
		{ID: "stt-rt-v7", Name: "Speech-to-Text Real-time v7", TranscriptionMode: "real_time"},
		{ID: "stt-rt-preview", AliasedModelID: "stt-rt-v5", TranscriptionMode: "real_time"},
		{ID: "stt-rt-v99-alias", AliasedModelID: "stt-rt-v5", TranscriptionMode: "real_time"},
	}

	latest, ok := SelectLatestRealtimeModel(models)
	if !ok {
		t.Fatal("expected latest real-time model to be found")
	}
	if latest.ID != "stt-rt-v7" {
		t.Fatalf("latest.ID = %q, want stt-rt-v7", latest.ID)
	}

	if !IsModelOlder("stt-rt-v6", "stt-rt-v7") {
		t.Fatal("stt-rt-v6 should be older than stt-rt-v7")
	}
	if !IsModelOlder("stt-rt-preview", "stt-rt-v7") {
		t.Fatal("stt-rt-preview should be older than stt-rt-v7")
	}
	if IsModelOlder("stt-rt-v7", "stt-rt-v7") {
		t.Fatal("same version should not be older")
	}
	if IsModelOlder("stt-rt-v8", "stt-rt-v7") {
		t.Fatal("newer local version should not be older")
	}
}

func TestFetchModelsEndpoint(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(modelsResponse{
			Models: []ModelInfo{
				{ID: "stt-rt-v6", Name: "Real-time v6", TranscriptionMode: "real_time"},
				{ID: "stt-rt-v7", Name: "Real-time v7", TranscriptionMode: "real_time"},
			},
		})
	}))
	defer srv.Close()

	models, err := FetchModels(context.Background(), "snx_test_token_123", srv.URL)
	if err != nil {
		t.Fatalf("FetchModels failed: %v", err)
	}
	if gotAuth != "Bearer snx_test_token_123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	latest, ok := SelectLatestRealtimeModel(models)
	if !ok || latest.ID != "stt-rt-v7" {
		t.Fatalf("got (%+v, %v), want stt-rt-v7", latest, ok)
	}
}
