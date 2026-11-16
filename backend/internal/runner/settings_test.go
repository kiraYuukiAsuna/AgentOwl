package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentowl/backend/internal/integration"
)

type mutableSettings struct {
	mu    sync.Mutex
	value Settings
	err   error
}

func (p *mutableSettings) Settings(context.Context) (Settings, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.value, p.err
}

func (p *mutableSettings) set(value Settings, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.value, p.err = value, err
}

func configuredSettings() Settings {
	return Settings{ExecutionProfile: DefaultExecutionProfile(), ExecutionVersion: 1, ResourceLimits: ResourceLimitsRequired, MaxConcurrent: 1, Model: ModelSettings{Enabled: true, Version: 1, BaseURL: "https://model.invalid/v1", ModelName: "model-one", APIKey: "private-model-key-one", JSONMode: "json_schema", MaxTokensField: "max_tokens", MaxDailyTokens: 1000000}}
}

func configuredSpec(stage string, settings Settings) RunSpec {
	spec := specFor(stage)
	spec.ExecutionVersion, spec.ModelVersion, spec.ResourceLimits = settings.ExecutionVersion, settings.Model.Version, settings.ResourceLimits
	profile := settings.ExecutionProfile
	spec.ExecutionProfile = &profile
	return spec
}

func TestSettingsCallbackAuthenticationAndSecretSafeErrors(t *testing.T) {
	want := configuredSettings()
	const token = "internal-callback-token"
	var malformed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/runner/settings" || r.URL.Query().Get("machine_id") != "machine-one" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("unexpected settings callback: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if malformed.Load() {
			_, _ = w.Write([]byte(`{"model":{"api_key":"private-model-key-one"},"unexpected":"private-model-key-one"}`))
			return
		}
		writeJSON(w, http.StatusOK, want)
	}))
	defer server.Close()
	provider := NewHTTPAuthorizer(server.URL, token, "machine-one", server.Client())
	got, err := provider.Settings(context.Background())
	if err != nil || got.Model.APIKey != want.Model.APIKey || got.ExecutionVersion != want.ExecutionVersion {
		t.Fatalf("settings response not decoded: err=%v", err)
	}
	malformed.Store(true)
	if _, err := provider.Settings(context.Background()); err == nil || strings.Contains(err.Error(), want.Model.APIKey) {
		t.Fatalf("invalid response accepted or exposed credential: %v", err)
	}
}

