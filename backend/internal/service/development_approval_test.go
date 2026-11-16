package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type developmentApprovalFixture struct {
	s        *Service
	a        Actor
	task     Data
	repo     Data
	files    []runner.File
	baseline string
}

func newDevelopmentApprovalFixture(t *testing.T, mode string, timeout int) *developmentApprovalFixture {
	t.Helper()
	s, a := testService(t)
	ctx := context.Background()
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		b := executionSettingsBody(0)
		b["timeout_seconds"] = timeout
		// These tests exercise finite policies, independently of unlimited defaults.
		b["max_tokens"], b["max_files"] = 32000, 20000
		return s.SaveExecutionSettings(ctx, tx, a, b)
	})
	repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "repositories", "", Data{"name": "approval fixture", "owner": "owner", "repo": "repo",
			"enabled": true, "allowed_paths": []string{"auth"}, "runner_image": "local/test@sha256:" + strings.Repeat("a", 64),
			"test_argv": []string{"go", "test", "./..."}})
	})
	issue := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveCase(ctx, tx, a, "", Data{"title": "Auth approval fixture"})
	})
	task := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.ConfirmDevelopment(ctx, tx, a, Str(issue, "id"), Data{"expected_version": Num(issue, "version"),
			"repository_id": Str(repo, "id"), "goal": "Fix auth", "acceptance_criteria": "Tests pass"})
	})
	f := &developmentApprovalFixture{s: s, a: a, task: task, repo: repo,
		files: []runner.File{{Path: "auth/main.go", Mode: 0644, Data: []byte("source")}}}
	if mode == "auto" {
		for _, stage := range []string{"plan", "edit", "test", "review", "publish_pr"} {
			f.policy(t, stage, "auto")
		}
	}
	return f
}

func (f *developmentApprovalFixture) prepare(t *testing.T, stage string) (Data, Data) {
	t.Helper()
	ctx := context.Background()
	if stage != "plan" && f.baseline == "" {
		f.prepare(t, "plan")
	}
	input := Data{"base_commit": strings.Repeat("1", 40), "base_tree": strings.Repeat("2", 40), "target_base_commit": strings.Repeat("1", 40),
		"snapshot_id": f.baseline, "baseline_snapshot_id": f.baseline, "tests_passed": true,
		"test_digest": mustSnapshotDigest(f.files)}
	if err := f.s.persistPreparedStep(ctx, f.task, f.repo, stage, input, f.files); err != nil {
		t.Fatal(err)
	}
	step, err := one(ctx, f.s.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE task_id=$1 AND stage=$2", Str(f.task, "id"), stage)
	if err != nil {
		t.Fatal(err)
	}
	if stage == "plan" {
		f.baseline = Str(Object(step, "input"), "snapshot_id")
	}
	approval, err := locked(ctx, f.s.DB, "approvals", Str(step, "approval_id"))
	if err != nil {
		t.Fatal(err)
	}
	return step, approval
}

func (f *developmentApprovalFixture) policy(t *testing.T, stage, mode string) Data {
	t.Helper()
	return mutateTest(t, f.s, f.a, func(tx pgx.Tx) (Data, error) {
		return f.s.SaveConfig(context.Background(), tx, f.a, "policies", "", Data{"name": stage + "-" + mode,
			"kind": stage, "mode": mode, "enabled": true, "max_files": 50000, "max_tokens": 200000,
			"max_seconds": 3600, "allowed_paths": []string{"auth"}})
	})
}

func TestDevelopmentApprovalModesAndAudit(t *testing.T) {
	for _, mode := range []string{"manual", "auto", "deny", "unmatched"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			for _, stage := range []string{"plan", "edit", "test", "review", "publish_pr"} {
				f := newDevelopmentApprovalFixture(t, "policy", 30)
				policy := Data{}
				if mode != "unmatched" {
					policy = f.policy(t, stage, mode)
				}
				_, ap := f.prepare(t, stage)
				id := Str(ap, "id")
				status, event := "pending", "pending"
				if mode == "auto" {
					status, event = "approved", "auto_approved"
				}
				if mode == "deny" {
					status, event = "rejected", "denied"
				}
				if Str(ap, "status") != status {
					t.Fatal(stage, mode, ap)
				}
				if supportCount(t, f.s, "SELECT count(*) FROM jobs WHERE kind='notify' AND payload->>'approval_id'=$1 AND payload->>'approval_event'=$2", id, event) != 1 {
					t.Fatal("missing notice", stage, event)
				}
				if mode != "auto" {
					continue
				}
				if Str(ap, "policy_id") != Str(policy, "id") || Num(ap, "policy_version") != 1 || Str(ap, "actor_id") != "system:policy:"+Str(policy, "id") {
					t.Fatal("lost policy identity", ap)
				}
				if err := f.s.validateApproval(ctx, f.s.DB, ap, true); err != nil {
					t.Fatal(stage, err)
				}
				if supportCount(t, f.s, "SELECT count(*) FROM approval_decisions WHERE approval_id=$1 AND actor_id=$2 AND evidence_hash=$3", id, Str(ap, "actor_id"), Str(ap, "action_hash")) != 1 || supportCount(t, f.s, "SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action='auto_approve'", id) != 1 {
					t.Fatal("missing decision or audit")
				}
			}
		})
	}
}

