package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

func saveModelSettingTest(t *testing.T, s *Service, a Actor, body Data) Data {
	t.Helper()
	return mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveModelSettings(context.Background(), tx, a, body)
	})
}

func modelSettingSaveError(s *Service, a Actor, body Data) error {
	_, _, err := s.Mutate(context.Background(), a, ID(), ID(), func(tx pgx.Tx) (Data, int, error) {
		data, err := s.SaveModelSettings(context.Background(), tx, a, body)
		return data, 200, err
	})
	return err
}

func TestModelSettingsSecretsVersionsAndPermissions(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	initial, err := s.GetModelSettings(ctx, a)
	if err != nil || Num(initial, "version") != 0 || Bool(initial, "enabled") || Num(initial, "max_daily_tokens") != defaultDailyTokens {
		t.Fatalf("unexpected empty settings: %#v, %v", initial, err)
	}
	if model, err := s.CurrentModel(ctx); err != nil || model != nil {
		t.Fatalf("unconfigured model: %v", err)
	}
	if _, err = s.GetModelSettings(ctx, Actor{}); err != Forbidden {
		t.Fatal("anonymous settings read accepted", err)
	}
	viewer := Actor{ID: ID(), Role: "viewer"}
	if _, err = s.GetModelSettings(ctx, viewer); err != nil {
		t.Fatal("authenticated settings read denied", err)
	}
	if err = modelSettingSaveError(s, viewer, Data{"expected_version": 0}); err != Forbidden {
		t.Fatal("viewer changed settings", err)
	}
	const secret = "model-secret-do-not-persist-plaintext"
	config := saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true,
		"base_url": "https://model.example.test/v1", "model_name": "example-model", "api_key": secret})
	if Num(config, "version") != 1 || !Bool(config, "api_key_configured") || Str(config, "updated_by") != a.ID {
		t.Fatal("save metadata missing", config)
	}
	if strings.Contains(string(JSON(config)), secret) || config["api_key"] != nil || config["secret"] != nil {
		t.Fatal("public model response leaked credential")
	}
	var encrypted string
	if err = s.DB.QueryRow(ctx, "SELECT secret FROM system_settings WHERE name='model'").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if encrypted == "" || strings.Contains(encrypted, secret) {
		t.Fatal("key not encrypted")
	}
	plain, err := s.Vault.Open(encrypted, []byte("system_settings:model"))
	if err != nil || !strings.Contains(string(plain), secret) {
		t.Fatal("credential did not roundtrip with model AAD", err)
	}
	if _, err = s.Vault.Open(encrypted, []byte("system_settings:execution")); err == nil {
		t.Fatal("credential accepted under a different AAD")
	}
	private, err := s.RunnerModelSettings(ctx)
	if err != nil || Str(private, "api_key") != secret {
		t.Fatal("internal settings missing key", err)
	}
	config = saveModelSettingTest(t, s, a, Data{"expected_version": 1, "api_key": "   ", "model_name": "updated-model"})
	var retained string
	if err = s.DB.QueryRow(ctx, "SELECT secret FROM system_settings WHERE name='model'").Scan(&retained); err != nil || retained != encrypted {
		t.Fatal("blank key did not preserve encrypted credential", err)
	}
	if err = modelSettingSaveError(s, a, Data{"expected_version": 1, "enabled": false}); err != Conflict {
		t.Fatal("stale model update accepted", err)
	}
	if err = modelSettingSaveError(s, a, Data{"expected_version": 2, "base_url": "https://other.example.test/v1"}); err == nil {
		t.Fatal("existing key silently reused at a new destination")
	}
	if err = modelSettingSaveError(s, a, Data{"expected_version": 2, "api_key": "new", "clear_api_key": true}); err == nil {
		t.Fatal("ambiguous key update accepted")
	}
	config = saveModelSettingTest(t, s, a, Data{"expected_version": 2, "base_url": "https://other.example.test/v1", "clear_api_key": true})
	if Bool(config, "api_key_configured") || Num(config, "version") != 3 {
		t.Fatal("explicit clear failed", config)
	}
	config = saveModelSettingTest(t, s, a, Data{"expected_version": 3, "api_key": secret})
	config = saveModelSettingTest(t, s, a, Data{"expected_version": 4, "enabled": false})
	private, err = s.RunnerModelSettings(ctx)
	if err != nil || private["api_key"] != nil || !Bool(private, "api_key_configured") {
		t.Fatal("disabled settings exposed or removed key", err)
	}
	if model, err := s.CurrentModel(ctx); err != nil || model != nil {
		t.Fatal("disabled model remained usable", err)
	}
	if version, err := s.ModelSettingsVersion(ctx); err != nil || version != Num(config, "version") {
		t.Fatal("model version differs", version, err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM system_settings WHERE data::text LIKE '%'||$1||'%'", secret); n != 0 {
		t.Fatal("secret stored in public configuration")
	}
	if n := supportCount(t, s, "SELECT count(*) FROM audit_events WHERE detail LIKE '%'||$1||'%' OR metadata::text LIKE '%'||$1||'%'", secret); n != 0 {
		t.Fatal("secret stored in audit")
	}
	if n := supportCount(t, s, "SELECT count(*) FROM idempotency_keys WHERE response::text LIKE '%'||$1||'%'", secret); n != 0 {
		t.Fatal("secret stored in idempotency response")
	}
}

