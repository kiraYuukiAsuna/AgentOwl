package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPrepareStepSerializesWithSettingsSave(t *testing.T) {
	for _, setting := range []string{"execution", "model"} {
		t.Run(setting, func(t *testing.T) {
			s, a := testService(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.SaveExecutionSettings(ctx, tx, a, executionSettingsBody(0))
			})
			repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.SaveConfig(ctx, tx, a, "repositories", "", Data{"name": "prepare settings fixture", "owner": "owner", "repo": "repo", "enabled": true, "allowed_paths": []string{"auth"}})
			})
			issue := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.SaveCase(ctx, tx, a, "", Data{"title": "Auth prepare settings test"})
			})
			task := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.ConfirmDevelopment(ctx, tx, a, Str(issue, "id"), Data{"expected_version": Num(issue, "version"), "repository_id": Str(repo, "id"), "goal": "fix", "acceptance_criteria": "passes"})
			})
			// Give preparation a distinct connection identity so the test can
			// observe its actual advisory-lock wait instead of relying on sleeps.
			cfg := s.DB.Config()
			appName := "prepare_settings_" + ID()
			cfg.ConnConfig.RuntimeParams["application_name"] = appName
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			preparer := *s
			preparer.DB = pool
			saveTx, err := s.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer saveTx.Rollback(context.Background())
			lockSQL := "SELECT pg_advisory_xact_lock(728690107)"
			if setting == "model" {
				lockSQL = "SELECT pg_advisory_xact_lock(hashtextextended('system_settings:model',0))"
			}
			if _, err = saveTx.Exec(ctx, lockSQL); err != nil {
				t.Fatal(err)
			}
			prepared := make(chan error, 1)
			go func() {
				prepared <- preparer.persistPreparedStep(ctx, task, repo, "plan", Data{"base_commit": fmt.Sprintf("%040d", 1), "base_tree": fmt.Sprintf("%040d", 2)}, []runner.File{{Path: "auth/main.go", Mode: 0644, Data: []byte("source")}})
			}()
			for {
				var waiting bool
				if err = s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND wait_event='advisory')", appName).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err = <-prepared:
					t.Fatalf("preparation passed an in-flight %s save: %v", setting, err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if setting == "execution" {
				body := executionSettingsBody(1)
				body["timeout_seconds"], body["max_tokens"] = 73, 2345
				_, err = s.SaveExecutionSettings(ctx, saveTx, a, body)
			} else {
				_, err = s.SaveModelSettings(ctx, saveTx, a, Data{"expected_version": 0, "max_daily_tokens": 900000})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = saveTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-prepared; err != nil {
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
			input, action := Object(step, "input"), Object(approval, "action")
			wantExecution, wantModel := int64(2), int64(0)
			if setting == "model" {
				wantExecution, wantModel = 1, 1
			}
			for _, frozen := range []Data{input, action} {
				if Num(frozen, "execution_settings_version") != wantExecution || Num(frozen, "model_settings_version") != wantModel {
					t.Fatalf("approval froze old settings after save: %v", frozen)
				}
				if setting == "execution" && (Num(Object(frozen, "limits"), "timeout_seconds") != 73 || Num(Object(frozen, "limits"), "max_tokens") != 2345) {
					t.Fatalf("approval versions and limits differ: %v", frozen)
				}
			}
			// The approval must be usable, rather than leave pending_approval
			// with stale settings that neither approval nor resume can repair.
			mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.Decide(ctx, tx, a, Str(approval, "id"), Data{"decision": "approve", "expected_version": Num(approval, "version")})
			})
		})
	}
}
