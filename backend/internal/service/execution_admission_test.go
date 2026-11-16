package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func queuedSettingsRun(t *testing.T) (*Service, Actor, Data, Data, Data, Data) {
	t.Helper()
	s, a := testService(t)
	ctx := context.Background()
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveExecutionSettings(ctx, tx, a, executionSettingsBody(0))
	})
	repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "repositories", "", Data{"name": "queued admission fixture", "owner": "owner", "repo": "repo", "enabled": true, "allowed_paths": []string{"auth"}, "runner_image": "local/test@sha256:" + fmt.Sprintf("%064d", 0)})
	})
	_, machineURL := supportCapabilityServer(t, runner.Capabilities{ContextManifestVersion: 2, Reachable: true, Ready: true, SupportsResourceLimits: true, ResourceLimits: runner.ResourceLimitsDisabled, MaxConcurrent: 1, SettingsConfigured: true, ModelConfigured: true})
	supportRuntimeMachine(t, s, machineURL)
	issue := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveCase(ctx, tx, a, "", Data{"title": "Auth queued settings test"})
	})
	task := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ConfirmDevelopment(ctx, tx, a, Str(issue, "id"), Data{"expected_version": Num(issue, "version"), "repository_id": Str(repo, "id"), "goal": "fix", "acceptance_criteria": "passes"})
	})
	if err := s.persistPreparedStep(ctx, task, repo, "plan", Data{"base_commit": fmt.Sprintf("%040d", 1), "base_tree": fmt.Sprintf("%040d", 2)}, []runner.File{{Path: "auth/main.go", Mode: 0644, Data: []byte("source")}}); err != nil {
		t.Fatal(err)
	}
	step, err := one(ctx, s.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE task_id=$1", Str(task, "id"))
	if err != nil {
		t.Fatal(err)
	}
	approval, err := locked(ctx, s.DB, "approvals", Str(step, "approval_id"))
	if err != nil {
		t.Fatal(err)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.Decide(ctx, tx, a, Str(approval, "id"), Data{"decision": "approve", "expected_version": Num(approval, "version")})
	})
	if err = s.executeApproval(ctx, Str(approval, "id")); err != nil {
		t.Fatal(err)
	}
	step, err = locked(ctx, s.DB, "task_steps", Str(step, "id"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := locked(ctx, s.DB, "run_attempts", Str(step, "attempt_id"))
	if err != nil || Str(run, "status") != "queued" || Str(step, "status") != "running" {
		t.Fatalf("queued attempt: %v %v", run, err)
	}
	return s, a, task, step, run, repo
}
func setRunMachineURL(t *testing.T, s *Service, run Data, baseURL string) {
	t.Helper()
	machineID := Str(run, "runtime_machine_id")
	if machineID == "" {
		t.Fatal("run is not bound to a Runtime Machine")
	}
	if _, err := s.DB.Exec(context.Background(), "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", machineID, JSON(Data{"base_url": baseURL})); err != nil {
		t.Fatal(err)
	}
}

func runPermitRequest(run Data) Data {
	return Data{"run_id": Str(run, "id"), "attempt_id": Str(run, "id"), "task_id": Str(run, "task_id"), "step_id": Str(run, "step_id"), "epoch": Num(run, "epoch"), "action_digest": Str(run, "action_digest"), "spec_digest": Str(run, "spec_digest")}
}

func TestQueuedRunSettingsConflictCanResume(t *testing.T) {
	for _, setting := range []string{"execution", "model"} {
		t.Run(setting, func(t *testing.T) {
			s, a, task, step, run, repo := queuedSettingsRun(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if setting == "execution" {
				mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
					return s.SaveExecutionSettings(ctx, tx, a, executionSettingsBody(1))
				})
			} else {
				saveModelSettingTest(t, s, a, Data{"expected_version": 0, "max_daily_tokens": 900000})
			}
			approval, err := locked(ctx, s.DB, "approvals", Str(step, "approval_id"))
			if err != nil || Str(approval, "status") != "approved" {
				t.Fatalf("attempt approval should remain until admission is reconciled: %v %v", approval, err)
			}
			var starts, inspections atomic.Int32
			callback := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/runs" {
					starts.Add(1)
					var spec runner.RunSpec
					if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
						t.Error(err)
					}
					settings, err := s.RunnerSettings(ctx)
					if err != nil || spec.ExecutionVersion == Num(settings, "execution_version") && spec.ModelVersion == Num(Object(settings, "model"), "version") {
						t.Errorf("fixture did not reject stale settings: %v", err)
					}
					w.WriteHeader(http.StatusConflict)
					return
				}
				if r.Method == http.MethodGet && inspections.Add(1) == 2 {
					// The absence check must hold the exact row used by prepare's
					// authorization callback, not merely check it before the GET.
					_, err := s.DB.Exec(ctx, "SELECT id FROM run_attempts WHERE id=$1 FOR UPDATE NOWAIT", Str(run, "id"))
					var pgerr *pgconn.PgError
					if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
						t.Errorf("absence check did not fence authorization: %v", err)
					}
					go func() {
						permit, err := s.AuthorizeRun(ctx, permitForPhase(run, "prepare"))
						if err == nil && Bool(permit, "allowed") {
							err = errors.New("concurrent prepare received a permit for a blocked attempt")
						}
						callback <- err
					}()
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			setRunMachineURL(t, s, run, server.URL)
			if err = s.pollRun(ctx, Str(run, "id")); err != nil {
				t.Fatal(err)
			}
			if err = <-callback; err != nil {
				t.Fatal(err)
			}
			for table, id := range map[string]string{"run_attempts": Str(run, "id"), "task_steps": Str(step, "id"), "dev_tasks": Str(task, "id")} {
				row, err := locked(ctx, s.DB, table, id)
				if err != nil || Str(row, "status") != "blocked" || Str(row, "error") == "" {
					t.Fatalf("%s not blocked with a reason: %v %v", table, row, err)
				}
			}
			approval, err = locked(ctx, s.DB, "approvals", Str(step, "approval_id"))
			if err != nil || Str(approval, "status") != "stale" {
				t.Fatalf("approval not invalidated: %v %v", approval, err)
			}
			budget, err := one(ctx, s.DB, "SELECT to_jsonb(b) FROM budget_reservations b WHERE id=$1", Str(run, "id"))
			if err != nil || Str(budget, "status") != "settled" || budget["actual"] == nil || Num(budget, "actual") != 0 {
				t.Fatalf("unused budget not settled: %v %v", budget, err)
			}
			if err = s.pollRun(ctx, Str(run, "id")); err != nil || starts.Load() != 1 || inspections.Load() != 2 {
				t.Fatalf("terminal poll restarted a blocked attempt: %v starts=%d inspections=%d", err, starts.Load(), inspections.Load())
			}
			task, err = locked(ctx, s.DB, "dev_tasks", Str(task, "id"))
			if err != nil {
				t.Fatal(err)
			}
			task = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.TaskAction(ctx, tx, a, Str(task, "id"), Data{"expected_version": Num(task, "version"), "action": "resume"})
			})
			if Num(task, "epoch") != Num(run, "epoch")+1 || Str(task, "status") != "queued" {
				t.Fatalf("task did not resume: %v", task)
			}
			if err = s.persistPreparedStep(ctx, task, repo, "plan", Data{"base_commit": fmt.Sprintf("%040d", 1), "base_tree": fmt.Sprintf("%040d", 2)}, []runner.File{{Path: "auth/main.go", Mode: 0644, Data: []byte("source")}}); err != nil {
				t.Fatal(err)
			}
			prepared, err := locked(ctx, s.DB, "task_steps", Str(step, "id"))
			if err != nil || Str(prepared, "attempt_id") != "" || Str(prepared, "approval_id") == Str(step, "approval_id") || Str(prepared, "status") != "pending_approval" {
				t.Fatalf("resume did not require a new approval: %v %v", prepared, err)
			}
			permit, err := s.AuthorizeRun(ctx, permitForPhase(run, "admit"))
			if err != nil || Bool(permit, "allowed") {
				t.Fatalf("old attempt reauthorized after resume: %v %v", permit, err)
			}
		})
	}
}

