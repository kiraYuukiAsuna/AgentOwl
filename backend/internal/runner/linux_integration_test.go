//go:build linux

package runner

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentowl/backend/internal/integration"
)

type smokeAuthorizer struct{ deny atomic.Bool }

func (a *smokeAuthorizer) Authorize(context.Context, PermitRequest) (Permit, error) {
	return Permit{Allowed: !a.deny.Load(), ExpiresAt: time.Now().Add(time.Minute)}, nil
}

// This opt-in test runs real containers on an otherwise idle, dedicated Runner
// daemon. Use a pinned image containing /bin/sh, /bin/sleep and /var/tmp. It never
// calls a model provider, business API, or GitHub, and removes its own artifacts.
// An optional public AGENTOWL_RUNNER_SMOKE_NETWORK_URL is downloaded with wget in
// the bridge test only; the payload is never executed or added to source output.
func TestLinuxRunnerSmoke(t *testing.T) {
	root, image := os.Getenv("AGENTOWL_RUNNER_SMOKE_ROOT"), os.Getenv("AGENTOWL_RUNNER_SMOKE_IMAGE")
	if root == "" || image == "" {
		t.Skip("set AGENTOWL_RUNNER_SMOKE_ROOT and AGENTOWL_RUNNER_SMOKE_IMAGE on an idle dedicated Runner host")
	}
	mode := os.Getenv("AGENTOWL_RUNNER_SMOKE_RESOURCE_LIMITS")
	networkURL := os.Getenv("AGENTOWL_RUNNER_SMOKE_NETWORK_URL")
	if networkURL != "" {
		parsed, err := url.Parse(networkURL)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Scheme != "https" && parsed.Scheme != "http" {
			t.Fatal("network smoke URL must be public HTTP(S) without credentials, query parameters or fragment")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	engine := NewEngine(filepath.Join(root, "control", "docker.sock"))
	existing, err := engine.Managed(ctx)
	if err != nil || len(existing) != 0 {
		t.Fatalf("dedicated daemon must be idle: containers=%d error=%v", len(existing), err)
	}
	auth := &smokeAuthorizer{}
	var calls int
	model := modelFunc(func(_ context.Context, _ integration.ChatRequest) (integration.ChatResponse, error) {
		calls++
		response := integration.ChatResponse{FinishReason: "stop", Usage: integration.Usage{TotalTokens: 10}, Message: integration.ChatMessage{Role: "assistant", Content: "Applied the test edit."}}
		switch {
		case calls <= 105:
			response.Message.ToolCalls = []integration.ToolCall{{ID: fmt.Sprintf("read-%d", calls), Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}}
		case calls == 106:
			response.Message.ToolCalls = []integration.ToolCall{{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"main.go","content":"package main\n// smoke edit\n","executable":false}`)}}
		}
		if len(response.Message.ToolCalls) > 0 {
			response.Message.Content = ""
			response.FinishReason = "tool_calls"
		}
		return response, nil
	})
	c, err := NewController(Config{DataRoot: root, HostDataRoot: root, AllowedImages: []string{image}, ResourceLimits: mode, LeasePoll: 50 * time.Millisecond}, engine, auth, model)
	if err != nil {
		t.Fatal(err)
	}
	var specs []RunSpec
	var volumes []string
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := c.Close(cleanup); err != nil {
			t.Errorf("close: %v", err)
		}
		for _, spec := range specs {
			for {
				_, err := engine.Inspect(cleanup, containerName(spec.RunID))
				if dockerNotFound(err) {
					break
				}
				if cleanup.Err() != nil {
					t.Errorf("container not removed for %s: %v", spec.RunID, err)
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			_ = os.RemoveAll(c.runDir(spec.RunID))
			_ = os.RemoveAll(filepath.Join(root, "tasks", spec.TaskID))
		}
		for _, volume := range volumes {
			if err := engine.json(cleanup, "GET", "/volumes/"+url.PathEscape(volume), nil, nil); !dockerNotFound(err) {
				t.Errorf("disposable test volume was not removed: %s %v", volume, err)
			}
		}
	})
	newSpec := func(stage string) RunSpec {
		spec := specFor(stage)
		id := fmt.Sprintf("smoke-%d-%d", time.Now().UnixNano(), len(specs))
		spec.RunID, spec.TaskID, spec.AttemptID = id, id, id
		spec.Image = image
		spec.Limits.TimeoutSeconds = 20
		specs = append(specs, spec)
		return spec
	}
	wait := func(t *testing.T, spec RunSpec) Run {
		t.Helper()
		for {
			run, err := c.Inspect(spec.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if terminal(run.State) {
				return run
			}
			select {
			case <-ctx.Done():
				t.Fatal("Runner smoke timed out")
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	waitContainer := func(t *testing.T, spec RunSpec) Container {
		t.Helper()
		for {
			container, err := engine.Inspect(ctx, containerName(spec.RunID))
			if err == nil && container.State.Running {
				return container
			}
			if ctx.Err() != nil {
				t.Fatal("container did not start", ctx.Err())
			}
			if run, err := c.Inspect(spec.RunID); err == nil && terminal(run.State) {
				t.Fatalf("container ended before inspection: %+v", run)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	logFailure := func(t *testing.T, spec RunSpec, run Run) {
		t.Helper()
		body, _ := json.Marshal(run)
		t.Logf("run result: %s", body)
		archive, err := c.Collect(spec.RunID)
		if err != nil {
			return
		}
		defer archive.Close()
		gz, err := gzip.NewReader(archive)
		if err != nil {
			return
		}
		defer gz.Close()
		entries, err := readArchive(gz, 64<<20, 100)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.Path == "test.log" {
				log := entry.Data
				if len(log) > 8192 {
					log = log[len(log)-8192:]
				}
				t.Logf("test output: %s", log)
			}
		}
	}
	t.Run("edit_and_collect", func(t *testing.T) {
		spec := newSpec("edit")
		spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: true}
		if _, err := c.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		run := wait(t, spec)
		if run.State != "succeeded" || run.Result.OutputDigest == spec.InputDigest || run.Result.ToolCalls != 106 || run.Result.Tokens <= 200000 {
			t.Fatalf("edit failed: %+v", run)
		}
		archive, err := c.Collect(spec.RunID)
		if err != nil {
			t.Fatal(err)
		}
		defer archive.Close()
		gz, err := gzip.NewReader(archive)
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		entries, err := readArchive(gz, 64<<20, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Path == "workspace/main.go" && string(entry.Data) == "package main\n// smoke edit\n" {
				return
			}
		}
		t.Fatal("edited source missing from artifacts")
	})
	t.Run("isolated_test", func(t *testing.T) {
		spec := newSpec("test")
		spec.Argv = []string{"/bin/sh", "-ec", `test "$(id -u)" = 65532; test -r main.go; test ! -w main.go; test ! -w /; test ! -S /var/run/docker.sock; test ! -e /runner; test "$(ls /sys/class/net)" = lo; test "$(awk '/NoNewPrivs/ {print $2}' /proc/self/status)" = 1; printf smoke > /output/probe; printf smoke > /tmp/probe`}
		if _, err := c.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		run := wait(t, spec)
		if run.State != "succeeded" || !run.Result.EvidenceValid || run.Result.ExitCode == nil || *run.Result.ExitCode != 0 {
			t.Fatalf("test failed: %+v", run)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		spec := newSpec("test")
		spec.Argv = []string{"/bin/sleep", "30"}
		spec.Limits.TimeoutSeconds = 2
		if _, err := c.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		run := wait(t, spec)
		if run.State != "failed" || run.Result.EvidenceValid || !strings.Contains(run.Result.Error, "deadline exceeded") {
			t.Fatalf("timeout not enforced: %+v", run)
		}
	})
	t.Run("bridge_writable_test_volume", func(t *testing.T) {
		spec := newSpec("test")
		spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: true, TestWorkspaceWritable: true}
		command := `test "$(id -u)" = 65532; test -w main.go; test -w .; test ! -S /var/run/docker.sock; test "$(ls /sys/class/net | wc -l)" -gt 1; mkdir -p node_modules/probe build; printf dependency > node_modules/probe/index.js; printf output > build/result; if (printf forbidden > /var/tmp/agentowl-profile-probe) 2>/dev/null; then exit 91; fi`
		if networkURL != "" {
			command += `; wget -T 15 -O node_modules/connection-probe.tgz "$1"; test -s node_modules/connection-probe.tgz`
			spec.Limits.TimeoutSeconds = 60
		}
		spec.Argv = []string{"/bin/sh", "-ec", command + "; sleep 2", "agentowl-network-smoke", networkURL}
		if _, err := c.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		container := waitContainer(t, spec)
		var actual struct {
			Mounts     []struct{ Type, Name, Destination string }
			HostConfig struct {
				NetworkMode     string
				ReadonlyRootfs  bool
				Privileged      bool
				PublishAllPorts bool
				PortBindings    map[string]any
				Tmpfs           map[string]string
				Ulimits         []struct {
					Name string
				}
			}
		}
		if err := engine.json(ctx, "GET", "/containers/"+url.PathEscape(container.ID)+"/json", nil, &actual); err != nil {
			t.Fatal(err)
		}
		if actual.HostConfig.NetworkMode != "bridge" || !actual.HostConfig.ReadonlyRootfs || actual.HostConfig.Privileged || actual.HostConfig.PublishAllPorts || len(actual.HostConfig.PortBindings) != 0 {
			t.Fatalf("actual container isolation differs: %+v", actual.HostConfig)
		}
		for _, name := range []string{"/tmp", "/output"} {
			if actual.HostConfig.Tmpfs[name] != "rw,nosuid,nodev,size=0,nr_inodes=0,mode=1777" {
				t.Fatalf("unlimited tmpfs configuration changed: %+v", actual.HostConfig.Tmpfs)
			}
		}
		for _, limit := range actual.HostConfig.Ulimits {
			if limit.Name == "fsize" {
				t.Fatal("unlimited workspace retained a file-size ulimit")
			}
		}
		workspaceVolume := false
		for _, mount := range actual.Mounts {
			if mount.Type == "bind" {
				t.Fatal("writable test unexpectedly has a host bind")
			}
			if mount.Destination == "/workspace" && mount.Type == "volume" && mount.Name != "" {
				workspaceVolume = true
				volumes = append(volumes, mount.Name)
			}
		}
		if !workspaceVolume {
			t.Fatal("independent test volume missing")
		}
		run := wait(t, spec)
		if run.State != "succeeded" || !run.Result.EvidenceValid || run.Result.OutputDigest != spec.InputDigest {
			logFailure(t, spec, run)
			t.Fatalf("writable test did not preserve input evidence: %+v", run)
		}
		archive, err := c.Collect(spec.RunID)
		if err != nil {
			t.Fatal(err)
		}
		defer archive.Close()
		gz, err := gzip.NewReader(archive)
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
				found = string(entry.Data) == string(spec.Files[0].Data)
			}
			if strings.HasPrefix(entry.Path, "workspace/") && entry.Path != "workspace/main.go" {
				t.Fatal("generated file entered source archive", entry.Path)
			}
		}
		if !found {
			t.Fatal("approved source missing from archive")
		}
	})
	t.Run("writable_test_rejects_source_mutation", func(t *testing.T) {
		spec := newSpec("test")
		spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "none", ReadOnlyRootFS: true, TestWorkspaceWritable: true}
		spec.Argv = []string{"/bin/sh", "-ec", `printf changed > main.go`}
		if _, err := c.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		run := wait(t, spec)
		if run.State != "failed" || run.Result.EvidenceValid || run.Result.OutputDigest != spec.InputDigest || !strings.Contains(run.Result.Error, "test modified approved input") {
			logFailure(t, spec, run)
			t.Fatalf("changed test source accepted: %+v", run)
		}
	})
	t.Run("writable_rootfs", func(t *testing.T) {
		spec := newSpec("test")
		spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "none", ReadOnlyRootFS: false}
		spec.Argv = []string{"/bin/sh", "-ec", `test "$(id -u)" = 65532; test ! -w main.go; test ! -S /var/run/docker.sock; printf writable > /var/tmp/agentowl-profile-probe; test -s /var/tmp/agentowl-profile-probe`}
		if _, err := c.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if run := wait(t, spec); run.State != "succeeded" || !run.Result.EvidenceValid {
			t.Fatalf("rootfs setting not applied: %+v", run)
		}
	})
	t.Run("disabled_timeout_still_stops", func(t *testing.T) {
		spec := newSpec("test")
		spec.Limits.TimeoutSeconds = 0
		spec.Argv = []string{"/bin/sleep", "30"}
		if _, err := c.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		waitContainer(t, spec)
		if _, err := c.Stop(ctx, spec.RunID); err != nil {
			t.Fatal(err)
		}
		if run := wait(t, spec); run.State != "stopped" || run.Result.EvidenceValid {
			t.Fatalf("unlimited test ignored stop: %+v", run)
		}
	})
	t.Run("permission_revoked", func(t *testing.T) {
		spec := newSpec("test")
		spec.Limits.TimeoutSeconds = 0
		spec.Argv = []string{"/bin/sleep", "30"}
		if _, err := c.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		for {
			container, err := engine.Inspect(ctx, containerName(spec.RunID))
			if err == nil && container.State.Running {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("container did not start")
			case <-time.After(50 * time.Millisecond):
			}
		}
		auth.deny.Store(true)
		defer auth.deny.Store(false)
		run := wait(t, spec)
		if run.State != "stopped" || run.Result.EvidenceValid {
			t.Fatalf("revoked permission not stopped: %+v", run)
		}
	})
}
