package service

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

// Opt-in test for the local Postman simulation; never sends data to a real model.
func TestModelSettingsPostmanSimulation(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("AGENTOWL_POSTMAN_MODEL_URL"), "/")
	if baseURL == "" {
		t.Skip("AGENTOWL_POSTMAN_MODEL_URL enables the local Postman simulation")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" || !contains([]string{"127.0.0.1", "localhost", "::1"}, u.Hostname()) ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/model/v1" {
		t.Fatal("simulation URL must be an HTTP loopback address with /model/v1")
	}
	s, a := testService(t)
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": baseURL,
		"allow_http": true, "model_name": "agentowl-simulation", "api_key": "simulation-key-only"})
	model, err := s.CurrentModel(context.Background())
	if err != nil || model == nil || s.Model != nil {
		t.Fatal("database-backed model unavailable", err)
	}
	result, usage, err := model.Classify(context.Background(), "How do I log in with Lark CLI?")
	if err != nil {
		t.Fatal("Postman classification failed", err)
	}
	if result.SupportRelevance != "in_scope" || result.Type != "question" || result.Module != "auth" ||
		result.NeedsHuman || usage.TotalTokens != 88 || usage.PromptTokens != 64 || usage.CompletionTokens != 24 {
		t.Fatalf("unexpected simulation result: %#v, %#v", result, usage)
	}
}