func TestNewRunsRequireCurrentSettingsWithoutFallback(t *testing.T) {
	for _, name := range []string{"unavailable", "missing runner settings", "invalid mode", "invalid concurrency", "missing model", "disabled model", "invalid model", "stale execution", "stale model", "wrong resource mode"} {
		t.Run(name, func(t *testing.T) {
			current := configuredSettings()
			spec := configuredSpec("test", current)
			provider := &mutableSettings{value: current}
			switch name {
			case "unavailable":
				provider.err = errors.New("upstream response with private-model-key-one")
			case "missing runner settings":
				provider.value.ExecutionVersion = 0
			case "invalid mode":
				provider.value.ResourceLimits = "off"
			case "invalid concurrency":
				provider.value.ResourceLimits, provider.value.MaxConcurrent = ResourceLimitsDisabled, 2
			case "missing model":
				provider.value.Model = ModelSettings{}
			case "disabled model":
				provider.value.Model.Enabled = false
			case "invalid model":
				provider.value.Model.BaseURL = "https://user:private-model-key-one@model.invalid"
			case "stale execution":
				provider.value.ExecutionVersion++
			case "stale model":
				provider.value.Model.Version++
			case "wrong resource mode":
				spec.ResourceLimits = ResourceLimitsDisabled
			}
			var fallbackCalls atomic.Int64
			fallback := modelFunc(func(context.Context, integration.ChatRequest) (integration.ChatResponse, error) {
				fallbackCalls.Add(1)
				return integration.ChatResponse{}, nil
			})
			c, fake, auth := fixtureConfig(t, fallback, Config{SettingsProvider: provider})
			if _, err := c.Start(context.Background(), spec); err == nil || strings.Contains(err.Error(), current.Model.APIKey) {
				t.Fatalf("unsafe settings admitted or leaked: %v", err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.creates != 0 || auth.calls.Load() != 0 || fallbackCalls.Load() != 0 {
				t.Fatal("invalid settings reached authorization, Docker, or fallback model")
			}
		})
	}
}

func TestCapabilitiesStayQueryableWhenRequiredLimitsUnsupported(t *testing.T) {
	provider := &mutableSettings{value: configuredSettings()}
	c, fake, _ := fixtureConfig(t, nil, Config{SettingsProvider: provider})
	fake.info.CgroupVersion, fake.info.CgroupDriver = "1", "none"
	fake.info.MemoryLimit, fake.info.CPUQuota, fake.info.PidsLimit = false, false, false
	c.preflight = func(_ context.Context, mode string) error {
		return validateInfo(fake.info, c.cfg.HostDataRoot, mode)
	}
	if err := c.Recover(context.Background()); err != nil {
		t.Fatalf("recovery blocked capability access: %v", err)
	}
	token := strings.Repeat("t", 32)
	handler, err := NewHandler(c, token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := NewClient(server.URL, token, server.Client())
	caps, err := client.Capabilities(context.Background())
	if err != nil || !caps.Reachable || !caps.Rootless || !caps.FilesystemReady || caps.SupportsResourceLimits || caps.Ready || caps.ResourceLimits != ResourceLimitsRequired || caps.Error == "" {
		t.Fatalf("wrong unsupported capabilities: %+v err=%v", caps, err)
	}
	encoded, _ := json.Marshal(caps)
	for _, private := range []string{provider.value.Model.APIKey, provider.value.Model.BaseURL, c.cfg.DataRoot} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("capabilities leaked private configuration")
		}
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("capabilities endpoint has no authentication")
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if health.Code != http.StatusServiceUnavailable {
		t.Fatal("unsupported required limits reported ready")
	}

	updated := provider.value
	updated.ExecutionVersion++
	updated.ResourceLimits = ResourceLimitsDisabled
	provider.set(updated, nil)
	caps, err = client.Capabilities(context.Background())
	if err != nil || !caps.Ready || caps.ResourceLimits != ResourceLimitsDisabled || caps.MaxConcurrent != 1 || caps.Warning == "" {
		t.Fatalf("new settings did not take effect: %+v err=%v", caps, err)
	}
	provider.set(Settings{}, errors.New("settings offline"))
	caps, err = client.Capabilities(context.Background())
	if err != nil || caps.Ready || caps.SettingsConfigured || !caps.Rootless {
		t.Fatalf("settings failure hid capabilities or permitted fallback: %+v err=%v", caps, err)
	}
}

func TestDynamicConcurrencyAppliesToNewRuns(t *testing.T) {
	initial := configuredSettings()
	initial.MaxConcurrent = 2
	provider := &mutableSettings{value: initial}
	c, _, _ := fixtureConfig(t, nil, Config{SettingsProvider: provider})
	// Represent one already-admitted run without launching a second container in
	// this single-container Docker protocol fixture.
	c.runs["existing"] = &activeRun{spec: RunSpec{RunID: "existing", TaskID: "existing", AttemptID: "existing"}, run: Run{RunID: "existing", State: "running", ResourceLimits: ResourceLimitsRequired}}
	t.Cleanup(func() { c.mu.Lock(); delete(c.runs, "existing"); c.mu.Unlock() })
	first := configuredSpec("test", initial)
	if _, err := c.Start(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if run := waitTerminal(t, c, first.RunID); run.State != "succeeded" {
		t.Fatalf("first run failed: %+v", run)
	}
	updated := initial
	updated.ExecutionVersion++
	updated.MaxConcurrent = 1
	provider.set(updated, nil)
	next := configuredSpec("test", updated)
	next.RunID, next.TaskID, next.AttemptID = "next", "next", "next"
	if _, err := c.Start(context.Background(), next); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("new concurrency was ignored: %v", err)
	}
	caps := c.Capabilities(context.Background())
	if caps.MaxConcurrent != 1 || caps.ActiveRuns != 1 {
		t.Fatalf("wrong active settings: %+v", caps)
	}
	old, _ := c.Inspect(first.RunID)
	if old.ExecutionVersion != initial.ExecutionVersion || old.ResourceLimits != ResourceLimitsRequired {
		t.Fatal("new settings changed a previous run")
	}
}

func TestRunPinsModelAndResourceModeWithoutPersistingSecrets(t *testing.T) {
	output := &synchronizedLog{}
	previous := log.Writer()
	log.SetOutput(output)
	t.Cleanup(func() { log.SetOutput(previous) })
	firstRequest, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		requests = append(requests, body.Model+" "+r.Header.Get("Authorization"))
		count := len(requests)
		mu.Unlock()
		if count == 1 || count == 3 {
			if count == 1 {
				close(firstRequest)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			writeJSON(w, 200, map[string]any{"id": "first", "model": body.Model, "choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "read", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go"}`}}}}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "final", "model": body.Model, "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "Plan completed after reading the file."}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
	}))
	defer server.Close()
	initial := configuredSettings()
	initial.ResourceLimits = ResourceLimitsDisabled
	initial.Model.BaseURL, initial.Model.AllowHTTP = server.URL+"/v1", true
	provider := &mutableSettings{value: initial}
	c, fake, _ := fixtureConfig(t, nil, Config{SettingsProvider: provider})
	first := configuredSpec("plan", initial)
	if _, err := c.Start(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstRequest:
	case <-time.After(3 * time.Second):
		t.Fatal("model request did not start")
	}
	updated := initial
	updated.ExecutionVersion++
	updated.ResourceLimits = ResourceLimitsRequired
	updated.ExecutionProfile = ExecutionProfile{NetworkMode: "bridge", TestWorkspaceWritable: true}
	updated.Model.Version++
	updated.Model.ModelName, updated.Model.APIKey = "model-two", "private-model-key-two"
	provider.set(updated, nil)
	caps := c.Capabilities(context.Background())
	if caps.ExecutionVersion != updated.ExecutionVersion || caps.ActiveRuns != 1 || caps.NetworkMode != "bridge" || caps.ReadOnlyRootFS || !caps.TestWorkspaceWritable {
		t.Fatalf("settings change not observed: %+v", caps)
	}
	unblock()
	firstResult := waitTerminal(t, c, first.RunID)
	if firstResult.State != "succeeded" || firstResult.ModelVersion != initial.Model.Version || firstResult.ResourceLimits != ResourceLimitsDisabled {
		t.Fatalf("running configuration changed: %+v", firstResult)
	}
	fake.mu.Lock()
	_, hasMemory := fake.createBody["HostConfig"].(map[string]any)["Memory"]
	firstProfileKept := fake.createBody["HostConfig"].(map[string]any)["NetworkMode"] == "none" && fake.createBody["HostConfig"].(map[string]any)["ReadonlyRootfs"] == true
	fake.mu.Unlock()
	if hasMemory || !firstProfileKept {
		t.Fatal("run did not use its frozen resource mode and profile")
	}
	// Wait for asynchronous post-result cleanup before reusing the one-container
	// fake Docker endpoint. Production uses distinct container identities.
	deadline := time.Now().Add(time.Second)
	for {
		fake.mu.Lock()
		removed := fake.container == nil
		fake.mu.Unlock()
		if removed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first container was not cleaned up")
		}
		time.Sleep(time.Millisecond)
	}
	next := configuredSpec("plan", updated)
	next.RunID, next.TaskID, next.AttemptID = "second", "second", "second"
	if _, err := c.Start(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	secondResult := waitTerminal(t, c, next.RunID)
	if secondResult.State != "succeeded" || secondResult.ModelVersion != updated.Model.Version || secondResult.ResourceLimits != ResourceLimitsRequired {
		t.Fatalf("next run did not use new configuration: %+v", secondResult)
	}
	fake.mu.Lock()
	nextProfileApplied := fake.createBody["HostConfig"].(map[string]any)["NetworkMode"] == "bridge" && fake.createBody["HostConfig"].(map[string]any)["ReadonlyRootfs"] == false
	fake.mu.Unlock()
	if !nextProfileApplied {
		t.Fatal("new run did not apply updated execution profile")
	}
	mu.Lock()
	actualRequests := append([]string(nil), requests...)
	mu.Unlock()
	want := []string{"model-one Bearer private-model-key-one", "model-one Bearer private-model-key-one", "model-two Bearer private-model-key-two", "model-two Bearer private-model-key-two"}
	if len(actualRequests) != len(want) {
		t.Fatalf("wrong request count: %d", len(actualRequests))
	}
	for i := range want {
		if actualRequests[i] != want[i] {
			t.Fatalf("request %d used the wrong frozen model", i)
		}
	}
	for _, spec := range []RunSpec{first, next} {
		for _, name := range []string{"spec.json", "run.json", "tools.ndjson"} {
			data, err := os.ReadFile(filepath.Join(c.runDir(spec.RunID), name))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			for _, secret := range []string{initial.Model.APIKey, updated.Model.APIKey, initial.Model.BaseURL} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatalf("%s leaked model configuration", name)
				}
			}
		}
		file, err := c.Collect(spec.RunID)
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(file)
		if err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		data, err := io.ReadAll(gz)
		_ = gz.Close()
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{initial.Model.APIKey, updated.Model.APIKey, initial.Model.BaseURL} {
			if bytes.Contains(data, []byte(secret)) || strings.Contains(output.String(), secret) {
				t.Fatal("artifact or log leaked model configuration")
			}
		}
	}
}
