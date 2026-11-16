package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentowl/backend/internal/integration"
)

func TestNotificationDraftReplySend(t *testing.T) {
	for _, scenario := range []string{"send", "private", "answer_draft", "claim_then_send", "send_failed", "send_unknown", "execute_failed", "dispatch_failed", "historical", "claim_changed_reply", "claim_changed_case", "changed_reply", "changed_case", "closed", "unbound", "denied", "queued_reply_changed", "queued_case_changed", "viewer", "wrong_nonce"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFeedbackFixture(t, "feishu")
			s, ctx := f.s, context.Background()
			if scenario == "private" {
				env := Object(f.c, "environment")
				env["p2p_chat"], env["private_connector_id"], env["external_channel_id"], env["private_actor_id"] = true, f.connector, "oc_private", "ou_user"
				f.channel = privateChannelID(f.connector, "ou_user", "oc_private")
				if _, err := s.DB.Exec(ctx, "UPDATE cases SET channel_id='',environment=$2 WHERE id=$1", Str(f.c, "id"), JSON(env)); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB.Exec(ctx, "UPDATE source_messages SET channel_id=$2 WHERE id=$1", f.source, f.channel); err != nil {
					t.Fatal(err)
				}
				var err error
				f.c, err = one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(f.c, "id"))
				if err != nil {
					t.Fatal(err)
				}
			}
			knowledge := supportKnowledge(t, s, "public", "Public JWT troubleshooting")
			notify := supportConfig(t, s, "channels", Data{"connector_id": f.connector, "external_id": "oc_oncall", "enabled": true, "visibility": "internal"}, nil)
			supportConfig(t, s, "owner-routes", Data{"module": "auth", "primary_id": f.actor.ID, "channel_id": notify, "enabled": true}, nil)
			supportConfig(t, s, "identity-bindings", Data{"provider": "feishu", "app_id": "cli_test", "subject_id": "ou_admin", "principal_id": f.actor.ID}, nil)
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "answer_draft" || scenario == "historical" {
					supportModelResponse(w, Data{"disposition": "answer", "text": "Please provide the JWT error code.", "citation_ids": []string{knowledge}, "missing_info": []string{}})
					return
				}
				supportModelResponse(w, Data{"disposition": "clarify", "text": "Please provide the JWT error code.", "citation_ids": []string{}, "missing_info": []string{"JWT error code"}})
			}))
			defer model.Close()
			saveModelSettingTest(t, s, f.actor, Data{"expected_version": 0, "enabled": true, "base_url": model.URL, "allow_http": true, "model_name": "test"})
			supportKnowledge(t, s, "public", "Ask for the JWT error code before diagnosing authentication failures.")
			rid := supportReply(t, s, f.c)
			if err := s.ProcessSupportJob(ctx, "answer", Data{"reply_id": rid, "automatic": true, "historical": scenario == "historical"}); err != nil {
				t.Fatal(err)
			}
			if scenario == "historical" {
				if supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='notify'") != 0 {
					t.Fatal("historical draft paged oncall")
				}
				return
			}
			job, err := one(ctx, s.DB, "SELECT to_jsonb(j) FROM jobs j WHERE kind='notify' AND payload->>'case_id'=$1", Str(f.c, "id"))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "queued_reply_changed" || scenario == "queued_case_changed" {
				query, id := "UPDATE replies SET version=version+1 WHERE id=$1", rid
				if scenario == "queued_case_changed" {
					query, id = "UPDATE cases SET version=version+1 WHERE id=$1", Str(f.c, "id")
				}
				if _, err = s.DB.Exec(ctx, query, id); err != nil {
					t.Fatal(err)
				}
				if err = s.ProcessSupportJob(ctx, "notify", Object(job, "payload")); err != nil {
					t.Fatal(err)
				}
				if supportCount(t, s, "SELECT count(*) FROM external_operations WHERE kind='notify'") != 0 {
					t.Fatal("stale draft notification queued")
				}
				return
			}
			if err = s.ProcessSupportJob(ctx, "notify", Object(job, "payload")); err != nil {
				t.Fatal(err)
			}
			op, err := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE kind='notify' AND subject_id=$1", Str(f.c, "id"))
			if err != nil {
				t.Fatal(err)
			}
			req := Object(op, "request")
			if Str(Object(Object(req, "card_actions"), "send_reply"), "reply_id") != rid {
				t.Fatal("draft notification has no send action")
			}
			card := string(notificationCard(Str(op, "id"), req))
			if !strings.Contains(card, "批准并发送") || !strings.Contains(card, "查看草稿") {
				t.Fatal("missing draft buttons")
			}
			if _, err = s.DB.Exec(ctx, "UPDATE external_operations SET status='succeeded',remote_id='om_notice' WHERE id=$1", Str(op, "id")); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "claim_") {
				h, b := supportCardActionSigned(t, Data{"open_id": "ou_admin", "tenant_key": "tenant_test", "open_message_id": "om_notice", "open_chat_id": "oc_oncall", "token": "verify-test", "action": Data{"name": "claim", "value": Data{"operation_id": Str(op, "id"), "nonce": Str(req, "card_nonce"), "action": "claim"}}})
				for range 2 {
					claimed, err := s.IngestCardAction(ctx, "feishu", f.connector, h, b)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(JSON(claimed)), "查看草稿") || !strings.Contains(string(JSON(claimed)), "批准并发送") {
						t.Fatal("claim removed draft actions")
					}
				}
			}
			otherNotify := supportConfig(t, s, "channels", Data{"connector_id": f.connector, "external_id": "oc_other", "enabled": true, "visibility": "internal"}, nil)
			supportConfig(t, s, "policies", Data{"kind": "reply_send", "mode": "manual", "enabled": true, "notification_channel_id": otherNotify}, nil)
			actor, nonce := "ou_admin", Str(req, "card_nonce")
			switch scenario {
			case "changed_reply", "claim_changed_reply":
				_, err = s.DB.Exec(ctx, "UPDATE replies SET version=version+1 WHERE id=$1", rid)
			case "changed_case", "claim_changed_case":
				_, err = s.DB.Exec(ctx, "UPDATE cases SET version=version+1 WHERE id=$1", Str(f.c, "id"))
			case "closed":
				_, err = s.DB.Exec(ctx, "UPDATE cases SET status='closed' WHERE id=$1", Str(f.c, "id"))
			case "viewer":
				_, err = s.DB.Exec(ctx, "UPDATE principals SET role='viewer',roles=ARRAY['viewer'] WHERE id=$1", f.actor.ID)
			case "wrong_nonce":
				nonce = "wrong"
			case "unbound":
				actor = "ou_stranger"
			case "denied":
				supportConfig(t, s, "policies", Data{"kind": "reply_send", "mode": "deny", "enabled": true}, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			h, b := supportCardActionSigned(t, Data{"open_id": actor, "tenant_key": "tenant_test", "open_message_id": "om_notice", "open_chat_id": "oc_oncall", "token": "verify-test", "action": Data{"name": "send_reply", "value": Data{"operation_id": Str(op, "id"), "nonce": nonce, "action": "send_reply"}}})
			result, err := s.IngestCardAction(ctx, "feishu", f.connector, h, b)
			if !contains([]string{"send", "private", "answer_draft", "claim_then_send", "send_failed", "send_unknown", "execute_failed", "dispatch_failed"}, scenario) {
				if err == nil {
					t.Fatal("unsafe send accepted", scenario)
				}
				if supportCount(t, s, "SELECT count(*) FROM approvals WHERE subject_id=$1", rid) != 0 {
					t.Fatal("failed callback left approval")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(JSON(result)), "加入发送队列") {
				t.Fatal("send result unclear", result)
			}
			if _, err = s.IngestCardAction(ctx, "feishu", f.connector, h, b); err != nil {
				t.Fatal(err)
			}
			if scenario == "claim_then_send" {
				h, b := supportCardActionSigned(t, Data{"open_id": "ou_admin", "tenant_key": "tenant_test", "open_message_id": "om_notice", "open_chat_id": "oc_oncall", "token": "verify-test", "action": Data{"name": "claim", "value": Data{"operation_id": Str(op, "id"), "nonce": Str(req, "card_nonce"), "action": "claim"}}})
				claimed, err := s.IngestCardAction(ctx, "feishu", f.connector, h, b)
				if err != nil || strings.Contains(string(JSON(claimed)), "批准并发送") {
					t.Fatal("duplicate claim resurrected send button", err)
				}
			}
			if supportCount(t, s, "SELECT count(*) FROM approvals WHERE subject_id=$1", rid) != 1 {
				t.Fatal("duplicate approval")
			}
			ap, err := one(ctx, s.DB, "SELECT to_jsonb(a) FROM approvals a WHERE subject_id=$1", rid)
			if err != nil || Str(ap, "status") != "approved" || Str(Object(ap, "action"), "channel_id") != f.channel {
				t.Fatal("wrong approval target", ap, err)
			}
			failReplyJob := func(kind, key, id string) {
				t.Helper()
				job, err := one(ctx, s.DB, "UPDATE jobs SET status='running',lease_token=$3,lease_until=now()+interval '45 seconds' WHERE kind=$1 AND payload->>$2=$4 RETURNING to_jsonb(jobs)", kind, key, ID(), id)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.failJob(ctx, job, Conflict); err != nil {
					t.Fatal(err)
				}
			}
			event := "reply_succeeded"
			if scenario == "execute_failed" {
				failReplyJob("execute_approval", "approval_id", Str(ap, "id"))
				event = "reply_failed"
			} else {
				if err = s.executeApproval(ctx, Str(ap, "id")); err != nil {
					t.Fatal(err)
				}
				send, err := one(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE approval_id=$1", Str(ap, "id"))
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "dispatch_failed" {
					failReplyJob("dispatch_external", "operation_id", Str(send, "id"))
					event = "reply_failed"
				} else if scenario == "send_failed" || scenario == "send_unknown" {
					send, err = one(ctx, s.DB, "UPDATE external_operations SET status='dispatching' WHERE id=$1 RETURNING to_jsonb(external_operations)", Str(send, "id"))
					if err != nil {
						t.Fatal(err)
					}
					cause := &integration.APIError{Provider: "lark", StatusCode: 403, Code: "forbidden"}
					event = "reply_failed"
					if scenario == "send_unknown" {
						cause.StatusCode = 503
						cause.OutcomeUnknown = true
						event = "reply_outcome_unknown"
					}
					if err = s.externalFailure(ctx, send, cause); err != nil {
						t.Fatal(err)
					}
				} else if err = s.dispatchExternal(ctx, Str(send, "id")); err != nil {
					t.Fatal(err)
				}
			}
			outcome := notificationJob(t, s, ap, event)
			for range 2 {
				if err = s.ProcessSupportJob(ctx, "notify", outcome); err != nil {
					t.Fatal(err)
				}
			}
			notices, err := many(ctx, s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE request->>'approval_id'=$1 AND request->>'approval_event'=$2", Str(ap, "id"), event)
			if err != nil || len(notices) != 1 {
				t.Fatal("missing or duplicate send result", notices, err)
			}
			if Str(Object(notices[0], "target"), "channel_id") != notify {
				t.Fatal("result sent to wrong group")
			}
			if allowed, err := s.approvalNotificationAllowed(ctx, s.DB, notices[0]); err != nil || !allowed {
				t.Fatal("result blocked for original notification group", err)
			}
			resultReq := Object(notices[0], "request")
			if len(Object(resultReq, "card_actions")) != 0 || !strings.Contains(string(notificationCard(Str(notices[0], "id"), resultReq)), "查看详情") {
				t.Fatal("result must link to details without a send action")
			}
			if contains([]string{"send_failed", "send_unknown", "execute_failed", "dispatch_failed"}, scenario) {
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.sent) != 1 || !strings.Contains(Str(f.sent[0], "content"), "Please provide the JWT error code.") {
				t.Fatal("reply was not sent once", f.sent)
			}
		})
	}
}