func TestQueuedRunConflictRetainsUnconfirmedBudget(t *testing.T) {
	for _, scenario := range []string{"remote_exists", "inspect_unavailable", "start_unavailable", "lease_lost", "state_advanced"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, task, step, run, _ := queuedSettingsRun(t)
			ctx := context.Background()
			jobID, lease := ID(), ID()
			if scenario == "lease_lost" {
				if _, err := s.DB.Exec(ctx, "INSERT INTO jobs(id,kind,dedupe_key,payload,status,lease_token,lease_until) VALUES($1::uuid,'poll_run',$1::text,'{}','running',$2,now()+interval '1 minute')", jobID, lease); err != nil {
					t.Fatal(err)
				}
				ctx = context.WithValue(ctx, leaseKey{}, Data{"id": jobID, "lease_token": lease})
			}
			var inspections atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					if scenario == "start_unavailable" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if scenario == "lease_lost" {
						if _, err := s.DB.Exec(ctx, "UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1", jobID); err != nil {
							t.Error(err)
						}
					}
					if scenario == "state_advanced" {
						if _, err := s.DB.Exec(ctx, "UPDATE run_attempts SET status='running' WHERE id=$1", Str(run, "id")); err != nil {
							t.Error(err)
						}
					}
					w.WriteHeader(http.StatusConflict)
					return
				}
				if inspections.Add(1) == 1 {
					w.WriteHeader(http.StatusNotFound)
				} else if scenario == "remote_exists" {
					_ = json.NewEncoder(w).Encode(runner.Run{RunID: Str(run, "id"), TaskID: Str(task, "id"), State: "running", Epoch: Num(run, "epoch"), SpecDigest: Str(run, "spec_digest")})
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			setRunMachineURL(t, s, run, server.URL)
			if err := s.pollRun(ctx, Str(run, "id")); err == nil {
				t.Fatal("unconfirmed admission was treated as safely blocked")
			}
			attempt, err := locked(ctx, s.DB, "run_attempts", Str(run, "id"))
			wantState := "queued"
			if scenario == "state_advanced" {
				wantState = "running"
			}
			if err != nil || Str(attempt, "status") != wantState {
				t.Fatalf("uncertain attempt changed: %v %v", attempt, err)
			}
			for table, id := range map[string]string{"task_steps": Str(step, "id"), "dev_tasks": Str(task, "id")} {
				row, err := locked(ctx, s.DB, table, id)
				if err != nil || Str(row, "status") != "running" {
					t.Fatalf("%s changed despite uncertainty: %v %v", table, row, err)
				}
			}
			budget, err := one(ctx, s.DB, "SELECT to_jsonb(b) FROM budget_reservations b WHERE id=$1", Str(run, "id"))
			if err != nil || Str(budget, "status") != "reserved" || budget["actual"] != nil {
				t.Fatalf("uncertain budget was released: %v %v", budget, err)
			}
			approval, err := locked(ctx, s.DB, "approvals", Str(step, "approval_id"))
			if err != nil || Str(approval, "status") != "approved" {
				t.Fatalf("uncertain approval was invalidated: %v %v", approval, err)
			}
		})
	}
}

