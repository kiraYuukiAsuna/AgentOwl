package service

import (
	"agentowl/backend/migrations"
	"context"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func creationApprovalFixture(t *testing.T, mode string) (*Service, Actor, Data, Data) {
	t.Helper()
	s, a, _, repo := runtimeTaskFixture(t)
	ctx := context.Background()
	co, ch := supportLarkFixture(t, s)
	h, b := supportSigned(t, ID(), "im.message.receive_v1", supportMessage("creation", "Fix login", time.Now()))
	in, err := s.IngestWebhook(ctx, "feishu", co, h, b)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.consumeSupportEvent(ctx, Str(in, "id")); err != nil {
		t.Fatal(err)
	}
	c, err := one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE channel_id=$1", ch)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := one(ctx, s.DB, "SELECT to_jsonb(m) FROM source_messages m WHERE case_id=$1", Str(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	rule := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", "", Data{"name": "Create fix", "kind": "create_task", "mode": mode, "channel_id": ch, "repository_id": Str(repo, "id"), "sponsor_principal_id": a.ID, "condition": "reproducible defects", "enabled": true})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supportModelResponse(w, Data{"outcome": "accept", "goal": "Fix login", "acceptance_criteria": "Login succeeds", "reason": "Reproduction supplied", "rule_id": Str(rule, "id"), "evidence_ids": []string{Str(msg, "id")}})
	}))
	t.Cleanup(server.Close)
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": server.URL, "allow_http": true, "model_name": "test"})
	if err = s.evaluateAdmission(ctx, Str(c, "id")); err != nil {
		t.Fatal(err)
	}
	ap, err := one(ctx, s.DB, "SELECT to_jsonb(a) FROM approvals a WHERE kind='create_task' AND action->>'case_id'=$1", Str(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	return s, a, rule, ap
}

func TestTaskCreationUsesSharedApprovalAndPermissions(t *testing.T) {
	ctx := context.Background()
	s, a, rule, ap := creationApprovalFixture(t, "manual")
	action := Object(ap, "action")
	if Str(ap, "status") != "pending" || supportCount(t, s, "SELECT count(*) FROM dev_tasks WHERE case_id=$1", Str(action, "case_id")) != 0 {
		t.Fatal("created before approval")
	}
	viewer := Actor{ID: ID(), Role: "viewer", ChannelIDs: []string{Str(rule, "channel_id")}, RepositoryIDs: []string{Str(rule, "repository_id")}}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		_, e := s.Decide(ctx, tx, viewer, Str(ap, "id"), Data{"decision": "approve", "expected_version": Num(ap, "version")})
		return e
	})
	if err == nil {
		t.Fatal("viewer approved")
	}
	hidden := Actor{ID: ID(), Role: "developer"}
	list, err := s.List(ctx, hidden, "approvals", url.Values{"kind": {"create_task"}})
	if err != nil {
		t.Fatal(err)
	}
	if Num(list, "total") != 0 {
		t.Fatal("cross-scope approval leaked", list)
	}
	approved := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.Decide(ctx, tx, a, Str(ap, "id"), Data{"decision": "approve", "expected_version": Num(ap, "version")})
	})
	if Str(approved, "actor_id") != a.ID {
		t.Fatal(approved)
	}
	for range 2 {
		if err = s.executeApproval(ctx, Str(ap, "id")); err != nil {
			t.Fatal(err)
		}
	}
	if supportCount(t, s, "SELECT count(*) FROM dev_tasks WHERE case_id=$1", Str(action, "case_id")) != 1 {
		t.Fatal("task not created exactly once")
	}
	if supportCount(t, s, "SELECT count(*) FROM approvals WHERE id=$1 AND status='executed'", Str(ap, "id")) != 1 {
		t.Fatal("shared approval not completed")
	}
}

func TestTaskCreationRechecksPolicySourceAndAuthority(t *testing.T) {
	for _, change := range []string{"source", "policy", "sponsor", "deny", "manual", "expiry", "revoke"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, a, rule, ap := creationApprovalFixture(t, "auto")
			action := Object(ap, "action")
			switch change {
			case "source":
				_, err := s.DB.Exec(ctx, "UPDATE source_messages SET content='changed',version=version+1 WHERE case_id=$1", Str(action, "case_id"))
				if err != nil {
					t.Fatal(err)
				}
			case "sponsor":
				_, err := s.DB.Exec(ctx, "UPDATE principals SET active=false WHERE id=$1", a.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "policy":
				mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
					return s.SaveConfig(ctx, tx, a, "policies", Str(rule, "id"), Data{"expected_version": Num(rule, "version"), "enabled": false})
				})
			case "deny", "manual":
				body := decodeData(rule)
				delete(body, "id")
				delete(body, "version")
				body["mode"] = change
				body["name"] = "stricter"
				mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveConfig(ctx, tx, a, "policies", "", body) })
			case "expiry":
				_, err := s.DB.Exec(ctx, `UPDATE configs SET data=jsonb_set(data,'{expires_at}',to_jsonb($2::text)) WHERE id=$1`, Str(rule, "id"), time.Now().Add(-time.Minute).Format(time.RFC3339))
				if err != nil {
					t.Fatal(err)
				}
			case "revoke":
				mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
					return s.Decide(ctx, tx, a, Str(ap, "id"), Data{"expected_version": Num(ap, "version"), "decision": "revoke", "reason": "stop"})
				})
			}
			_ = s.executeApproval(ctx, Str(ap, "id"))
			if supportCount(t, s, "SELECT count(*) FROM dev_tasks WHERE case_id=$1", Str(action, "case_id")) != 0 {
				t.Fatal("stale approval created a task")
			}
		})
	}
}

