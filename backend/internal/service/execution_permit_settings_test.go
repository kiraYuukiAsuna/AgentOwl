package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func permitForPhase(run Data, phase string) Data {
	p := runPermitRequest(run)
	p["phase"] = phase
	return p
}

func savePermitSettings(ctx context.Context, s *Service, tx pgx.Tx, actor Actor, setting string) (Data, error) {
	if setting == "model" {
		return s.SaveModelSettings(ctx, tx, actor, Data{"expected_version": 0, "max_daily_tokens": 900000})
	}
	body := executionSettingsBody(1)
	body["network_mode"], body["read_only_rootfs"], body["test_workspace_writable"] = "bridge", false, true
	return s.SaveExecutionSettings(ctx, tx, actor, body)
}

func TestRunPermitSettingsAdmissionBoundary(t *testing.T) {
	for _, setting := range []string{"execution", "model"} {
		for _, prepared := range []bool{false, true} {
			name := setting + "/before_prepare"
			if prepared {
				name = setting + "/after_prepare"
			}
			t.Run(name, func(t *testing.T) {
				s, actor, _, step, run, _ := queuedSettingsRun(t)
				ctx := context.Background()
				permit, err := s.AuthorizeRun(ctx, permitForPhase(run, "admit"))
				if err != nil || !Bool(permit, "allowed") {
					t.Fatal("initial admission denied", permit, err)
				}
				current, err := locked(ctx, s.DB, "run_attempts", Str(run, "id"))
				if err != nil || Str(current, "status") != "queued" || current["started_at"] != nil {
					t.Fatal("read-only admit consumed the running boundary", current, err)
				}
				if prepared {
					permit, err = s.AuthorizeRun(ctx, permitForPhase(run, "prepare"))
					if err != nil || !Bool(permit, "allowed") {
						t.Fatal("prepare denied", permit, err)
					}
				}
				mutateTest(t, s, actor, func(tx pgx.Tx) (Data, error) {
					return savePermitSettings(ctx, s, tx, actor, setting)
				})
				for _, phase := range []string{"admit", "prepare", "renew"} {
					permit, err = s.AuthorizeRun(ctx, permitForPhase(run, phase))
					if err != nil || Bool(permit, "allowed") != prepared {
						t.Fatal("settings update crossed the wrong admission boundary", phase, permit, err)
					}
					if !prepared && Str(permit, "reason") != "settings_changed" {
						t.Fatal("stale admission must report a recoverable settings conflict", phase, permit)
					}
				}
				current, err = locked(ctx, s.DB, "run_attempts", Str(run, "id"))
				if err != nil || Hash(JSON(current["spec"])) != Hash(JSON(run["spec"])) {
					t.Fatal("settings update changed frozen run specification", err)
				}
				wantState := "queued"
				if prepared {
					wantState = "running"
				}
				if Str(current, "status") != wantState || (current["started_at"] != nil) != prepared {
					t.Fatal("invalid durable preparation state", current)
				}
				approval, err := locked(ctx, s.DB, "approvals", Str(step, "approval_id"))
				if err != nil || Str(approval, "status") != "approved" {
					t.Fatal("attempt must be reconciled before revoking its existing approval", approval, err)
				}
				permit, err = s.AuthorizeRun(ctx, permitForPhase(run, "arbitrary"))
				if err != nil || Bool(permit, "allowed") {
					t.Fatal("unknown permit phase was allowed", permit, err)
				}
			})
		}
	}
}

func TestRunPreparePermitSerializesWithSettingsSave(t *testing.T) {
	for _, setting := range []string{"execution", "model"} {
		t.Run(setting, func(t *testing.T) {
			s, actor, _, _, run, _ := queuedSettingsRun(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			config := s.DB.Config()
			app := "prepare_permit_" + ID()
			config.ConnConfig.RuntimeParams["application_name"] = app
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			authorizer := *s
			authorizer.DB = pool
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
			type result struct {
				permit Data
				err    error
			}
			done := make(chan result, 1)
			go func() {
				permit, err := authorizer.AuthorizeRun(ctx, permitForPhase(run, "prepare"))
				done <- result{permit, err}
			}()
			for {
				var waiting bool
				if err = s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND wait_event='advisory')", app).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case got := <-done:
					t.Fatalf("prepare permission passed an in-flight save: %v %v", got.permit, got.err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if _, err = savePermitSettings(ctx, s, saveTx, actor, setting); err != nil {
				t.Fatal(err)
			}
			if err = saveTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.err != nil || Bool(got.permit, "allowed") || Str(got.permit, "reason") != "settings_changed" {
				t.Fatal("prepare permission accepted obsolete settings", got.permit, got.err)
			}
			current, err := locked(ctx, s.DB, "run_attempts", Str(run, "id"))
			if err != nil || Str(current, "status") != "queued" || current["started_at"] != nil {
				t.Fatal("rejected preparation was recorded as running", current, err)
			}
		})
	}
}
