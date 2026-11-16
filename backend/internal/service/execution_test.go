package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

func runArchive(t *testing.T, spec runner.RunSpec, remote runner.Run, files []runner.File) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	manifest := []Data{}
	for _, f := range files {
		manifest = append(manifest, Data{"path": f.Path, "mode": f.Mode, "size": len(f.Data), "sha256": Hash(f.Data)})
	}
	entries := []runner.File{{Path: "manifest.json", Mode: 0644, Data: JSON(manifest)}, {Path: "result.json", Mode: 0644, Data: JSON(remote)}, {Path: "diff.patch", Mode: 0644, Data: []byte("verified test fixture")}}
	if spec.Stage == "test" {
		entries = append(entries, runner.File{Path: "test.log", Mode: 0644, Data: []byte("go test ./...: PASS")})
	}
	for _, f := range files {
		entries = append(entries, runner.File{Path: "workspace/" + f.Path, Mode: f.Mode, Data: f.Data})
	}
	for _, f := range entries {
		if e := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: int64(f.Mode), Size: int64(len(f.Data)), Typeflag: tar.TypeReg}); e != nil {
			t.Fatal(e)
		}
		if _, e := tw.Write(f.Data); e != nil {
			t.Fatal(e)
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e := gz.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestDevelopmentStagesFreezeEvidenceAndRevoke(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	var mu sync.Mutex
	specs := map[string]runner.RunSpec{}
	runs := map[string]runner.Run{}
	archives := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "GET" && r.URL.Path == "/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(runner.Capabilities{ContextManifestVersion: 2, Reachable: true, Ready: true, SupportsResourceLimits: true, ResourceLimits: runner.ResourceLimitsRequired, MaxConcurrent: 1, SettingsConfigured: true, ModelConfigured: true})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/runs" {
			var spec runner.RunSpec
			if json.NewDecoder(r.Body).Decode(&spec) != nil {
				w.WriteHeader(400)
				return
			}
			specs[spec.RunID] = spec
			files := append([]runner.File{}, spec.Files...)
			if spec.Stage == "edit" {
				files[0].Data = []byte("fixed authentication\n")
			}
			zero := 0
			digest, _ := runner.SpecDigest(spec)
			run := runner.Run{RunID: spec.RunID, TaskID: spec.TaskID, AttemptID: spec.RunID, Epoch: spec.Epoch, Stage: spec.Stage, State: "succeeded", SpecDigest: digest, CreatedAt: time.Now(), Result: runner.Result{Summary: "Stage completed in protocol fixture", ExitCode: &zero, InputDigest: spec.InputDigest, OutputDigest: mustSnapshotDigest(files), TreeSHA: spec.TreeSHA, EvidenceValid: true, Tokens: 100}}
			runs[spec.RunID] = run
			archives[spec.RunID] = runArchive(t, spec, run, files)
			_ = json.NewEncoder(w).Encode(run)
			return
		}
		id := r.PathValue("id")
		if id == "" {
			parts := bytes.Split([]byte(r.URL.Path), []byte("/"))
			if len(parts) > 3 {
				id = string(parts[3])
			}
		}
		run, ok := runs[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(204)
			return
		}
		if bytes.HasSuffix([]byte(r.URL.Path), []byte("/artifacts")) {
			_, _ = w.Write(archives[id])
			return
		}
		_ = json.NewEncoder(w).Encode(run)
	}))
	defer server.Close()
	supportRuntimeMachine(t, s, server.URL)
	s.Model, _ = integration.NewModelClient(integration.ModelConfig{BaseURL: "https://model.example.test/v1", Model: "test"})
	repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "repositories", "", Data{"name": "Test repository", "owner": "owner", "repo": "repo", "enabled": true, "allowed_paths": []string{"auth"}, "runner_image": "local/test@sha256:" + fmt.Sprintf("%064d", 0), "test_argv": []string{"go", "test", "./..."}})
	})
	c := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveCase(ctx, tx, a, "", Data{"title": "Auth repair"}) })
	task := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ConfirmDevelopment(ctx, tx, a, Str(c, "id"), Data{"expected_version": Num(c, "version"), "repository_id": Str(repo, "id"), "goal": "Repair auth refresh", "acceptance_criteria": "Refresh succeeds"})
	})
	files := []runner.File{{Path: "auth/main.go", Mode: 0644, Data: []byte("broken authentication\n")}}
	if e := s.persistPreparedStep(ctx, task, repo, "plan", Data{"base_commit": fmt.Sprintf("%040d", 1), "base_tree": fmt.Sprintf("%040d", 2)}, files); e != nil {
		t.Fatal(e)
	}
	for _, stage := range []string{"plan", "edit", "test", "review"} {
		st, e := one(ctx, s.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE task_id=$1 AND stage=$2", Str(task, "id"), stage)
		if e != nil {
			t.Fatal(e)
		}
		ap, e := locked(ctx, s.DB, "approvals", Str(st, "approval_id"))
		if e != nil {
			t.Fatal(e)
		}
		if Str(ap, "status") != "pending" {
			t.Fatalf("stage %s did not require approval", stage)
		}
		if stage == "review" {
			input := Object(st, "input")
			var manifest runner.ContextManifest
			if err := json.Unmarshal(JSON(input["context_manifest"]), &manifest); err != nil {
				t.Fatal(err)
			}
			foundLog := false
			for _, ref := range manifest.Refs {
				foundLog = foundLog || ref.ID == "test_log" && ref.ArtifactID == Str(input, "test_log_artifact_id")
			}
			if !foundLog || Str(input, "test_log_artifact_id") == "" || Str(input, "test_digest") != Str(input, "input_digest") {
				t.Fatal("review did not receive test log reference bound to the tested snapshot")
			}
		}
		mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
			return s.Decide(ctx, tx, a, Str(ap, "id"), Data{"decision": "approve", "expected_version": Num(ap, "version")})
		})
		if e = s.executeApproval(ctx, Str(ap, "id")); e != nil {
			t.Fatal(stage, e)
		}
		st, e = locked(ctx, s.DB, "task_steps", Str(st, "id"))
		if e != nil {
			t.Fatal(e)
		}
		runID := Str(st, "attempt_id")
		r, e := locked(ctx, s.DB, "run_attempts", runID)
		if e != nil {
			t.Fatal(e)
		}
		permit, e := s.AuthorizeRun(ctx, Data{"phase": "admit", "run_id": runID, "attempt_id": runID, "task_id": Str(task, "id"), "step_id": Str(st, "id"), "epoch": 1, "action_digest": Str(r, "action_digest"), "spec_digest": Str(r, "spec_digest")})
		if e != nil || !Bool(permit, "allowed") {
			t.Fatalf("permit %v %v", permit, e)
		}
		if e = s.pollRun(ctx, runID); e != nil {
			t.Fatal(stage, e)
		}
		st, e = locked(ctx, s.DB, "task_steps", Str(st, "id"))
		if e != nil || Str(st, "status") != "succeeded" {
			t.Fatalf("stage=%s result=%v err=%v", stage, st, e)
		}
		next := stages[indexStage(stage)+1]
		if e = s.prepareStep(ctx, Data{"task_id": Str(task, "id"), "revision": 1, "epoch": 1, "stage": next}); e != nil {
			t.Fatal(next, e)
		}
	}
	publish, e := one(ctx, s.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE task_id=$1 AND stage='publish_pr'", Str(task, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if !Bool(Object(publish, "input"), "tests_passed") || Str(Object(publish, "input"), "input_digest") != Str(Object(publish, "input"), "test_digest") {
		t.Fatal("publish was not bound to tested snapshot")
	}
	current, e := locked(ctx, s.DB, "dev_tasks", Str(task, "id"))
	if e != nil {
		t.Fatal(e)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.TaskAction(ctx, tx, a, Str(task, "id"), Data{"expected_version": Num(current, "version"), "action": "pause"})
	})
	ap, e := locked(ctx, s.DB, "approvals", Str(publish, "approval_id"))
	if e != nil || Str(ap, "status") != "stale" {
		t.Fatalf("pause failed to invalidate approval: %v %v", ap, e)
	}
	var count int
	_ = s.DB.QueryRow(ctx, "SELECT count(*) FROM external_operations WHERE kind='publish_pr'").Scan(&count)
	if count != 0 {
		t.Fatal("unapproved PR operation created")
	}
}