func TestUnifiedPolicyMigrationPreservesIdentityAndDoesNotAuthorize(t *testing.T) {
	for _, mode := range []string{"off", "observe", "auto"} {
		t.Run(mode, func(t *testing.T) {
			s, a, task, repo := runtimeTaskFixture(t)
			ctx := context.Background()
			_, ch := supportLarkFixture(t, s)
			id := ID()
			_, err := s.DB.Exec(ctx, `UPDATE system_settings SET data=jsonb_build_object('mode',$1::text) WHERE name='admission'`, mode)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.DB.Exec(ctx, `INSERT INTO configs(id,kind,name,version,data) VALUES($1,'admission-rules','legacy',7,$2)`, id, JSON(Data{"enabled": true, "channel_id": ch, "repository_id": Str(repo, "id"), "sponsor_principal_id": a.ID, "condition": "defects", "allowed_paths": []string{"src"}}))
			if err != nil {
				t.Fatal(err)
			}
			sql, err := migrations.Files.ReadFile("000012_unified_approval_policies.up.sql")
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err = s.DB.Exec(ctx, string(sql)); err != nil {
					t.Fatal(err)
				}
			}
			policy, err := s.Get(ctx, s.DB, a, "policies", id)
			if err != nil {
				t.Fatal(err)
			}
			if Num(policy, "version") != 7 || Str(policy, "kind") != "create_task" || (Str(policy, "mode") == "auto") != (mode == "auto") || Bool(policy, "enabled") != (mode != "off") {
				t.Fatal(policy)
			}
			if supportCount(t, s, "SELECT count(*) FROM approvals WHERE kind='create_task' AND status IN ('pending','approved')") != 0 {
				t.Fatal("migration granted authority")
			}
			if supportCount(t, s, "SELECT count(*) FROM approvals WHERE action->>'task_id'=$1 AND status='executed'", Str(task, "id")) != 1 {
				t.Fatal("historical confirmation missing")
			}
		})
	}
}

func TestAutomaticPolicyCannotBypassNewManualPolicy(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "auto", 30)
	ctx := context.Background()
	_, ap := f.prepare(t, "plan")
	f.policy(t, "plan", "manual")
	if err := f.s.validateApproval(ctx, f.s.DB, ap, true); err == nil {
		t.Fatal("automatic policy bypassed manual policy")
	}
}

func TestCreationDenyPolicyNeedsOnlyScopeAndBlocksDirectConfirmation(t *testing.T) {
	s, a, rule, ap := creationApprovalFixture(t, "manual")
	ctx := context.Background()
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", "", Data{"name": "no creation", "kind": "create_task", "mode": "deny", "enabled": true, "channel_id": Str(rule, "channel_id"), "repository_id": Str(rule, "repository_id")})
	})
	action := Object(ap, "action")
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		_, err := s.ConfirmDevelopment(ctx, tx, a, Str(action, "case_id"), Data{"expected_version": Num(action, "case_version"), "repository_id": Str(rule, "repository_id"), "goal": "fix", "acceptance_criteria": "works"})
		return err
	})
	if err == nil {
		t.Fatal("manual confirmation bypassed deny")
	}
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		_, err := s.Decide(ctx, tx, a, Str(ap, "id"), Data{"decision": "approve", "expected_version": Num(ap, "version")})
		return err
	})
	if err == nil {
		t.Fatal("approval bypassed deny")
	}
}

func TestDevelopmentApprovalUsesMatchedPolicy(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "policy", 30)
	policy := f.policy(t, "plan", "auto")
	_, ap := f.prepare(t, "plan")
	if Str(ap, "status") != "approved" || Str(ap, "policy_id") != Str(policy, "id") {
		t.Fatal(ap)
	}
	if err := f.s.validateApproval(context.Background(), f.s.DB, ap, true); err != nil {
		t.Fatal(err)
	}
}