func TestDevelopmentApprovalAutoStillHonorsDenyAndRevocation(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "auto", 30)
	ctx := context.Background()
	step, approval := f.prepare(t, "plan")
	for name, mutate := range map[string]func(Data){
		"actor version": func(ap Data) { ap["actor_id"] = "system:policy:" + ID() },
		"policy spoof":  func(ap Data) { ap["policy_id"] = ID() },
		"wrong kind":    func(ap Data) { ap["kind"] = "reply_send" },
		"no decision":   func(ap Data) { ap["id"] = ID() },
		"changed hash": func(ap Data) {
			Object(ap, "action")["max_tokens"] = 1
			ap["action_hash"] = Hash(JSON(ap["action"]))
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := decodeData(approval)
			mutate(copy)
			if err := f.s.validateApproval(ctx, f.s.DB, copy, true); err == nil {
				t.Fatal("forged automatic approval accepted")
			}
		})
	}
	// Bootstrap grants admin through both the primary role and additional roles.
	if _, err := f.s.DB.Exec(ctx, "UPDATE principals SET role='developer',roles=ARRAY['developer'],repository_ids='{}' WHERE id=$1", f.a.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.validateApproval(ctx, f.s.DB, approval, true); err == nil {
		t.Fatal("auto approval bypassed confirmer repository permission")
	}
	if _, err := f.s.DB.Exec(ctx, "UPDATE principals SET role='admin',roles=ARRAY['admin'],active=false WHERE id=$1", f.a.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.validateApproval(ctx, f.s.DB, approval, true); err == nil {
		t.Fatal("auto approval bypassed disabled confirmer")
	}
	if _, err := f.s.DB.Exec(ctx, "UPDATE principals SET active=true WHERE id=$1", f.a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec(ctx, "UPDATE task_steps SET input=input||'{\"goal\":\"changed\"}'::jsonb WHERE id=$1", Str(step, "id")); err != nil {
		t.Fatal(err)
	}
	if err := f.s.validateApproval(ctx, f.s.DB, approval, true); err == nil {
		t.Fatal("auto approval bypassed changed evidence")
	}
	if _, err := f.s.DB.Exec(ctx, "UPDATE task_steps SET input=$2 WHERE id=$1", Str(step, "id"), JSON(step["input"])); err != nil {
		t.Fatal(err)
	}
	deny := f.policy(t, "plan", "deny")
	if err := f.s.validateApproval(ctx, f.s.DB, approval, true); err == nil {
		t.Fatal("new deny rule did not stop automatic policy")
	}
	f.policy(t, "edit", "deny")
	_, rejected := f.prepare(t, "edit")
	if Str(rejected, "status") != "rejected" {
		t.Fatal("automatic policy ignored deny during creation")
	}
	mutateTest(t, f.s, f.a, func(tx pgx.Tx) (Data, error) {
		return f.s.SaveConfig(ctx, tx, f.a, "policies", Str(deny, "id"), Data{"expected_version": 1, "enabled": false})
	})
	// Restore the original task stage after testing another stage's explicit deny.
	if _, err := f.s.DB.Exec(ctx, "UPDATE dev_tasks SET status='pending_approval',stage='plan' WHERE id=$1", Str(f.task, "id")); err != nil {
		t.Fatal(err)
	}
	if err := f.s.validateApproval(ctx, f.s.DB, approval, true); err != nil {
		t.Fatal(err)
	}
	mutateTest(t, f.s, f.a, func(tx pgx.Tx) (Data, error) {
		return f.s.Decide(ctx, tx, f.a, Str(approval, "id"), Data{"expected_version": Num(approval, "version"), "decision": "revoke"})
	})
	current, err := locked(ctx, f.s.DB, "approvals", Str(approval, "id"))
	if err != nil || Str(current, "status") != "revoked" || f.s.validateApproval(ctx, f.s.DB, current, true) == nil {
		t.Fatal("automatic approval could not be revoked", err)
	}
}

func TestDevelopmentUnlimitedPolicyAndNonDevelopmentIsolation(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "policy", 0)
	ctx := context.Background()
	f.policy(t, "plan", "auto")
	_, approval := f.prepare(t, "plan")
	if Str(approval, "status") != "pending" || Num(Object(approval, "action"), "timeout_seconds") != 0 {
		t.Fatal("finite max_seconds approved an unlimited development run")
	}
	for _, kind := range []string{"reply_send", "knowledge_publish"} {
		mode, _, err := f.s.evaluateApprovalMode(ctx, f.s.DB, kind, Data{})
		if err != nil || mode != "manual" {
			t.Fatal("non-development mode changed without a policy", kind, mode, err)
		}
	}
}

func (f *developmentApprovalFixture) queuedPublication(t *testing.T) (Data, Data) {
	t.Helper()
	ctx := context.Background()
	step, approval := f.prepare(t, "plan")
	// The earlier stages are complete; this fixture starts at publication.
	if _, err := f.s.DB.Exec(ctx, "UPDATE approvals SET status='executed' WHERE id=$1", Str(approval, "id")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec(ctx, "UPDATE task_steps SET status='succeeded' WHERE id=$1", Str(step, "id")); err != nil {
		t.Fatal(err)
	}
	_, approval = f.prepare(t, "publish_pr")
	if err := f.s.executeApproval(ctx, Str(approval, "id")); err != nil {
		t.Fatal(err)
	}
	op, err := one(ctx, f.s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE approval_id=$1", Str(approval, "id"))
	if err != nil {
		t.Fatal(err)
	}
	return approval, op
}

func TestDevelopmentPublishSettingsBeforeAndAfterClaim(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			f := newDevelopmentApprovalFixture(t, "auto", 30)
			ctx := context.Background()
			approval, op := f.queuedPublication(t)
			if started {
				if _, err := f.s.DB.Exec(ctx, "UPDATE external_operations SET status='dispatching' WHERE id=$1", Str(op, "id")); err != nil {
					t.Fatal(err)
				}
			}
			mutateTest(t, f.s, f.a, func(tx pgx.Tx) (Data, error) {
				b := executionSettingsBody(1)
				b["timeout_seconds"] = 60
				return f.s.SaveExecutionSettings(ctx, tx, f.a, b)
			})
			current, _ := locked(ctx, f.s.DB, "approvals", Str(approval, "id"))
			op, _ = locked(ctx, f.s.DB, "external_operations", Str(op, "id"))
			if !started {
				if Str(current, "status") != "stale" || Str(op, "status") != "cancelled" || f.s.validateExternal(ctx, f.s.DB, op) == nil {
					t.Fatal("unstarted publication retained stale configuration approval")
				}
				return
			}
			if Str(current, "status") != "approved" || Str(op, "status") != "dispatching" {
				t.Fatal("already claimed publication lost its frozen configuration")
			}
			if err := f.s.validateExternal(ctx, f.s.DB, op); err != nil {
				t.Fatal("frozen auto approval became a nonexistent human actor", err)
			}
			f.policy(t, "publish_pr", "deny")
			if err := f.s.validateExternal(ctx, f.s.DB, op); err == nil {
				t.Fatal("publication checkpoint ignored newly explicit deny")
			}
		})
	}
}

func TestDevelopmentPublishClaimSerializesWithSettingsSave(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "auto", 30)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, op := f.queuedPublication(t)
	config := f.s.DB.Config()
	app := "publish_settings_" + ID()
	config.ConnConfig.RuntimeParams["application_name"] = app
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	publisher := *f.s
	publisher.DB = pool
	saveTx, err := f.s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer saveTx.Rollback(context.Background())
	if _, err = saveTx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690107)"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- publisher.dispatchExternal(ctx, Str(op, "id")) }()
	for {
		var waiting bool
		if err = f.s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND wait_event='advisory')", app).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err = <-done:
			t.Fatalf("publication did not wait for settings save: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	b := executionSettingsBody(1)
	b["timeout_seconds"] = 60
	if _, err = f.s.SaveExecutionSettings(ctx, saveTx, f.a, b); err != nil {
		t.Fatal(err)
	}
	if err = saveTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	op, err = locked(ctx, f.s.DB, "external_operations", Str(op, "id"))
	if err != nil || Str(op, "status") != "cancelled" {
		t.Fatal("publication crossed a concurrent configuration change", err, op)
	}
}
