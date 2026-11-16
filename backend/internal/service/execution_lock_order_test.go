package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunLifecycleLocksRunBeforeDependentRows(t *testing.T) {
	for _, operation := range []string{"poll", "poll_stopped", "finish", "pause", "revoke", "revise"} {
		t.Run(operation, func(t *testing.T) {
			s, actor, task, step, run, _ := queuedSettingsRun(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var inspections, acknowledgements atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost {
					acknowledgements.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				inspections.Add(1)
				_ = json.NewEncoder(w).Encode(runner.Run{RunID: Str(run, "id"), TaskID: Str(task, "id"), State: "queued", Epoch: Num(run, "epoch"), SpecDigest: Str(run, "spec_digest")})
			}))
			defer server.Close()
			setRunMachineURL(t, s, run, server.URL)
			spec, err := s.runSpec(ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			approval, err := locked(ctx, s.DB, "approvals", Str(step, "approval_id"))
			if err != nil {
				t.Fatal(err)
			}
			task, err = locked(ctx, s.DB, "dev_tasks", Str(task, "id"))
			if err != nil {
				t.Fatal(err)
			}
			config := s.DB.Config()
			app := "run_order_" + ID()
			config.ConnConfig.RuntimeParams["application_name"] = app
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			worker := *s
			worker.DB = pool
			blocker, err := s.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err = locked(ctx, blocker, "run_attempts", Str(run, "id")); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				switch operation {
				case "poll", "poll_stopped":
					done <- worker.pollRun(ctx, Str(run, "id"))
				case "finish":
					done <- worker.finishRun(ctx, run, spec, runner.Run{State: "failed"}, spec.Files, "", nil)
				default:
					done <- worker.transaction(ctx, func(tx pgx.Tx) error {
						var err error
						switch operation {
						case "pause":
							_, err = worker.TaskAction(ctx, tx, actor, Str(task, "id"), Data{"expected_version": Num(task, "version"), "action": "pause"})
						case "revoke":
							_, err = worker.Decide(ctx, tx, actor, Str(approval, "id"), Data{"expected_version": Num(approval, "version"), "decision": "revoke"})
						case "revise":
							_, err = worker.ReviseTask(ctx, tx, actor, Str(task, "id"), Data{"expected_version": Num(task, "version"), "goal": "changed", "acceptance_criteria": "passes"})
						}
						return err
					})
				}
			}()
			for {
				var waiting bool
				if err = s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')", app).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err = <-done:
					t.Fatalf("operation did not wait for its run row: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			// This is the other side of the former deadlock: holding the run row
			// must still allow taking its step, approval and task without waiting.
			for table, id := range map[string]string{"task_steps": Str(step, "id"), "approvals": Str(approval, "id"), "dev_tasks": Str(task, "id")} {
				if _, err = blocker.Exec(ctx, "SELECT id FROM "+table+" WHERE id=$1 FOR UPDATE NOWAIT", id); err != nil {
					t.Fatalf("%s was locked before the run row: %v", table, err)
				}
			}
			if operation == "poll_stopped" {
				if _, err = blocker.Exec(ctx, "UPDATE run_attempts SET status='stopped' WHERE id=$1", Str(run, "id")); err != nil {
					t.Fatal(err)
				}
			}
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-done
			if operation == "revise" {
				if err == nil {
					t.Fatal("revision ignored the unsettled attempt")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if operation == "poll_stopped" && (inspections.Load() != 0 || acknowledgements.Load() != 1) {
				t.Fatal("poll used its stale queued snapshot after the run stopped", inspections.Load(), acknowledgements.Load())
			}
		})
	}
}

func TestLateRunCompletionPreservesTerminalState(t *testing.T) {
	s, _, task, step, run, _ := queuedSettingsRun(t)
	ctx := context.Background()
	spec, err := s.runSpec(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, "UPDATE run_attempts SET status='stopped' WHERE id=$1", Str(run, "id")); err != nil {
		t.Fatal(err)
	}
	task, err = locked(ctx, s.DB, "dev_tasks", Str(task, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.finishRun(ctx, run, spec, runner.Run{State: "succeeded", Result: runner.Result{EvidenceValid: true, InputDigest: spec.InputDigest}}, spec.Files, "", nil); err != nil {
		t.Fatal(err)
	}
	current, err := locked(ctx, s.DB, "run_attempts", Str(run, "id"))
	if err != nil || Str(current, "status") != "stopped" {
		t.Fatal("late result resurrected a stopped run", current, err)
	}
	for table, before := range map[string]Data{"task_steps": step, "dev_tasks": task} {
		after, err := locked(ctx, s.DB, table, Str(before, "id"))
		if err != nil || Num(after, "version") != Num(before, "version") || Str(after, "status") != Str(before, "status") {
			t.Fatal("late result advanced a settled execution", table, err)
		}
	}
}
