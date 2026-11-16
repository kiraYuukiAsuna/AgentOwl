package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestModelRequestSizeIsIndependentOfResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size, err := io.Copy(io.Discard, r.Body)
		if err != nil || size <= 4<<20 {
			t.Errorf("large request was not delivered: %d, %v", size, err)
		}
		writeJSON(t, w, chatFixture("ok", nil, "stop"))
	}))
	defer server.Close()
	request := ChatRequest{Messages: []ChatMessage{{Role: "user", Content: strings.Repeat("x", 5<<20)}}, MaxTokens: 64}
	for _, limit := range []int64{1024, 1} {
		model, err := NewModelClient(ModelConfig{BaseURL: server.URL, Model: "test", MaxResponseBytes: limit})
		if err != nil {
			t.Fatal(err)
		}
		response, err := model.Complete(context.Background(), request)
		if limit == 1 {
			if !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("response protection lost: %v", err)
			}
		} else if err != nil || response.Message.Content != "ok" {
			t.Fatalf("large request rejected: %v, %v", response, err)
		}
	}
}

func TestModelRequestBudgetPostmanSimulation(t *testing.T) {
	base := strings.TrimRight(os.Getenv("AGENTOWL_POSTMAN_MODEL_URL"), "/")
	if base == "" {
		t.Skip("AGENTOWL_POSTMAN_MODEL_URL enables the local Postman simulation")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/model/v1" {
		t.Fatal("simulation requires an HTTP loopback URL ending in /model/v1")
	}
	model, err := NewModelClient(ModelConfig{BaseURL: base, Model: "agentowl-unlimited-simulation", MaxResponseBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Complete(context.Background(), ChatRequest{Messages: []ChatMessage{{Role: "user", Content: strings.Repeat("x", 8<<10)}}, MaxTokens: 64})
	if err != nil || response.Message.Content != "large request received" {
		t.Fatalf("Postman large-request probe failed: %v, %v", response, err)
	}
}