func TestModelSettingsConcurrentCreateAndBudgetUpdates(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	start := make(chan struct{})
	out := make(chan error, 2)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			out <- modelSettingSaveError(s, a, Data{"expected_version": 0, "max_daily_tokens": 100})
		}()
	}
	close(start)
	workers.Wait()
	close(out)
	success, conflict := 0, 0
	for err := range out {
		switch err {
		case nil:
			success++
		case Conflict:
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent creation: %d successes, %d conflicts", success, conflict)
	}
	if _, err := s.ReserveBudget(ctx, "first", 60); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveBudget(ctx, "over", 41); err == nil {
		t.Fatal("DB daily budget was ignored")
	}
	saveModelSettingTest(t, s, a, Data{"expected_version": 1, "max_daily_tokens": 200})
	if _, err := s.ReserveBudget(ctx, "after-increase", 100); err != nil {
		t.Fatal("new budget was not read immediately", err)
	}
	saveModelSettingTest(t, s, a, Data{"expected_version": 2, "max_daily_tokens": 1})
	if _, err := s.ReserveBudget(ctx, "after-decrease", 1); err == nil {
		t.Fatal("budget decrease was not respected")
	}
}

func TestModelSettingsCurrentClientUsesLatestDatabaseConfiguration(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	type observed struct {
		path, authorization string
		body                Data
	}
	requests := make(chan observed, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body Data
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		requests <- observed{r.URL.Path, r.Header.Get("Authorization"), body}
		supportModelResponse(w, Data{"ok": true})
	}))
	defer server.Close()
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "allow_http": true,
		"base_url": server.URL + "/first", "model_name": "first-model", "api_key": "first-key"})
	complete := func() observed {
		t.Helper()
		model, err := s.CurrentModel(ctx)
		if err != nil || model == nil {
			t.Fatal("model unavailable", err)
		}
		_, err = model.Complete(ctx, integration.ChatRequest{Messages: []integration.ChatMessage{{Role: "user", Content: "test"}},
			MaxTokens: 23, ResponseSchema: &integration.JSONSchema{Name: "result", Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)}})
		if err != nil {
			t.Fatal(err)
		}
		return <-requests
	}
	first := complete()
	if first.path != "/first/chat/completions" || first.authorization != "Bearer first-key" ||
		Str(first.body, "model") != "first-model" || Num(first.body, "max_tokens") != 23 ||
		Str(Object(first.body, "response_format"), "type") != "json_schema" {
		t.Fatal("initial model settings not applied")
	}
	saveModelSettingTest(t, s, a, Data{"expected_version": 1, "base_url": server.URL + "/second",
		"model_name": "second-model", "api_key": "second-key", "json_mode": "json_object", "max_tokens_field": "max_completion_tokens"})
	second := complete()
	if second.path != "/second/chat/completions" || second.authorization != "Bearer second-key" ||
		Str(second.body, "model") != "second-model" || Num(second.body, "max_completion_tokens") != 23 ||
		second.body["max_tokens"] != nil || Str(Object(second.body, "response_format"), "type") != "json_object" {
		t.Fatal("changed model settings not applied")
	}
	if s.Model != nil {
		t.Fatal("production stored a mutable shared model client")
	}
	saveModelSettingTest(t, s, a, Data{"expected_version": 2, "enabled": false})
	if model, err := s.CurrentModel(ctx); err != nil || model != nil {
		t.Fatal("disabled settings allowed another model request", err)
	}
}

func TestModelSettingsValidation(t *testing.T) {
	for name, update := range map[string]Data{
		"enabled without endpoint": {"enabled": true},
		"HTTP without opt-in":      {"base_url": "http://localhost/v1"},
		"URL credentials":          {"base_url": "https://key@example.test/v1"},
		"URL query":                {"base_url": "https://example.test/v1?api_key=private"},
		"URL empty query":          {"base_url": "https://example.test/v1?"},
		"URL fragment":             {"base_url": "https://example.test/v1#key"},
		"JSON mode":                {"json_mode": "text"},
		"token parameter":          {"max_tokens_field": "other"},
		"zero budget":              {"max_daily_tokens": 0},
		"fraction budget":          {"max_daily_tokens": 1.5},
		"oversized budget":         {"max_daily_tokens": int64(1 << 60)},
		"budget above API bound":   {"max_daily_tokens": int64(1000000001)},
		"model name above bound":   {"model_name": strings.Repeat("x", 201)},
	} {
		t.Run(name, func(t *testing.T) {
			settings := defaultModelSettings()
			for k, v := range update {
				settings[k] = v
			}
			if err := validateModelSettings(settings); err == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
	if err := validateModelSettings(defaultModelSettings()); err != nil {
		t.Fatal("disabled draft should be valid", err)
	}
}
