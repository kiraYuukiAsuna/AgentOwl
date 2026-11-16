package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentowl/backend/migrations"
	"github.com/jackc/pgx/v5"
)

func notificationKnowledge(t *testing.T, s *Service, a Actor) Data {
	t.Helper()
	ctx := context.Background()
	k := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveKnowledge(ctx, tx, a, "", Data{"title": "通知测试知识", "body": "经过审核的内容"})
	})
	return mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.KnowledgeAction(ctx, tx, a, Str(k, "id"), "submit", Data{"expected_version": Num(k, "version")})
	})
}
func notificationPolicy(t *testing.T, s *Service, a Actor, mode, channel string, enabled bool) Data {
	t.Helper()
	return mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(context.Background(), tx, a, "policies", "", Data{"name": "知识审批", "kind": "knowledge_publish", "mode": mode, "enabled": true, "max_files": 100, "max_tokens": 10000, "max_seconds": 300, "notification_enabled": enabled, "notification_channel_id": channel})
	})
}
func notificationJob(t *testing.T, s *Service, ap Data, event string) Data {
	t.Helper()
	job, err := one(context.Background(), s.DB, "SELECT to_jsonb(j) FROM jobs j WHERE kind='notify' AND payload->>'approval_id'=$1 AND payload->>'approval_event'=$2", Str(ap, "id"), event)
	if err != nil {
		t.Fatal("missing notification", event, err)
	}
	return Object(job, "payload")
}
func notificationOperation(t *testing.T, s *Service, ap Data, event string) Data {
	t.Helper()
	p := notificationJob(t, s, ap, event)
	for range 2 {
		if err := s.ProcessSupportJob(context.Background(), "notify", p); err != nil {
			t.Fatal(err)
		}
	}
	if supportCount(t, s, "SELECT count(*) FROM external_operations WHERE request->>'approval_id'=$1 AND request->>'approval_event'=$2", Str(ap, "id"), event) != 1 {
		t.Fatal("notification not deduplicated")
	}
	op, err := one(context.Background(), s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE request->>'approval_id'=$1 AND request->>'approval_event'=$2", Str(ap, "id"), event)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestApprovalNotificationsModesAndKnowledgeWithoutCase(t *testing.T) {
	for mode, event := range map[string]string{"manual": "pending", "auto": "auto_approved", "deny": "denied"} {
		for _, enabled := range []bool{true, false} {
			t.Run(mode+"/"+map[bool]string{true: "on", false: "off"}[enabled], func(t *testing.T) {
				s, a := testService(t)
				ctx := context.Background()
				_, ch := supportLarkFixture(t, s)
				policy := notificationPolicy(t, s, a, mode, ch, enabled)
				ap := notificationKnowledge(t, s, a)
				if !enabled {
					if supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='notify' AND payload->>'approval_id'=$1", Str(ap, "id")) != 0 {
						t.Fatal("disabled policy enqueued notice")
					}
					return
				}
				if mode == "auto" {
					if err := s.executeApproval(ctx, Str(ap, "id")); err != nil {
						t.Fatal(err)
					}
				}
				op := notificationOperation(t, s, ap, event)
				r := Object(op, "request")
				if Str(r, "case_id") != "" || Str(op, "approval_id") != "" || Str(Object(op, "target"), "channel_id") != ch || Str(r, "notification_policy_id") != Str(policy, "id") {
					t.Fatal(op)
				}
				if !strings.Contains(Str(r, "body"), approvalNotificationLabels[event]) || !strings.Contains(Str(r, "body"), "/approvals/"+Str(ap, "id")) {
					t.Fatal(r)
				}
				if (len(Object(r, "card_actions")) == 2) != (mode == "manual") {
					t.Fatal("wrong notification buttons", r)
				}
				allowed, err := s.approvalNotificationAllowed(ctx, s.DB, op)
				if err != nil || !allowed {
					t.Fatal("valid notification blocked", err)
				}
			})
		}
	}
}

