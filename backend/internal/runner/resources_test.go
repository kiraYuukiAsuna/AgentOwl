package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestResourceLimitsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, mode, wantMode string
		concurrency          int
		wantErr              bool
	}{
		{name: "default is required", wantMode: ResourceLimitsRequired},
		{name: "required", mode: ResourceLimitsRequired, wantMode: ResourceLimitsRequired, concurrency: 2},
		{name: "disabled", mode: ResourceLimitsDisabled, wantMode: ResourceLimitsDisabled},
		{name: "invalid mode", mode: "false", wantErr: true},
		{name: "mode is exact", mode: "DISABLED", wantErr: true},
		{name: "disabled rejects concurrency", mode: ResourceLimitsDisabled, concurrency: 2, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c, err := NewController(Config{DataRoot: root, HostDataRoot: root, AllowedImages: []string{testImage}, ResourceLimits: tc.mode, MaxConcurrent: tc.concurrency}, &Engine{}, &testAuthorizer{}, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("construction error = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil && (c.cfg.ResourceLimits != tc.wantMode || tc.mode == ResourceLimitsDisabled && c.cfg.MaxConcurrent != 1) {
				t.Fatalf("unexpected effective config: %+v", c.cfg)
			}
		})
	}
}

func TestDisabledResourceLimitsOnlyRelaxCgroupAdmission(t *testing.T) {
	info := DockerInfo{SecurityOptions: []string{"name=rootless"}, DockerRootDir: "/runner/runtime", CgroupVersion: "1", CgroupDriver: "none"}
	for _, mode := range []string{"", ResourceLimitsRequired} {
		if err := validateInfo(info, "/runner", mode); err == nil {
			t.Fatalf("mode %q admitted cgroup v1", mode)
		}
	}
	if err := validateInfo(info, "/runner", ResourceLimitsDisabled); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*DockerInfo){
		func(i *DockerInfo) { i.SecurityOptions = nil },
		func(i *DockerInfo) { i.DockerRootDir = "/var/lib/docker" },
	} {
		bad := info
		change(&bad)
		if err := validateInfo(bad, "/runner", ResourceLimitsDisabled); err == nil {
			t.Fatalf("disabled mode accepted the wrong runtime: %+v", bad)
		}
	}
	c, fake, _ := fixtureConfig(t, nil, Config{ResourceLimits: ResourceLimitsDisabled})
	// Use the real production preflight closure. A normal temporary directory
	// cannot pass the required bounded tmpfs/block-filesystem checks in any mode.
	actual, err := NewController(c.cfg, c.engine, c.authorizer, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake.info.CgroupVersion, fake.info.CgroupDriver = "1", "none"
	fake.info.MemoryLimit, fake.info.PidsLimit, fake.info.CPUQuota = false, false, false
	if err := actual.preflight(context.Background(), ResourceLimitsDisabled); err == nil {
		t.Fatal("disabled mode bypassed filesystem admission")
	}
}

