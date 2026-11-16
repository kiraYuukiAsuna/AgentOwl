package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentowl/backend/internal/integration"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func TestDefaultTaskQuotasAreUnlimitedAndPositiveValuesHaveNoCeiling(t *testing.T) {
	c, _, _ := fixture(t, nil)
	spec := specFor("test")
	l := spec.Limits
	if l.WorkspaceBytes != 0 || l.MaxFiles != 0 || l.ArtifactBytes != 0 || l.MaxToolCalls != 0 || l.MaxTokens != 0 {
		t.Fatal("default task quotas are not unlimited", l)
	}
	if err := c.validate(spec); err != nil {
		t.Fatal(err)
	}
	spec.Limits.WorkspaceBytes, spec.Limits.ArtifactBytes = math.MaxInt64, math.MaxInt64
	spec.Limits.MaxFiles, spec.Limits.MaxToolCalls, spec.Limits.MaxTokens = math.MaxInt, math.MaxInt, math.MaxInt
	if err := c.validate(spec); err != nil {
		t.Fatal("positive quota was capped", err)
	}
	for _, field := range []string{"workspace", "files", "artifacts", "tools", "tokens", "daily"} {
		spec = specFor("test")
		switch field {
		case "workspace":
			spec.Limits.WorkspaceBytes = -1
		case "files":
			spec.Limits.MaxFiles = -1
		case "artifacts":
			spec.Limits.ArtifactBytes = -1
		case "tools":
			spec.Limits.MaxToolCalls = -1
		case "tokens":
			spec.Limits.MaxTokens = -1
		case "daily":
			spec.DailyTokenBudget = -1
		}
		if c.validate(spec) == nil {
			t.Error("negative quota accepted", field)
		}
	}
	if addSize(math.MaxInt64-1, 2) != math.MaxInt64 || multiplySize(math.MaxInt64, 1024) != math.MaxInt64 || agentMaxSteps(math.MaxInt) != math.MaxInt || agentMaxSteps(0) != math.MaxInt {
		t.Fatal("quota arithmetic wrapped")
	}
	if c.checkStoredCapacity(math.MaxInt64) == nil || c.checkStoredCapacity(-1) == nil {
		t.Fatal("storage physical boundary was bypassed")
	}
}

func TestSnapshotAndArchiveSupportIndependentUnlimitedQuotas(t *testing.T) {
	files := []File{{Path: "large.txt", Mode: 0644, Data: bytes.Repeat([]byte("x"), 300<<10)}, {Path: "second.txt", Mode: 0644, Data: []byte("second")}}
	root := t.TempDir()
	if err := storeSnapshot(root, files); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f.Path, Mode: int64(f.Mode), Typeflag: tar.TypeReg, Size: int64(len(f.Data))})
		_, _ = tw.Write(f.Data)
	}
	_ = tw.Close()
	for _, limits := range []struct {
		bytes int64
		files int
		pass  bool
	}{{0, 0, true}, {0, 2, true}, {1 << 20, 0, true}, {math.MaxInt64, math.MaxInt, true}, {10, 0, false}, {0, 1, false}} {
		for _, kind := range []string{"snapshot", "archive"} {
			var got []File
			var err error
			if kind == "snapshot" {
				got, err = readSnapshot(root, limits.bytes, limits.files)
			} else {
				got, err = readArchive(bytes.NewReader(archive.Bytes()), limits.bytes, limits.files)
			}
			if (err == nil) != limits.pass || limits.pass && len(got) != 2 {
				t.Fatalf("%s limits %+v: files=%d err=%v", kind, limits, len(got), err)
			}
		}
	}
	if err := os.Symlink("large.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := readSnapshot(root, 0, 0); err == nil {
		t.Fatal("unlimited snapshot accepted a link")
	}
	var unsafe bytes.Buffer
	tw = tar.NewWriter(&unsafe)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "outside"})
	_ = tw.Close()
	if _, err := readArchive(&unsafe, 0, 0); err == nil {
		t.Fatal("unlimited archive accepted a link")
	}
}