func TestApprovalNotificationsHumanDecisionsAndStaleCards(t *testing.T) {
	for _, decision := range []string{"approve", "reject", "revoke"} {
		t.Run(decision, func(t *testing.T) {
			s, a := testService(t)
			ctx := context.Background()
			_, ch := supportLarkFixture(t, s)
			policy := notificationPolicy(t, s, a, "manual", ch, true)
			ap := notificationKnowledge(t, s, a)
			pending := notificationOperation(t, s, ap, "pending")
			decide := func(d string) {
				ap = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
					return s.Decide(ctx, tx, a, Str(ap, "id"), Data{"expected_version": Num(ap, "version"), "decision": d, "reason": "人工确认"})
				})
			}
			if decision == "revoke" {
				decide("approve")
				notificationOperation(t, s, ap, "approved")
			}
			decide(decision)
			event := map[string]string{"approve": "approved", "reject": "rejected", "revoke": "revoked"}[decision]
			op := notificationOperation(t, s, ap, event)
			if len(Object(Object(op, "request"), "card_actions")) != 0 || !strings.Contains(Str(Object(op, "request"), "body"), "人工确认") {
				t.Fatal(op)
			}
			if err := s.dispatchExternal(ctx, Str(pending, "id")); err != nil {
				t.Fatal(err)
			}
			if supportCount(t, s, "SELECT count(*) FROM external_operations WHERE id=$1 AND status='cancelled'", Str(pending, "id")) != 1 {
				t.Fatal("stale approval card not cancelled")
			}
			mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.SaveConfig(ctx, tx, a, "policies", Str(policy, "id"), Data{"expected_version": Num(policy, "version"), "notification_enabled": false})
			})
			if err := s.dispatchExternal(ctx, Str(op, "id")); err != nil {
				t.Fatal(err)
			}
			if supportCount(t, s, "SELECT count(*) FROM external_operations WHERE id=$1 AND status='cancelled'", Str(op, "id")) != 1 {
				t.Fatal("unsent result not cancelled")
			}
		})
	}
}

func TestApprovalNotificationsRoutingAndValidation(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	co, ch := supportLarkFixture(t, s)
	ap := notificationKnowledge(t, s, a)
	p := notificationJob(t, s, ap, "pending")
	err := s.ProcessSupportJob(ctx, "notify", p)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "notification_unrouted" {
		t.Fatal("missing route must fail visibly", err)
	}
	if supportCount(t, s, "SELECT count(*) FROM approvals WHERE id=$1 AND status='pending'", Str(ap, "id")) != 1 {
		t.Fatal("route failure changed approval")
	}
	supportConfig(t, s, "owner-routes", Data{"module": "*", "enabled": true, "primary_id": a.ID, "channel_id": ch}, nil)
	op := notificationOperation(t, s, ap, "pending")
	if Str(Object(op, "target"), "channel_id") != ch {
		t.Fatal(op)
	}
	external := supportConfig(t, s, "channels", Data{"connector_id": co, "external_id": "external", "enabled": true, "visibility": "public"}, nil)
	for _, bad := range []Data{{"notification_enabled": "false"}, {"notification_channel_id": external}, {"notification_channel_id": []string{ch}}} {
		body := Data{"name": "bad notification", "kind": "knowledge_publish", "mode": "manual", "enabled": true}
		for k, v := range bad {
			body[k] = v
		}
		if err := s.transaction(ctx, func(tx pgx.Tx) error { _, e := s.SaveConfig(ctx, tx, a, "policies", "", body); return e }); err == nil {
			t.Fatal("invalid notification accepted", bad)
		}
	}
	policy := notificationPolicy(t, s, a, "manual", ch, true)
	ap = notificationKnowledge(t, s, a)
	p = notificationJob(t, s, ap, "pending")
	policy = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "policies", Str(policy, "id"), Data{"expected_version": Num(policy, "version"), "notification_enabled": false})
	})
	if err := s.ProcessSupportJob(ctx, "notify", p); err != nil {
		t.Fatal(err)
	}
	if supportCount(t, s, "SELECT count(*) FROM external_operations WHERE request->>'approval_id'=$1", Str(ap, "id")) != 0 {
		t.Fatal("disabled job created notice")
	}
	if err := s.transaction(ctx, func(tx pgx.Tx) error {
		_, e := s.DeleteConfig(ctx, tx, a, "channels", ch, Data{"expected_version": 1})
		return e
	}); err == nil {
		t.Fatal("referenced channel deleted")
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.DeleteConfig(ctx, tx, a, "channels", ch, Data{"expected_version": 1, "force": true})
	})
	current, _, err := s.config(ctx, s.DB, Str(policy, "id"), "policies")
	if err != nil || notificationEnabled(current) || policyVersion(current) != policyVersion(policy) {
		t.Fatal("notification detach changed authority", current, err)
	}
}