func TestDockerResourceModeChangesOnlyCgroupFields(t *testing.T) {
	c, fake, _ := fixture(t, nil)
	spec := specFor("test")
	var required map[string]any
	for _, mode := range []string{ResourceLimitsRequired, ResourceLimitsDisabled} {
		if _, err := c.engine.Create(context.Background(), spec, "test-container", "/runner/tasks/task/attempt", "digest", mode); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		body := fake.createBody
		fake.mu.Unlock()
		host := body["HostConfig"].(map[string]any)
		for key, want := range map[string]float64{"Memory": float64(spec.Limits.MemoryBytes), "MemorySwap": float64(spec.Limits.MemoryBytes), "NanoCpus": float64(spec.Limits.CPUMillis * 1_000_000), "PidsLimit": float64(spec.Limits.PIDs)} {
			got, exists := host[key]
			if mode == ResourceLimitsRequired && (!exists || got != want) {
				t.Fatalf("required mode %s = %v, want %v", key, got, want)
			}
			if mode == ResourceLimitsDisabled && exists {
				t.Fatalf("disabled mode still sends %s", key)
			}
			delete(host, key)
		}
		if mode == ResourceLimitsRequired {
			required = body
		} else if !reflect.DeepEqual(required, body) {
			t.Fatal("disabled mode changed non-resource container settings")
		}
		mount := host["Mounts"].([]any)[0].(map[string]any)
		if body["User"] != "65532:65532" || body["NetworkDisabled"] != true || host["NetworkMode"] != "none" || host["ReadonlyRootfs"] != true || mount["ReadOnly"] != true ||
			!reflect.DeepEqual(host["CapDrop"], []any{"ALL"}) || !reflect.DeepEqual(host["SecurityOpt"], []any{"no-new-privileges:true"}) || host["Tmpfs"] == nil || host["Ulimits"] == nil {
			t.Fatal("container isolation settings were relaxed")
		}
	}
	if _, err := c.engine.Create(context.Background(), spec, "bad-mode", "/runner/tasks/task/attempt", "digest", "typo"); err == nil {
		t.Fatal("unknown resource mode reached Docker")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 2 {
		t.Fatalf("invalid mode created a container: count %d", fake.creates)
	}
}

type synchronizedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *synchronizedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestDisabledResourceLimitsAreVisibleInHealthLogsAndArtifacts(t *testing.T) {
	output := &synchronizedLog{}
	previous := log.Writer()
	log.SetOutput(output)
	t.Cleanup(func() { log.SetOutput(previous) })
	c, _, _ := fixtureConfig(t, nil, Config{ResourceLimits: ResourceLimitsDisabled})
	if !strings.Contains(output.String(), resourceLimitsWarning) {
		t.Fatal("missing startup warning")
	}
	handler, err := NewHandler(c, strings.Repeat("secret", 8))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/healthz", "/health/ready"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		var status map[string]string
		if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != 200 || status["status"] != "ready" || status["resource_limits"] != ResourceLimitsDisabled || status["warning"] != resourceLimitsWarning {
			t.Fatalf("missing degraded readiness: %s", recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), c.cfg.DataRoot) || strings.Contains(recorder.Body.String(), "secret") {
			t.Fatal("health response leaked configuration")
		}
	}
	spec := specFor("test")
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "succeeded" || run.ResourceLimits != ResourceLimitsDisabled || run.ResourceWarning != resourceLimitsWarning {
		t.Fatalf("run lost resource mode: %+v", run)
	}
	if text := output.String(); !strings.Contains(text, "run_id="+spec.RunID) || strings.Count(text, resourceLimitsWarning) < 2 {
		t.Fatalf("missing per-run warning: %s", text)
	}
	fd, err := c.Collect(spec.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	gz, err := gzip.NewReader(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatal("result.json missing from artifacts:", err)
		}
		if header.Name != "result.json" {
			continue
		}
		var result Run
		if err := json.NewDecoder(reader).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.ResourceLimits != ResourceLimitsDisabled || result.ResourceWarning != resourceLimitsWarning {
			t.Fatal("exported result omitted disabled limits")
		}
		break
	}
}

