package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentowl/backend/internal/integration"
)

const testImage = "example/toolchain@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type testAuthorizer struct {
	deny  atomic.Bool
	calls atomic.Int64
}

func (a *testAuthorizer) Authorize(_ context.Context, _ PermitRequest) (Permit, error) {
	a.calls.Add(1)
	return Permit{Allowed: !a.deny.Load(), ExpiresAt: time.Now().Add(100 * time.Millisecond)}, nil
}

type modelFunc func(context.Context, integration.ChatRequest) (integration.ChatResponse, error)

func (f modelFunc) Complete(ctx context.Context, r integration.ChatRequest) (integration.ChatResponse, error) {
	return f(ctx, r)
}

type fakeDocker struct {
	mu           sync.Mutex
	container    *Container
	createBody   map[string]any
	workspace    string
	creates      int
	runningTest  bool
	mutate       bool
	failInspect  bool
	info         DockerInfo
	volumeRoot   string
	volume       bool
	onStart      func(string)
	seedOwned    bool
	archiveCalls int
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.45")
	if p == "/info" {
		writeJSON(w, 200, f.info)
		return
	}
	if strings.HasPrefix(p, "/images/") {
		writeJSON(w, 200, map[string]string{"Id": "image"})
		return
	}
	if p == "/containers/json" {
		var rows []map[string]string
		if f.container != nil {
			rows = append(rows, map[string]string{"Id": f.container.ID})
		}
		writeJSON(w, 200, rows)
		return
	}
	if p == "/containers/create" {
		f.creates++
		if err := json.NewDecoder(r.Body).Decode(&f.createBody); err != nil {
			tHTTPError(w, 400, err.Error())
			return
		}
		c := &Container{ID: "container-1", Name: r.URL.Query().Get("name")}
		labels := f.createBody["Labels"].(map[string]any)
		c.Config.Labels = map[string]string{}
		for k, v := range labels {
			c.Config.Labels[k] = v.(string)
		}
		c.State.Status = "created"
		f.container = c
		host := f.createBody["HostConfig"].(map[string]any)
		mounts := host["Mounts"].([]any)
		mount := mounts[0].(map[string]any)
		f.volume = mount["Type"] == "volume"
		if f.volume {
			volumeName := fmt.Sprintf("%064x", f.creates)
			f.workspace = filepath.Join(f.volumeRoot, volumeName, "_data")
			c.Mounts = []ContainerMount{{Type: "volume", Name: volumeName, Source: f.workspace, Destination: "/workspace", Driver: "local", RW: true}}
			if err := os.MkdirAll(f.workspace, 0755); err != nil {
				tHTTPError(w, 500, err.Error())
				return
			}
		} else {
			f.workspace = mount["Source"].(string)
		}
		writeJSON(w, 201, map[string]string{"Id": c.ID})
		return
	}
	if strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json") {
		if f.failInspect {
			tHTTPError(w, 503, "runtime unavailable")
			return
		}
		if f.container == nil {
			tHTTPError(w, 404, "not found")
			return
		}
		writeJSON(w, 200, f.container)
		return
	}
	if f.container == nil {
		tHTTPError(w, 404, "not found")
		return
	}
	if strings.HasSuffix(p, "/start") {
		cmd := f.createBody["Cmd"].([]any)
		test := cmd[0].(string) != "/bin/sleep"
		f.container.State.Running = !test || f.runningTest
		f.container.State.Status = "running"
		if test && !f.runningTest {
			f.container.State.Status = "exited"
		}
		if f.mutate {
			_ = os.WriteFile(filepath.Join(f.workspace, "main.go"), []byte("tampered"), 0644)
		}
		if f.onStart != nil {
			workdir := f.workspace
			if f.volume {
				workdir = filepath.Join(workdir, "source")
			}
			f.onStart(workdir)
		}
		w.WriteHeader(204)
		return
	}
	if strings.HasSuffix(p, "/kill") {
		f.container.State.Running = false
		f.container.State.Status = "exited"
		w.WriteHeader(204)
		return
	}
	if strings.HasSuffix(p, "/archive") && r.Method == "GET" {
		f.archiveCalls++
		name := strings.TrimPrefix(r.URL.Query().Get("path"), "/workspace/")
		data, err := os.ReadFile(filepath.Join(f.workspace, name))
		if err != nil {
			tHTTPError(w, 404, "file missing")
			return
		}
		b, _ := fileTar(filepath.Base(name), 0644, data)
		info, err := os.Lstat(filepath.Join(f.workspace, name))
		if err != nil {
			tHTTPError(w, 404, "file missing")
			return
		}
		mode := uint32(0644)
		if info.Mode()&0111 != 0 {
			mode = 0755
		}
		b, _ = fileTar(filepath.Base(name), mode, data)
		stat, _ := json.Marshal(map[string]any{"mode": info.Mode(), "size": info.Size()})
		w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(b)
		return
	}
	if strings.HasSuffix(p, "/archive") && r.Method == "PUT" {
		f.archiveCalls++
		if r.URL.Query().Get("copyUIDGID") == "1" {
			tr := tar.NewReader(r.Body)
			f.seedOwned = true
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					tHTTPError(w, 400, err.Error())
					return
				}
				f.seedOwned = f.seedOwned && h.Uid == 65532 && h.Gid == 65532
				target := filepath.Join(f.workspace, h.Name)
				if h.Typeflag == tar.TypeDir {
					_ = os.MkdirAll(target, os.FileMode(h.Mode))
					continue
				}
				data, err := io.ReadAll(tr)
				if err == nil {
					err = os.WriteFile(target, data, os.FileMode(h.Mode))
				}
				if err != nil {
					tHTTPError(w, 500, err.Error())
					return
				}
			}
			w.WriteHeader(200)
			return
		}
		files, err := readArchive(r.Body, 1<<20, 100)
		if err != nil {
			tHTTPError(w, 400, err.Error())
			return
		}
		for _, file := range files {
			target := filepath.Join(f.workspace, file.Path)
			_ = os.MkdirAll(filepath.Dir(target), 0755)
			if err := os.WriteFile(target, file.Data, os.FileMode(file.Mode)); err != nil {
				tHTTPError(w, 500, err.Error())
				return
			}
		}
		w.WriteHeader(200)
		return
	}
	if strings.HasSuffix(p, "/logs") {
		w.WriteHeader(200)
		return
	}
	if r.Method == "DELETE" {
		if f.volume && r.URL.Query().Get("v") == "true" {
			_ = os.RemoveAll(f.workspace)
		}
		f.container = nil
		w.WriteHeader(204)
		return
	}
	tHTTPError(w, 404, "unexpected route")
}
func tHTTPError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func fixture(t *testing.T, model Model) (*Controller, *fakeDocker, *testAuthorizer) {
	return fixtureConfig(t, model, Config{})
}