func TestDockerUnlimitedAndLargeWorkspaceQuotas(t *testing.T) {
	for _, unlimited := range []bool{true, false} {
		t.Run(fmt.Sprint(unlimited), func(t *testing.T) {
			c, fake, _ := fixture(t, nil)
			spec := specFor("test")
			if !unlimited {
				spec.Limits.WorkspaceBytes, spec.Limits.MaxFiles = 1<<30, 100001
			}
			if _, err := c.engine.Create(context.Background(), spec, "quota-test", c.workspace(spec), "digest", ResourceLimitsRequired); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			host := fake.createBody["HostConfig"].(map[string]any)
			mounts := host["Tmpfs"].(map[string]any)
			want := fmt.Sprintf("rw,nosuid,nodev,size=%d,nr_inodes=%d,mode=1777", spec.Limits.WorkspaceBytes, spec.Limits.MaxFiles)
			if mounts["/tmp"] != want || mounts["/output"] != want {
				t.Fatalf("unexpected tmpfs fallback: %+v", mounts)
			}
			fsize := false
			for _, raw := range host["Ulimits"].([]any) {
				limit := raw.(map[string]any)
				if limit["Name"] == "fsize" {
					fsize = true
					if limit["Soft"] != float64(spec.Limits.WorkspaceBytes) || limit["Hard"] != float64(spec.Limits.WorkspaceBytes) {
						t.Fatal("wrong finite file-size quota", limit)
					}
				}
			}
			if fsize == unlimited || host["Memory"] != float64(spec.Limits.MemoryBytes) || host["ReadonlyRootfs"] != true || host["NetworkMode"] != "none" || fake.createBody["User"] != "65532:65532" {
				t.Fatal("unlimited file quota changed another isolation setting", host)
			}
		})
	}
}

