package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestExecutionApprovalSerializesWithSettingsSave(t *testing.T) {
	for _, stage := range []string{"plan", "publish_pr"} {
		t.Run(stage, func(t *testing.T) {
			f := newDevelopmentApprovalFixture(t, "auto", 30)
			_, approval := f.prepare(t, stage)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cfg := f.s.DB.Config()
			appName := "execute_settings_" + ID()
			cfg.ConnConfig.RuntimeParams["application_name"] = appName
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			executor := *f.s
			executor.DB = pool
			saveTx, err := f.s.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer saveTx.Rollback(context.Background())
			if _, err = saveTx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690107)"); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- executor.executeApproval(ctx, Str(approval, "id")) }()
			for {
				var waiting bool
				if err = f.s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND wait_event='advisory')", appName).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err = <-done:
					t.Fatalf("execution passed an in-flight settings save: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if _, err = f.s.SaveExecutionSettings(ctx, saveTx, f.a, executionSettingsBody(1)); err != nil {
				t.Fatal(err)
			}
			if err = saveTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			if supportCount(t, f.s, "SELECT count(*) FROM run_attempts WHERE task_id=$1", Str(f.task, "id")) != 0 ||
				supportCount(t, f.s, "SELECT count(*) FROM external_operations WHERE approval_id=$1", Str(approval, "id")) != 0 {
				t.Fatal("invalidated approval created an execution")
			}
		})
	}
}

func TestQueuedResponseKeepsPreparedRunFrozen(t *testing.T) {
	s, a, _, _, run, _ := queuedSettingsRun(t)
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var spec runner.RunSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		permission := runPermitRequest(run)
		permission["phase"] = "prepare"
		permit, err := s.AuthorizeRun(ctx, permission)
		if err != nil || !Bool(permit, "allowed") {
			t.Errorf("prepare permit rejected: %v %v", permit, err)
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(runner.Run{RunID: spec.RunID, TaskID: spec.TaskID, SpecDigest: Str(run, "spec_digest"), Epoch: spec.Epoch, State: "queued"})
	}))
	defer server.Close()
	setRunMachineURL(t, s, run, server.URL)
	if err := s.pollRun(ctx, Str(run, "id")); err != nil {
		t.Fatal(err)
	}
	current, err := locked(ctx, s.DB, "run_attempts", Str(run, "id"))
	if err != nil || Str(current, "status") != "running" {
		t.Fatalf("late response lost preparation grant: %v %v", current, err)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveExecutionSettings(ctx, tx, a, executionSettingsBody(1)) })
	permission := runPermitRequest(run)
	permission["phase"] = "renew"
	permit, err := s.AuthorizeRun(ctx, permission)
	if err != nil || !Bool(permit, "allowed") {
		t.Fatalf("started run lost its frozen configuration: %v %v", permit, err)
	}
}