func TestQueuedRunLateAdmissionCannotResurrectBlockedAttempt(t *testing.T) {
	s, _, task, _, run, _ := queuedSettingsRun(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	firstStart := make(chan struct{})
	releaseStart := make(chan struct{})
	var starts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if starts.Add(1) != 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		close(firstStart)
		select {
		case <-releaseStart:
		case <-ctx.Done():
			return
		}
		_ = json.NewEncoder(w).Encode(runner.Run{RunID: Str(run, "id"), TaskID: Str(task, "id"), State: "queued", Epoch: Num(run, "epoch"), SpecDigest: Str(run, "spec_digest")})
	}))
	defer server.Close()
	setRunMachineURL(t, s, run, server.URL)
	firstPoll := make(chan error, 1)
	go func() { firstPoll <- s.pollRun(ctx, Str(run, "id")) }()
	select {
	case <-firstStart:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := s.pollRun(ctx, Str(run, "id")); err != nil {
		cancel()
		t.Fatal(err)
	}
	close(releaseStart)
	if err := <-firstPoll; err != nil {
		t.Fatal(err)
	}
	attempt, err := locked(ctx, s.DB, "run_attempts", Str(run, "id"))
	if err != nil || Str(attempt, "status") != "blocked" || attempt["started_at"] != nil {
		t.Fatalf("late admission resurrected revoked attempt: %v %v", attempt, err)
	}
	permit, err := s.AuthorizeRun(ctx, permitForPhase(run, "admit"))
	if err != nil || Bool(permit, "allowed") {
		t.Fatalf("late admission may execute: %v %v", permit, err)
	}
}
