package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

func executionSettingsBody(version int64) Data {
	b := defaultExecutionSettings()
	for _, key := range []string{"version", "updated_at", "updated_by"} {
		delete(b, key)
	}
	b["expected_version"] = version
	b["resource_limits"] = "disabled"
	return b
}

func TestExecutionSettingsValidation(t *testing.T) {
	for _, change := range []Data{
		{"resource_limits": "auto"}, {"max_concurrent": 2}, {"timeout_seconds": -1}, {"memory_mb": 8193},
		{"cpu_millis": 99}, {"pids": 513}, {"workspace_mb": -1}, {"max_files": -1},
		{"max_tool_calls": -1}, {"max_tokens": -1}, {"artifact_mb": -1}, {"timeout_seconds": 1.5},
		{"max_tokens": "1000"}, {"host_path": "/etc"}, {"network_mode": "host"}, {"approval_mode": "off"}, {"approval_mode": "manual"}, {"approval_mode": "auto"},
		{"read_only_rootfs": "false"}, {"test_workspace_writable": 1},
	} {
		b := executionSettingsBody(0)
		for k, v := range change {
			b[k] = v
		}
		if _, err := validateExecutionSettings(b); err == nil {
			t.Fatalf("accepted invalid settings: %v", change)
		}
	}
	b := executionSettingsBody(0)
	b["resource_limits"], b["max_concurrent"] = "required", 4
	b["timeout_seconds"] = 0
	b["network_mode"], b["read_only_rootfs"], b["test_workspace_writable"] = "bridge", false, true
	if value, err := validateExecutionSettings(b); err != nil {
		t.Fatal(err)
	} else if _, exists := value["approval_mode"]; exists {
		t.Fatal("retired override persisted")
	}
}