func TestNotificationEditsPreserveApprovalAndTaskAuthority(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"manual", "auto"} {
		t.Run(mode, func(t *testing.T) {
			s, a, policy, ap := creationApprovalFixture(t, mode)
			updated := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.SaveConfig(ctx, tx, a, "policies", Str(policy, "id"), Data{"expected_version": Num(policy, "version"), "notification_enabled": false, "approval_version": 999})
			})
			if Num(updated, "version") != Num(policy, "version")+1 || policyVersion(updated) != policyVersion(policy) {
				t.Fatal(updated)
			}
			current, err := one(ctx, s.DB, "SELECT to_jsonb(a) FROM approvals a WHERE id=$1", Str(ap, "id"))
			if err != nil || Str(current, "status") != Str(ap, "status") {
				t.Fatal("notification edit invalidated approval", current, err)
			}
			if mode == "manual" {
				mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
					return s.Decide(ctx, tx, a, Str(ap, "id"), Data{"expected_version": Num(ap, "version"), "decision": "approve"})
				})
			}
			if err := s.executeApproval(ctx, Str(ap, "id")); err != nil {
				t.Fatal(err)
			}
			task, err := one(ctx, s.DB, "SELECT to_jsonb(t) FROM dev_tasks t WHERE case_id=$1", Str(Object(ap, "action"), "case_id"))
			if err != nil {
				t.Fatal(err)
			}
			updated = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.SaveConfig(ctx, tx, a, "policies", Str(policy, "id"), Data{"expected_version": Num(updated, "version"), "notification_enabled": true})
			})
			if err := s.validateTaskMandate(ctx, s.DB, task); err != nil {
				t.Fatal("notification edit revoked task", err)
			}
			changed := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
				return s.SaveConfig(ctx, tx, a, "policies", Str(policy, "id"), Data{"expected_version": Num(updated, "version"), "enabled": false})
			})
			if policyVersion(changed) != Num(changed, "version") {
				t.Fatal("authority revision did not advance", changed)
			}
			if mode == "auto" && s.validateTaskMandate(ctx, s.DB, task) == nil {
				t.Fatal("authority edit did not revoke auto task")
			}
		})
	}
}

func TestNotificationSwitchWithManualPolicyAndTargetChanges(t *testing.T) {
	f := newDevelopmentApprovalFixture(t, "manual", 30)
	ctx := context.Background()
	_, ch := supportLarkFixture(t, f.s)
	policy := f.policy(t, "plan", "manual")
	mutateTest(t, f.s, f.a, func(tx pgx.Tx) (Data, error) {
		return f.s.SaveConfig(ctx, tx, f.a, "policies", Str(policy, "id"), Data{"expected_version": Num(policy, "version"), "notification_enabled": false})
	})
	_, ap := f.prepare(t, "plan")
	if Str(ap, "status") != "pending" || supportCount(t, f.s, "SELECT count(*) FROM jobs WHERE kind='notify' AND payload->>'approval_id'=$1", Str(ap, "id")) != 0 {
		t.Fatal("manual policy discarded notification switch", ap)
	}
	policy = notificationPolicy(t, f.s, f.a, "manual", ch, true)
	ap = notificationKnowledge(t, f.s, f.a)
	op := notificationOperation(t, f.s, ap, "pending")
	_, other := supportLarkFixture(t, f.s)
	mutateTest(t, f.s, f.a, func(tx pgx.Tx) (Data, error) {
		return f.s.SaveConfig(ctx, tx, f.a, "policies", Str(policy, "id"), Data{"expected_version": Num(policy, "version"), "notification_channel_id": other})
	})
	allowed, err := f.s.approvalNotificationAllowed(ctx, f.s.DB, op)
	if err != nil || allowed {
		t.Fatal("old destination remained allowed", err)
	}
}