func TestRequiredResourceHealthAndFailureRemainExplicit(t *testing.T) {
	c, _, _ := fixture(t, nil)
	handler, err := NewHandler(c, strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var status map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || status["resource_limits"] != ResourceLimitsRequired || status["warning"] != "" {
		t.Fatalf("wrong required readiness: %s", recorder.Body.String())
	}
	c.preflight = func(context.Context, string) error { return errors.New("private host path /do/not/disclose") }
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != 503 || strings.Contains(recorder.Body.String(), "/do/not/disclose") {
		t.Fatalf("preflight failure leaked or reported healthy: %s", recorder.Body.String())
	}
}

func TestRunRequestCannotDisableControllerResourceLimits(t *testing.T) {
	c, fake, auth := fixture(t, nil)
	token := strings.Repeat("x", 32)
	handler, err := NewHandler(c, token)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(specFor("test"))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	body["resource_limits"] = ResourceLimitsDisabled
	encoded, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || c.cfg.ResourceLimits != ResourceLimitsRequired || auth.calls.Load() != 0 {
		t.Fatalf("request changed deployment policy: %d %s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 0 {
		t.Fatal("unrecognized policy field reached Docker")
	}
}

func TestDisabledLimitsKeepApprovalTimeoutAndCapacity(t *testing.T) {
	c, fake, auth := fixtureConfig(t, nil, Config{ResourceLimits: ResourceLimitsDisabled})
	auth.deny.Store(true)
	if _, err := c.Start(context.Background(), specFor("test")); err == nil {
		t.Fatal("disabled limits bypassed authorization")
	}
	auth.deny.Store(false)
	fake.runningTest = true
	spec := specFor("test")
	spec.Limits.TimeoutSeconds = 1
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	next := spec
	next.RunID, next.TaskID, next.AttemptID = "another-run", "another-task", "another-attempt"
	if _, err := c.Start(context.Background(), next); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("disabled limits admitted concurrent execution: %v", err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "failed" || run.Result.EvidenceValid || !strings.Contains(run.Result.Error, "deadline exceeded") {
		t.Fatalf("disabled limits bypassed timeout: %+v", run)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.container != nil && fake.container.State.Running {
		t.Fatal("timed out container is still running")
	}
}

func TestRecoverStopsOldDisabledRunBeforeRequiredAdmission(t *testing.T) {
	c, fake, _ := fixture(t, nil)
	fake.info.CgroupVersion, fake.info.CgroupDriver = "1", "none"
	fake.info.MemoryLimit, fake.info.PidsLimit, fake.info.CPUQuota = false, false, false
	spec := specFor("test")
	digest, _ := SpecDigest(spec)
	if err := os.MkdirAll(c.runDir(spec.RunID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := storeSnapshot(c.workspace(spec), spec.Files); err != nil {
		t.Fatal(err)
	}
	old := Run{RunID: spec.RunID, TaskID: spec.TaskID, AttemptID: spec.AttemptID, Epoch: spec.Epoch, Stage: spec.Stage, State: "running", SpecDigest: digest, ResourceLimits: ResourceLimitsDisabled, ResourceWarning: resourceLimitsWarning}
	if err := atomicJSON(filepath.Join(c.runDir(spec.RunID), "spec.json"), spec); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(c.runDir(spec.RunID), "run.json"), old); err != nil {
		t.Fatal(err)
	}
	fake.container = &Container{ID: "leftover", Name: containerName(spec.RunID)}
	fake.container.Config.Labels = map[string]string{"agentowl.managed": "true", "agentowl.run_id": spec.RunID, "agentowl.task_id": spec.TaskID, "agentowl.attempt_id": spec.AttemptID, "agentowl.epoch": "1", "agentowl.spec_digest": digest}
	fake.container.State.Running = true
	c.preflight = func(context.Context, string) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.container != nil {
			t.Error("admission ran before old container cleanup")
		}
		return validateInfo(fake.info, c.cfg.HostDataRoot, c.cfg.ResourceLimits)
	}
	if err := c.Recover(context.Background()); err == nil || !strings.Contains(err.Error(), "cgroup v2") {
		t.Fatalf("required admission did not fail after cleanup: %v", err)
	}
	run, err := c.Inspect(spec.RunID)
	if err != nil || run.State != "failed" || run.Result.EvidenceValid || run.ResourceLimits != ResourceLimitsDisabled {
		t.Fatalf("interrupted run was not invalidated: %+v %v", run, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.container != nil || fake.creates != 0 {
		t.Fatal("recovery left or restarted old execution")
	}
}

func TestRecoverNeverCleansWrongRuntime(t *testing.T) {
	for _, kind := range []string{"rootful", "different data-root"} {
		t.Run(kind, func(t *testing.T) {
			c, fake, _ := fixture(t, nil)
			if kind == "rootful" {
				fake.info.SecurityOptions = nil
			} else {
				fake.info.DockerRootDir = "/other/runtime"
			}
			fake.container = &Container{ID: "unrelated"}
			fake.container.Config.Labels = map[string]string{"agentowl.managed": "true", "agentowl.run_id": "unrelated"}
			fake.container.State.Running = true
			if err := c.Recover(context.Background()); err == nil {
				t.Fatal("recovery accepted the wrong runtime")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.container == nil || !fake.container.State.Running {
				t.Fatal("recovery touched another runtime")
			}
		})
	}
}