func fixtureConfig(t *testing.T, model Model, cfg Config) (*Controller, *fakeDocker, *testAuthorizer) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"tasks", "runtime", "manifests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeDocker{volumeRoot: filepath.Join(root, "runtime", "volumes"), info: DockerInfo{SecurityOptions: []string{"name=rootless"}, CgroupVersion: "2", CgroupDriver: "systemd", DockerRootDir: filepath.Join(root, "runtime"), MemoryLimit: true, PidsLimit: true, CPUQuota: true}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	engine := &Engine{client: server.Client(), baseURL: server.URL}
	auth := &testAuthorizer{}
	cfg.DataRoot, cfg.HostDataRoot = root, root
	cfg.AllowedImages = []string{testImage}
	cfg.LeasePoll = 10 * time.Millisecond
	c, err := NewController(cfg, engine, auth, model)
	if err != nil {
		t.Fatal(err)
	}
	// Only tests replace filesystem preflight; resource mode never bypasses it.
	c.preflight = func(context.Context, string) error { return nil }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c, fake, auth
}
func specFor(stage string) RunSpec {
	files := []File{{Path: "main.go", Mode: 0644, Data: []byte("package main\n")}}
	digest, _ := SnapshotDigest(files)
	spec := RunSpec{RunID: "run-1", TaskID: "task-1", StepID: "step-1", AttemptID: "attempt-1", Epoch: 1, Stage: stage, ActionDigest: strings.Repeat("a", 64), InputDigest: digest, TreeSHA: strings.Repeat("b", 40), Image: testImage, Goal: "Make the approved change", AllowedPaths: []string{"main.go"}, Limits: DefaultLimits(), Files: files}
	if stage == "test" {
		spec.Argv = []string{"/bin/true"}
	}
	return spec
}
func waitTerminal(t *testing.T, c *Controller, id string) Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := c.Inspect(id)
		if err != nil {
			t.Fatal(err)
		}
		if terminal(run.State) {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return Run{}
}