func TestApprovalNotificationMigrationPreservesAuthority(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	id := supportConfig(t, s, "policies", Data{"kind": "plan", "mode": "auto", "enabled": true}, nil)
	if _, err := s.DB.Exec(ctx, "UPDATE configs SET version=7 WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	sql, err := migrations.Files.ReadFile("000013_approval_notifications.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	policy, _, err := s.config(ctx, s.DB, id, "policies")
	if err != nil || Num(policy, "version") != 7 || policyVersion(policy) != 7 || !notificationEnabled(policy) {
		t.Fatal(policy, err)
	}
	if supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='notify'") != 0 {
		t.Fatal("migration replayed notifications")
	}
}

func TestApprovalNotificationDeliveryAndKnowledgeCardCallback(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	co, ch := supportLarkFixture(t, s)
	var sent atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/open-apis/im/v1/messages" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var body Data
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if Str(body, "receive_id") != "oc_test" || Str(body, "msg_type") != "interactive" || !strings.Contains(Str(body, "content"), "待审批") {
			t.Error("wrong outgoing card", body)
		}
		sent.Add(1)
		_ = json.NewEncoder(w).Encode(Data{"code": 0, "data": Data{"message_id": "om_approval"}})
	}))
	defer server.Close()
	if _, err := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", co, JSON(Data{"base_url": server.URL})); err != nil {
		t.Fatal(err)
	}
	notificationPolicy(t, s, a, "manual", ch, true)
	ap := notificationKnowledge(t, s, a)
	op := notificationOperation(t, s, ap, "pending")
	for range 2 {
		if err := s.dispatchExternal(ctx, Str(op, "id")); err != nil {
			t.Fatal(err)
		}
	}
	if sent.Load() != 1 || supportCount(t, s, "SELECT count(*) FROM external_operations WHERE id=$1 AND status='succeeded' AND remote_id='om_approval'", Str(op, "id")) != 1 {
		t.Fatal("notice not delivered exactly once", sent.Load())
	}
	supportConfig(t, s, "identity-bindings", Data{"provider": "feishu", "app_id": "cli_test", "subject_id": "ou_admin", "principal_id": a.ID}, nil)
	card := Data{"open_id": "ou_admin", "tenant_key": "tenant_test", "open_message_id": "om_approval", "open_chat_id": "oc_test", "token": "verify-test", "action": Data{"name": "approve", "value": Data{"operation_id": Str(op, "id"), "nonce": Str(Object(op, "request"), "card_nonce"), "action": "approve"}}}
	h, b := supportCardActionSigned(t, card)
	for range 2 {
		if _, err := s.IngestCardAction(ctx, "feishu", co, h, b); err != nil {
			t.Fatal(err)
		}
	}
	if supportCount(t, s, "SELECT count(*) FROM approvals WHERE id=$1 AND status='approved' AND actor_id=$2", Str(ap, "id"), a.ID) != 1 {
		t.Fatal("knowledge card did not approve")
	}
	if supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='notify' AND payload->>'approval_id'=$1 AND payload->>'approval_event'='approved'", Str(ap, "id")) != 1 {
		t.Fatal("callback duplicated result notice")
	}
}
