package service

import (
	"context"
	"testing"

	"agentowl/backend/migrations"
)

func TestRetiredStepApprovalOverrideCannotAffectPolicies(t *testing.T) {
	ctx := context.Background()
	for _, legacy := range []string{"manual", "auto"} {
		t.Run(legacy, func(t *testing.T) {
			f := newDevelopmentApprovalFixture(t, "policy", 30)
			if _, err := f.s.DB.Exec(ctx, `UPDATE system_settings SET data=data||jsonb_build_object('approval_mode',$1::text) WHERE name='execution'`, legacy); err != nil {
				t.Fatal(err)
			}
			_, ap := f.prepare(t, "plan")
			if Str(ap, "status") != "pending" {
				t.Fatal("retired setting granted approval", ap)
			}
			policy := f.policy(t, "test", "auto")
			_, ap = f.prepare(t, "test")
			if Str(ap, "status") != "approved" || Str(ap, "policy_id") != Str(policy, "id") {
				t.Fatal("retired setting overrode policy", ap)
			}
			if err := f.s.validateApproval(ctx, f.s.DB, ap, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStepApprovalMigrationPreservesManualAndInvalidatesUnstarted(t *testing.T) {
	ctx := context.Background()
	for _, legacy := range []string{"policy", "manual", "auto"} {
		t.Run(legacy, func(t *testing.T) {
			f := newDevelopmentApprovalFixture(t, "policy", 30)
			policy := f.policy(t, "plan", "auto")
			_, ap := f.prepare(t, "plan")
			if _, err := f.s.DB.Exec(ctx, `UPDATE system_settings SET data=data||jsonb_build_object('approval_mode',$1::text) WHERE name='execution'`, legacy); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.DB.Exec(ctx, `UPDATE approvals SET action=action||jsonb_build_object('approval_mode',$2::text) WHERE id=$1`, Str(ap, "id"), legacy); err != nil {
				t.Fatal(err)
			}
			sql, err := migrations.Files.ReadFile("000014_step_approval_policies.up.sql")
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err = f.s.DB.Exec(ctx, string(sql)); err != nil {
					t.Fatal(err)
				}
			}
			current, _, err := f.s.config(ctx, f.s.DB, Str(policy, "id"), "policies")
			if err != nil {
				t.Fatal(err)
			}
			if (Str(current, "mode") == "manual") != (legacy == "manual") || policyVersion(current) != Num(current, "version") || !notificationEnabled(current) {
				t.Fatal("policy authority or notice changed unexpectedly", current)
			}
			if legacy == "manual" && Num(current, "version") != 2 {
				t.Fatal("migration was not idempotent", current)
			}
			state, err := one(ctx, f.s.DB, "SELECT to_jsonb(a) FROM approvals a WHERE id=$1", Str(ap, "id"))
			if err != nil {
				t.Fatal(err)
			}
			want := "stale"
			if legacy == "policy" {
				want = "approved"
			}
			if Str(state, "status") != want {
				t.Fatal(state)
			}
			expected := 1
			if legacy == "policy" {
				expected = 0
			}
			if supportCount(t, f.s, "SELECT count(*) FROM task_events WHERE task_id=$1 AND actor='system:migration'", Str(f.task, "id")) != expected {
				t.Fatal("missing or duplicated transition")
			}
			if supportCount(t, f.s, "SELECT count(*) FROM system_settings WHERE name='execution' AND data ? 'approval_mode'") != 0 {
				t.Fatal("old override retained")
			}
		})
	}
}

func TestStepApprovalMigrationPreservesStartedLegacyPublication(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "auto", 30)
	ctx := context.Background()
	ap, op := f.queuedPublication(t)
	action := Object(ap, "action")
	action["approval_mode"] = "auto"
	digest := Hash(JSON(action))
	actor := "system:execution-settings:1"
	if _, err := f.s.DB.Exec(ctx, `UPDATE approvals SET actor_id=$2,policy_id='',policy_version=0,action=$3,action_hash=$4 WHERE id=$1`, Str(ap, "id"), actor, JSON(action), digest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec(ctx, `UPDATE approval_decisions SET actor_id=$2,policy_id='',policy_version=0,evidence_hash=$3 WHERE approval_id=$1`, Str(ap, "id"), actor, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.Exec(ctx, `UPDATE external_operations SET status='dispatching' WHERE id=$1`, Str(op, "id")); err != nil {
		t.Fatal(err)
	}
	sql, err := migrations.Files.ReadFile("000014_step_approval_policies.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	op, err = one(ctx, f.s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE id=$1", Str(op, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if Str(op, "status") != "dispatching" {
		t.Fatal("started publication cancelled", op)
	}
	if err = f.s.validateExternal(ctx, f.s.DB, op); err != nil {
		t.Fatal("lost frozen legacy authority", err)
	}
}