func TestSnapshotDigestRejectsAmbiguousAndEscapingPaths(t *testing.T) {
	a := []File{{Path: "a", Mode: 0644, Data: []byte("x")}, {Path: "b", Mode: 0755, Data: []byte("y")}}
	digest, err := SnapshotDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	other, err := SnapshotDigest([]File{a[1], a[0]})
	if err != nil || digest != other {
		t.Fatal("snapshot ordering affected digest")
	}
	for _, p := range []string{"../escape", "/root", "a/../b", "a\\b", ".git/config", "x\x00"} {
		if _, err := SnapshotDigest([]File{{Path: p, Mode: 0644}}); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	if _, err := SnapshotDigest([]File{a[0], a[0]}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := SnapshotDigest([]File{{Path: "a", Mode: 0644}, {Path: "a-b", Mode: 0644}, {Path: "a/x", Mode: 0644}}); err == nil {
		t.Fatal("nonadjacent file/directory collision accepted")
	}
}

func TestArchiveRejectsLinksAndTraversal(t *testing.T) {
	for _, h := range []tar.Header{{Name: "../escape", Mode: 0644, Typeflag: tar.TypeReg}, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, {Name: "device", Typeflag: tar.TypeChar}} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		if _, err := readArchive(&b, 1024, 10); err == nil {
			t.Fatalf("accepted archive header %+v", h)
		}
	}
}

func TestDockerTestRunProducesBoundEvidenceAndSafeConfig(t *testing.T) {
	c, fake, _ := fixture(t, nil)
	spec := specFor("test")
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "succeeded" || !run.Result.EvidenceValid || run.Result.ExitCode == nil || *run.Result.ExitCode != 0 || run.Result.OutputDigest != spec.InputDigest {
		t.Fatalf("wrong test evidence: %+v", run)
	}
	fake.mu.Lock()
	host := fake.createBody["HostConfig"].(map[string]any)
	mount := host["Mounts"].([]any)[0].(map[string]any)
	if host["NetworkMode"] != "none" || host["ReadonlyRootfs"] != true || mount["ReadOnly"] != true || mount["Source"] != filepath.Join(c.cfg.HostDataRoot, "tasks", spec.TaskID, spec.AttemptID) {
		t.Errorf("unsafe create config: %+v", host)
	}
	creates := fake.creates
	fake.mu.Unlock()
	if creates != 1 {
		t.Fatal("duplicate container")
	}
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	changed := spec
	changed.Goal = "different"
	if _, err := c.Start(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("wanted conflict, got %v", err)
	}
	file, err := c.Collect(spec.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	entries, err := readArchive(gz, 64<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Path == "workspace/main.go" {
			found = true
			if string(entry.Data) != "package main\n" {
				t.Fatal("wrong source artifact")
			}
		}
	}
	if !found {
		t.Fatal("source artifact missing")
	}
}

func TestModifiedTestInputInvalidatesEvidence(t *testing.T) {
	c, fake, _ := fixture(t, nil)
	fake.mutate = true
	spec := specFor("test")
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "failed" || run.Result.EvidenceValid || !strings.Contains(run.Result.Error, "modified frozen input") {
		t.Fatalf("tampering not detected: %+v", run)
	}
}

func TestAgentEditsRealSandboxFilesWithoutShell(t *testing.T) {
	var calls int
	model := modelFunc(func(_ context.Context, request integration.ChatRequest) (integration.ChatResponse, error) {
		calls++
		for _, tool := range request.Tools {
			if strings.Contains(tool.Name, "exec") || strings.Contains(tool.Name, "shell") {
				t.Fatal("shell capability exposed")
			}
		}
		response := integration.ChatResponse{FinishReason: "stop", Usage: integration.Usage{TotalTokens: 10}, Message: integration.ChatMessage{Role: "assistant"}}
		switch calls {
		case 1:
			response.Message.ToolCalls = []integration.ToolCall{{ID: "one", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}}
		case 2:
			response.Message.ToolCalls = []integration.ToolCall{{ID: "two", Name: "write_file", Arguments: json.RawMessage(`{"path":"main.go","content":"package main\n// approved change\n","executable":false}`)}}
		default:
			response.Message.Content = "Implemented the approved change. Tests not run."
		}
		if len(response.Message.ToolCalls) > 0 {
			response.FinishReason = "tool_calls"
		}
		return response, nil
	})
	c, _, _ := fixture(t, model)
	spec := specFor("edit")
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "succeeded" || run.Result.OutputDigest == spec.InputDigest || run.Result.ToolCalls != 2 {
		t.Fatalf("agent failed: %+v", run)
	}
}

func TestReviewVerdictBlocksFindingsAndMalformedFinals(t *testing.T) {
	for _, test := range []struct {
		name, content string
		passed        bool
	}{
		{"pass", `{"verdict":"pass","summary":"Inspected main.go. Tests were not rerun; the supplied evidence was reviewed.","findings":[]}`, true},
		{"blocked", `{"verdict":"blocked","summary":"Authentication can bypass validation.","findings":["Reject missing credentials before continuing."]}`, false},
		{"contradictory", `{"verdict":"pass","summary":"Needs changes.","findings":["Missing authorization check."]}`, false},
		{"plain_text", "Blocking vulnerability: authentication is bypassed.", false},
		{"duplicate", `{"verdict":"blocked","verdict":"pass","summary":"Invalid duplicate verdict.","findings":[]}`, false},
		{"missing_findings", `{"verdict":"pass","summary":"No findings declared."}`, false},
		{"null_findings", `{"verdict":"pass","summary":"No findings declared.","findings":null}`, false},
		{"unknown_field", `{"verdict":"pass","summary":"Claiming approval.","findings":[],"approved":true}`, false},
		{"trailing", `{"verdict":"pass","summary":"Finished.","findings":[]} {}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			model := modelFunc(func(_ context.Context, req integration.ChatRequest) (integration.ChatResponse, error) {
				calls++
				if req.ResponseSchema == nil || req.ResponseSchema.Name != "agentowl_code_review" {
					t.Fatal("structured review schema missing")
				}
				response := integration.ChatResponse{FinishReason: "stop", Message: integration.ChatMessage{Role: "assistant", Content: test.content}}
				if calls == 1 {
					response.FinishReason = "tool_calls"
					response.Message.Content = ""
					response.Message.ToolCalls = []integration.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}}
				}
				return response, nil
			})
			c, _, _ := fixture(t, model)
			spec := specFor("review")
			if _, err := c.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			run := waitTerminal(t, c, spec.RunID)
			if (run.State == "succeeded") != test.passed || run.Result.EvidenceValid != test.passed || run.Result.Summary != test.content {
				t.Fatalf("wrong review result: %+v", run)
			}
			if calls != 2 {
				t.Fatalf("unexpected model retries: %d", calls)
			}
		})
	}
}

func TestModelBudgetRejectsLargePromptBeforeFirstRequest(t *testing.T) {
	calls := 0
	c, _, _ := fixture(t, modelFunc(func(context.Context, integration.ChatRequest) (integration.ChatResponse, error) {
		calls++
		return integration.ChatResponse{}, nil
	}))
	spec := specFor("plan")
	spec.Limits.MaxTokens = 4096
	spec.Context = strings.Repeat("x", 6000)
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "failed" || calls != 0 || run.Result.Tokens != 0 || !strings.Contains(run.Result.Error, "request was not sent") {
		t.Fatalf("oversize prompt sent: %+v calls=%d", run, calls)
	}
}

func TestModelBudgetCountsToolHistoryBeforeNextRequest(t *testing.T) {
	calls := 0
	c, _, _ := fixture(t, modelFunc(func(_ context.Context, req integration.ChatRequest) (integration.ChatResponse, error) {
		calls++
		if req.MaxTokens <= 0 || req.MaxTokens > 4096 {
			t.Fatalf("invalid completion budget: %d", req.MaxTokens)
		}
		return integration.ChatResponse{FinishReason: "tool_calls", Message: integration.ChatMessage{Role: "assistant", ToolCalls: []integration.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}}}}, nil
	}))
	spec := specFor("plan")
	spec.Limits.MaxTokens = 9000
	spec.Files[0].Data = []byte(strings.Repeat("large source\n", 1000))
	spec.InputDigest, _ = SnapshotDigest(spec.Files)
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "failed" || calls != 1 || run.Result.Tokens <= 0 || run.Result.Tokens > int64(spec.Limits.MaxTokens) || !strings.Contains(run.Result.Error, "request was not sent") {
		t.Fatalf("oversize tool history sent: %+v calls=%d", run, calls)
	}
}

func TestModelFailureRetainsBudgetReservationAndReviewReport(t *testing.T) {
	calls := 0
	report := `{"verdict":"blocked","summary":"Validation failed.","findings":["Unsafe input."]}`
	c, _, _ := fixture(t, modelFunc(func(_ context.Context, req integration.ChatRequest) (integration.ChatResponse, error) {
		calls++
		return integration.ChatResponse{Message: integration.ChatMessage{Role: "assistant", Content: report}}, integration.ErrInvalidModelOutput
	}))
	spec := specFor("review")
	spec.Limits.MaxTokens = 32000
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "failed" || calls != 1 || run.Result.Tokens <= 0 || run.Result.Tokens > int64(spec.Limits.MaxTokens) || run.Result.Summary != report || run.Result.EvidenceValid {
		t.Fatalf("ambiguous request lost budget/report: %+v", run)
	}
}

func TestPermissionRevocationStopsRunningContainer(t *testing.T) {
	c, fake, auth := fixture(t, nil)
	fake.runningTest = true
	spec := specFor("test")
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r, _ := c.Inspect(spec.RunID)
		if r.State == "running" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	auth.deny.Store(true)
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "stopped" || run.Result.EvidenceValid {
		t.Fatalf("revoked execution continued: %+v", run)
	}
}

func TestDeniedPermissionNeverCreatesContainer(t *testing.T) {
	c, fake, auth := fixture(t, nil)
	auth.deny.Store(true)
	if _, err := c.Start(context.Background(), specFor("test")); err == nil {
		t.Fatal("denied run started")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 0 {
		t.Fatal("created container before authorization")
	}
}

func TestModelCannotWriteDuringPlanOrEscapeEditScope(t *testing.T) {
	c, _, _ := fixture(t, nil)
	for _, stage := range []string{"plan", "review", "edit"} {
		a := &activeRun{spec: specFor(stage)}
		_, err := c.tool(context.Background(), a, integration.ToolCall{Name: "write_file", Arguments: json.RawMessage(`{"path":"other.go","content":"bad","executable":false}`)})
		if err == nil {
			t.Fatalf("%s allowed unauthorized write", stage)
		}
	}
}

func TestHTTPAuthAndAuthorizerContract(t *testing.T) {
	c, _, _ := fixture(t, nil)
	handler, err := NewHandler(c, strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := NewClient(server.URL, "wrong", server.Client())
	if _, err := client.Start(context.Background(), specFor("test")); err == nil {
		t.Fatal("missing service authentication")
	}
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/runner/authorize" || r.URL.Query().Get("machine_id") != "machine-one" || r.Header.Get("Authorization") != "Bearer callback-secret" {
			t.Errorf("wrong callback request")
		}
		var req PermitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SpecDigest != "digest" {
			t.Errorf("missing evidence binding")
		}
		writeJSON(w, 200, Permit{Allowed: true, ExpiresAt: time.Now().Add(time.Hour)})
	}))
	defer callback.Close()
	permit, err := NewHTTPAuthorizer(callback.URL, "callback-secret", "machine-one", callback.Client()).Authorize(context.Background(), PermitRequest{SpecDigest: "digest"})
	if err != nil || !permit.Allowed || time.Until(permit.ExpiresAt) > time.Minute {
		t.Fatalf("invalid permit: %+v %v", permit, err)
	}
}

func TestPreflightRejectsRootfulOrUnenforcedRuntime(t *testing.T) {
	good := DockerInfo{SecurityOptions: []string{"name=rootless"}, CgroupVersion: "2", CgroupDriver: "systemd", DockerRootDir: "/data/runtime", MemoryLimit: true, PidsLimit: true, CPUQuota: true}
	if err := validateInfo(good, "/data", ""); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*DockerInfo){func(i *DockerInfo) { i.SecurityOptions = nil }, func(i *DockerInfo) { i.CgroupDriver = "none" }, func(i *DockerInfo) { i.CPUQuota = false }, func(i *DockerInfo) { i.DockerRootDir = "/var/lib/docker" }} {
		bad := good
		change(&bad)
		if err := validateInfo(bad, "/data", ""); err == nil {
			t.Fatal("unsafe daemon accepted")
		}
	}
}

func TestRecoverKillsInterruptedContainerAndDoesNotRestart(t *testing.T) {
	c, fake, _ := fixture(t, nil)
	spec := specFor("test")
	digest, _ := SpecDigest(spec)
	if err := os.MkdirAll(c.runDir(spec.RunID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := storeSnapshot(c.workspace(spec), spec.Files); err != nil {
		t.Fatal(err)
	}
	a := &activeRun{spec: spec, run: Run{RunID: spec.RunID, TaskID: spec.TaskID, AttemptID: spec.AttemptID, Epoch: 1, Stage: "test", State: "running", SpecDigest: digest}}
	if err := atomicJSON(filepath.Join(c.runDir(spec.RunID), "spec.json"), spec); err != nil {
		t.Fatal(err)
	}
	if err := c.persist(a); err != nil {
		t.Fatal(err)
	}
	fake.container = &Container{ID: "orphan", Name: containerName(spec.RunID)}
	fake.container.Config.Labels = map[string]string{"agentowl.managed": "true", "agentowl.run_id": spec.RunID, "agentowl.task_id": spec.TaskID, "agentowl.attempt_id": spec.AttemptID, "agentowl.epoch": "1", "agentowl.spec_digest": digest}
	fake.container.State.Running = true
	if err := c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err := c.Inspect(spec.RunID)
	if err != nil || run.State != "failed" || run.Result.EvidenceValid {
		t.Fatalf("recovery resumed unsafe state: %+v %v", run, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.container != nil || fake.creates != 0 {
		t.Fatal("recovery did not terminate interrupted container")
	}
}

func TestArchiveURLCannotInjectQuery(t *testing.T) {
	u, err := url.Parse("http://docker/v1.45/containers/id/archive?path=" + url.QueryEscape("/workspace/a?path=/etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Query()) != 1 {
		t.Fatal("path escaped into query")
	}
}

func TestRedactionPreservesZodSchemaButMasksPassword(t *testing.T) {
	const source = `password: z.string().min(1, "请输入密码"),`
	if got := redact(source); got != source {
		t.Fatalf("schema was corrupted: %q", got)
	}
	if got := redact("password=private-secret"); strings.Contains(got, "private-secret") {
		t.Fatal("password was not redacted")
	}
}

func TestRedactionRemovesSecrets(t *testing.T) {
	text := redact("Authorization: Bearer secret\naccess_token=token\n-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----")
	for _, value := range []string{"Bearer secret", "=token", "\nkey\n"} {
		if strings.Contains(text, value) {
			t.Fatal("secret leaked")
		}
	}
}