func TestUnlimitedAgentCanExceedFormerToolAndTokenLimits(t *testing.T) {
	var calls atomic.Int32
	model := modelFunc(func(_ context.Context, request integration.ChatRequest) (integration.ChatResponse, error) {
		if request.MaxTokens != 4096 {
			t.Errorf("per-request completion limit changed: %d", request.MaxTokens)
		}
		n := calls.Add(1)
		if n <= 105 {
			return einoToolReply(integration.ToolCall{ID: fmt.Sprint(n), Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}), nil
		}
		if n == 106 {
			return einoToolReply(integration.ToolCall{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"main.go","content":"package main\n// changed\n","executable":false}`)}), nil
		}
		return einoFinalReply("Updated the inspected file. Tests not run."), nil
	})
	c, _, _ := fixture(t, model)
	spec := specFor("edit")
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "succeeded" || !run.Result.EvidenceValid || run.Result.ToolCalls != 106 || run.Result.Tokens <= 200000 || calls.Load() != 107 {
		t.Fatalf("unlimited run hit a fixed limit: %+v requests=%d", run, calls.Load())
	}
}

func TestUnlimitedTaskTokensStillRespectDailyReservation(t *testing.T) {
	var calls atomic.Int32
	c, _, _ := fixture(t, modelFunc(func(context.Context, integration.ChatRequest) (integration.ChatResponse, error) {
		calls.Add(1)
		return einoToolReply(integration.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}), nil
	}))
	spec := specFor("plan")
	spec.DailyTokenBudget = 9000
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "failed" || calls.Load() == 0 || calls.Load() > 10 || run.Result.Tokens <= 0 || run.Result.Tokens > spec.DailyTokenBudget || !strings.Contains(run.Result.Error, "request was not sent") {
		t.Fatalf("daily reservation not enforced: %+v requests=%d", run, calls.Load())
	}
	spec.Limits.MaxTokens = 8000
	if tokenBudget(spec) != 8000 {
		t.Fatal("task quota did not narrow daily reservation")
	}
	spec.Limits.MaxTokens = 10000
	if tokenBudget(spec) != 9000 {
		t.Fatal("daily reservation did not narrow task quota")
	}
}

func TestAgentAccountsForProviderUsageAboveRequestedCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, wantState string
		daily           int64
	}{{"within_daily_budget", "succeeded", 100000}, {"exceeds_daily_budget", "failed", 8000}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _, _ := fixture(t, modelFunc(func(_ context.Context, request integration.ChatRequest) (integration.ChatResponse, error) {
				if request.MaxTokens != 4096 {
					t.Fatalf("unexpected completion request: %d", request.MaxTokens)
				}
				if calls.Add(1) == 1 {
					response := einoToolReply(integration.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)})
					response.Usage = integration.Usage{PromptTokens: 100, CompletionTokens: 12000, TotalTokens: 12100}
					return response, nil
				}
				return einoFinalReply("Reviewed the file. Tests not run."), nil
			}))
			spec := specFor("plan")
			spec.DailyTokenBudget = tc.daily
			if _, err := c.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			run := waitTerminal(t, c, spec.RunID)
			if run.State != tc.wantState || run.Result.Tokens < 12100 {
				t.Fatalf("usage not correctly accounted: %+v", run)
			}
			if tc.wantState == "failed" && !strings.Contains(run.Result.Error, "model token budget exceeded") {
				t.Fatalf("daily budget not enforced: %+v", run)
			}
		})
	}
}

func TestUnlimitedLargeFileToolsAndArtifactCollection(t *testing.T) {
	c, _, _ := fixture(t, nil)
	spec := specFor("edit")
	spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: true}
	if err := storeSnapshot(c.workspace(spec), spec.Files); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.runDir(spec.RunID), 0700); err != nil {
		t.Fatal(err)
	}
	a := &activeRun{spec: spec, run: Run{RunID: spec.RunID, State: "collecting"}}
	large := strings.Repeat("source text\n", 60000)
	args, _ := json.Marshal(map[string]any{"path": "main.go", "content": large, "executable": false})
	if _, err := c.tool(context.Background(), a, integration.ToolCall{Name: "write_file", Arguments: args}); err != nil {
		t.Fatal("large source write rejected", err)
	}
	got, err := c.tool(context.Background(), a, integration.ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)})
	if err != nil || got != large {
		t.Fatal("large source read rejected", err)
	}
	a.spec.Limits.ArtifactBytes = 1
	if err := c.collect(a, "succeeded"); err == nil {
		t.Fatal("finite artifact quota ignored")
	}
	a.spec.Limits.ArtifactBytes = 0
	if err := c.collect(a, "succeeded"); err != nil {
		t.Fatal("unlimited collection rejected", err)
	}
	fd, err := os.Open(filepath.Join(c.runDir(spec.RunID), "artifacts.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	gz, err := gzip.NewReader(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	files, err := readArchive(gz, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Path == "workspace/main.go" && string(file.Data) == large {
			return
		}
	}
	t.Fatal("large source missing from artifact")
}

func TestUnlimitedModelContextAndFileToolOutput(t *testing.T) {
	c, _, _ := fixture(t, nil)
	spec := specFor("plan")
	spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: true}
	large := strings.Repeat("source text\n", 60000)
	spec.Files[0].Data = []byte(large)
	if err := storeSnapshot(c.workspace(spec), spec.Files); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.runDir(spec.RunID), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	a := &activeRun{spec: spec, permitUntil: time.Now().Add(time.Minute)}
	session := &agentSession{controller: c, run: a, cancel: cancel}
	tools, err := session.tools()
	if err != nil {
		t.Fatal(err)
	}
	var read tool.InvokableTool
	for _, candidate := range tools {
		info, _ := candidate.Info(ctx)
		if info.Name == "read_file" {
			read = candidate.(tool.InvokableTool)
		}
	}
	output, err := read.InvokableRun(ctx, `{"path":"main.go"}`)
	if err != nil || output != large {
		t.Fatal("Eino file tool retained its fixed output cap", err)
	}
	called := false
	a.model = modelFunc(func(_ context.Context, request integration.ChatRequest) (integration.ChatResponse, error) {
		called = true
		if len(request.Messages[0].Content) <= 2<<20 || request.MaxTokens != 4096 {
			t.Fatal("context was truncated or per-request completion setting changed")
		}
		return einoFinalReply("Inspected the source. Tests not run."), nil
	})
	chat := &agentChatModel{session: session}
	if _, err := chat.Generate(ctx, []*schema.Message{schema.UserMessage(strings.Repeat(output, 4))}); err != nil || !called {
		t.Fatal("unlimited model context rejected before provider", err)
	}
}