func TestExecutionSettingsPersistPermissionsAndCAS(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	initial, err := s.GetExecutionSettings(ctx, admin)
	if err != nil || Num(initial, "version") != 0 || Str(initial, "resource_limits") != "required" {
		t.Fatalf("defaults: %v %v", initial, err)
	}
	_, _, err = s.Mutate(ctx, Actor{ID: admin.ID, Role: "viewer"}, ID(), ID(), func(tx pgx.Tx) (Data, int, error) {
		v, e := s.SaveExecutionSettings(ctx, tx, Actor{ID: admin.ID, Role: "viewer"}, executionSettingsBody(0))
		return v, 200, e
	})
	if err != Forbidden {
		t.Fatalf("viewer save: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, e := s.Mutate(ctx, admin, ID(), ID(), func(tx pgx.Tx) (Data, int, error) {
				b := executionSettingsBody(0)
				b["timeout_seconds"], b["max_tokens"] = 37, 2048
				v, e := s.SaveExecutionSettings(ctx, tx, admin, b)
				return v, 200, e
			})
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	succeeded, conflicts := 0, 0
	for e := range errs {
		if e == nil {
			succeeded++
		} else if e == Conflict {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if succeeded != 1 || conflicts != 1 {
		t.Fatalf("CAS: success=%d conflict=%d", succeeded, conflicts)
	}
	saved, err := s.GetExecutionSettings(ctx, Actor{ID: admin.ID, Role: "viewer"})
	if err != nil || Num(saved, "version") != 1 || Num(saved, "timeout_seconds") != 37 || Num(saved, "max_tokens") != 2048 {
		t.Fatalf("saved: %v %v", saved, err)
	}
	limits := executionLimits(saved)
	if limits.TimeoutSeconds != 37 || limits.MaxTokens != 2048 || limits.MemoryBytes != 1<<30 {
		t.Fatalf("wire limits: %+v", limits)
	}
}

func TestExecutionSettingsProfileRoundTrip(t *testing.T) {
	s, admin := testService(t)
	ctx := context.Background()
	got, err := s.GetExecutionSettings(ctx, admin)
	if err != nil || Str(got, "network_mode") != "none" || !Bool(got, "read_only_rootfs") || Bool(got, "test_workspace_writable") || Str(got, "approval_mode") != "" {
		t.Fatalf("defaults: %v %v", got, err)
	}
	mutateTest(t, s, admin, func(tx pgx.Tx) (Data, error) {
		b := executionSettingsBody(0)
		b["network_mode"], b["read_only_rootfs"], b["test_workspace_writable"], b["timeout_seconds"] = "bridge", false, true, 0
		return s.SaveExecutionSettings(ctx, tx, admin, b)
	})
	got, err = s.GetExecutionSettings(ctx, admin)
	if err != nil || Num(got, "timeout_seconds") != 0 || Str(got, "approval_mode") != "" {
		t.Fatalf("saved configuration: %v %v", got, err)
	}
	callback, err := s.RunnerSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wire runner.Settings
	if err := json.Unmarshal(JSON(callback), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ExecutionVersion != 1 || wire.ExecutionProfile.NetworkMode != "bridge" || wire.ExecutionProfile.ReadOnlyRootFS || !wire.ExecutionProfile.TestWorkspaceWritable {
		t.Fatalf("runner profile: %+v", wire.ExecutionProfile)
	}
}

func TestExecutionSettingsRejectUnsupportedHost(t *testing.T) {
	s, admin := testService(t)
	_, machineURL := supportCapabilityServer(t, runner.Capabilities{ContextManifestVersion: 2, Reachable: true, Rootless: true, FilesystemReady: true, CgroupVersion: "1"})
	supportRuntimeMachine(t, s, machineURL)
	b := executionSettingsBody(0)
	b["resource_limits"] = "required"
	_, _, err := s.Mutate(context.Background(), admin, ID(), ID(), func(tx pgx.Tx) (Data, int, error) {
		v, e := s.SaveExecutionSettings(context.Background(), tx, admin, b)
		return v, 200, e
	})
	if problem, ok := err.(*Error); !ok || problem.Code != "resource_limits_unsupported" {
		t.Fatalf("unsupported host save: %v", err)
	}
}

func TestExecutionSettingsFreezeApprovalAndInvalidateUnstarted(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	settings := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		b := executionSettingsBody(0)
		b["timeout_seconds"], b["max_tokens"] = 23, 1234
		b["network_mode"], b["read_only_rootfs"], b["test_workspace_writable"] = "bridge", false, true
		return s.SaveExecutionSettings(ctx, tx, a, b)
	})
	repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "repositories", "", Data{"name": "settings fixture", "owner": "owner", "repo": "repo", "enabled": true, "allowed_paths": []string{"auth"}})
	})
	newTask := func() (Data, Data, Data) {
		issue := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveCase(ctx, tx, a, "", Data{"title": "Auth settings test"}) })
		task := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
			return s.ConfirmDevelopment(ctx, tx, a, Str(issue, "id"), Data{"expected_version": Num(issue, "version"), "repository_id": Str(repo, "id"), "goal": "fix", "acceptance_criteria": "passes"})
		})
		if err := s.persistPreparedStep(ctx, task, repo, "plan", Data{"base_commit": fmt.Sprintf("%040d", 1), "base_tree": fmt.Sprintf("%040d", 2)}, []runner.File{{Path: "auth/main.go", Mode: 0644, Data: []byte("source")}}); err != nil {
			t.Fatal(err)
		}
		step, _ := one(ctx, s.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE task_id=$1", Str(task, "id"))
		approval, _ := locked(ctx, s.DB, "approvals", Str(step, "approval_id"))
		return task, step, approval
	}
	waiting, step, approval := newTask()
	input, action := Object(step, "input"), Object(approval, "action")
	if Num(Object(input, "limits"), "timeout_seconds") != 23 || Num(Object(action, "limits"), "max_tokens") != 1234 || Str(action, "resource_limits") != "disabled" || Num(action, "execution_settings_version") != Num(settings, "version") {
		t.Fatalf("settings not bound to approval: %v", action)
	}
	for _, frozen := range []Data{input, action} {
		profile := Object(frozen, "execution_profile")
		if Str(profile, "network_mode") != "bridge" || Bool(profile, "read_only_rootfs") || !Bool(profile, "test_workspace_writable") || Str(frozen, "approval_mode") != "policy" {
			t.Fatalf("execution profile not bound to approval: %v", frozen)
		}
	}
	running, runningStep, runningApproval := newTask()
	if _, err := s.DB.Exec(ctx, "UPDATE task_steps SET attempt_id=$2,status='running' WHERE id=$1", Str(runningStep, "id"), ID()); err != nil {
		t.Fatal(err)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		b := executionSettingsBody(Num(settings, "version"))
		b["timeout_seconds"] = 99
		return s.SaveExecutionSettings(ctx, tx, a, b)
	})
	updated, _ := locked(ctx, s.DB, "approvals", Str(approval, "id"))
	waiting, _ = locked(ctx, s.DB, "dev_tasks", Str(waiting, "id"))
	stillRunning, _ := locked(ctx, s.DB, "approvals", Str(runningApproval, "id"))
	running, _ = locked(ctx, s.DB, "dev_tasks", Str(running, "id"))
	if Str(updated, "status") != "stale" || Str(waiting, "status") != "blocked" || Str(stillRunning, "status") != "pending" || Str(running, "status") == "blocked" {
		t.Fatalf("invalidation affected wrong tasks: waiting=%s/%s running=%s/%s", Str(updated, "status"), Str(waiting, "status"), Str(stillRunning, "status"), Str(running, "status"))
	}
}
