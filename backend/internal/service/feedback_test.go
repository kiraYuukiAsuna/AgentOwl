package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type feedbackFixture struct {
	s                          *Service
	actor                      Actor
	c                          Data
	connector, channel, source string
	mu                         sync.Mutex
	sent                       []Data
}

func newFeedbackFixture(t *testing.T, provider string) *feedbackFixture {
	t.Helper()
	s, actor := testService(t)
	f := &feedbackFixture{s: s, actor: actor}
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		if r.Method != "POST" {
			t.Errorf("unexpected network method %s", r.Method)
			w.WriteHeader(405)
			return
		}
		payload := Data{}
		if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.sent = append(f.sent, payload)
		n := len(f.sent)
		f.mu.Unlock()
		if provider == "github" {
			_ = json.NewEncoder(w).Encode(Data{"id": 1000 + n, "body": Str(payload, "body"), "html_url": fmt.Sprintf("https://github.com/owner/repo/issues/17#issuecomment-%d", 1000+n), "user": Data{"id": 9, "login": "agentowl", "type": "Bot"}})
			return
		}
		_ = json.NewEncoder(w).Encode(Data{"code": 0, "data": Data{"message_id": fmt.Sprintf("om_reply_%d", n), "chat_id": "oc_test"}})
	}))
	t.Cleanup(server.Close)
	config := Data{"provider": provider, "base_url": server.URL, "status": "verified"}
	secret := Data{"webhook_secret": "feedback-webhook-secret"}
	external, source, participant := "owner/repo", "issue:100", "42"
	if provider == "github" {
		config["owner"], config["repo"] = "owner", "repo"
		secret["token"] = "test-token"
	}
	if provider != "github" {
		config["app_id"], config["tenant_id"], config["bot_open_id"] = "cli_test", "tenant_test", "ou_bot"
		secret = Data{"app_secret": "secret", "verification_token": "verify-test", "encrypt_key": "encryption-test"}
		external, source, participant = "oc_test", "om_source", "ou_user"
	}
	f.connector = supportConfig(t, s, "connectors", config, secret)
	f.channel = supportConfig(t, s, "channels", Data{"connector_id": f.connector, "external_id": external, "enabled": true, "visibility": "external", "reply_mode": "draft"}, nil)
	f.c = supportCase(t, s, actor, f.channel)
	f.source = ID()
	if _, e := s.DB.Exec(ctx, "UPDATE cases SET source=$2,environment=$3 WHERE id=$1", Str(f.c, "id"), provider, JSON(Data{"last_message_id": source, "source_number": 17})); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, "INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,actor_id,content,source_revision) VALUES($1,$2,$3,$4,$5,$6,'Auth failed','revision-1')", f.source, f.connector, f.channel, source, Str(f.c, "id"), participant); e != nil {
		t.Fatal(e)
	}
	var e error
	f.c, e = locked(ctx, s.DB, "cases", Str(f.c, "id"))
	if e != nil {
		t.Fatal(e)
	}
	return f
}

func (f *feedbackFixture) send(t *testing.T) Data {
	t.Helper()
	ctx := context.Background()
	reply := mutateTest(t, f.s, f.actor, func(tx pgx.Tx) (Data, error) {
		return f.s.CreateReply(ctx, tx, f.actor, Str(f.c, "id"), Data{"body": "Try the documented login flow.", "expected_version": Num(f.c, "version")})
	})
	ap := mutateTest(t, f.s, f.actor, func(tx pgx.Tx) (Data, error) {
		return f.s.SubmitReply(ctx, tx, f.actor, Str(reply, "id"), Data{"expected_version": Num(reply, "version")})
	})
	mutateTest(t, f.s, f.actor, func(tx pgx.Tx) (Data, error) {
		return f.s.Decide(ctx, tx, f.actor, Str(ap, "id"), Data{"decision": "approve", "expected_version": Num(ap, "version")})
	})
	if e := f.s.executeApproval(ctx, Str(ap, "id")); e != nil {
		t.Fatal(e)
	}
	op, e := one(ctx, f.s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE approval_id=$1", Str(ap, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.dispatchExternal(ctx, Str(op, "id")); e != nil {
		t.Fatal(e)
	}
	op, e = locked(ctx, f.s.DB, "external_operations", Str(op, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if Str(op, "status") != "succeeded" {
		t.Fatalf("send did not succeed: %#v", op)
	}
	return op
}

func feedbackCardEvent(op Data, action string) Data {
	return Data{"operator": Data{"open_id": "ou_user", "tenant_key": "external-customer-tenant"}, "context": Data{"open_message_id": Str(op, "remote_id"), "open_chat_id": "oc_test"}, "action": Data{"name": "feedback_" + action, "value": Data{"operation_id": Str(op, "id"), "nonce": Str(Object(op, "request"), "card_nonce"), "action": "feedback_" + action}}}
}

func TestFeedbackLarkSendThenOrdinaryParticipantCallback(t *testing.T) {
	for _, choice := range []string{"resolved", "unresolved", "human"} {
		t.Run(choice, func(t *testing.T) {
			f := newFeedbackFixture(t, "feishu")
			op := f.send(t)
			ctx := context.Background()
			f.mu.Lock()
			payload := f.sent[0]
			f.mu.Unlock()
			if Str(payload, "msg_type") != "interactive" || !strings.Contains(Str(payload, "content"), "feedback_human") {
				t.Fatalf("missing feedback card: %#v", payload)
			}
			before, e := locked(ctx, f.s.DB, "cases", Str(f.c, "id"))
			if e != nil {
				t.Fatal(e)
			}
			if Num(before, "version") != Num(Object(Object(op, "request"), "feedback_context"), "case_version") {
				t.Fatal("freshly sent card is stale")
			}
			h, b := supportSigned(t, "feedback-"+choice, "card.action.trigger", feedbackCardEvent(op, choice))
			for range 2 {
				result, e := f.s.IngestWebhook(ctx, "feishu", f.connector, h, b)
				if e != nil {
					t.Fatal(e)
				}
				if Str(Object(result, "toast"), "type") != "success" {
					t.Fatalf("unexpected toast: %#v", result)
				}
				card := Object(result, "card")
				if Str(card, "type") != "raw" || !strings.Contains(string(JSON(card["data"])), "按钮已停用") {
					t.Fatalf("feedback card was not replaced: %#v", result)
				}
			}
			after, e := locked(ctx, f.s.DB, "cases", Str(f.c, "id"))
			if e != nil {
				t.Fatal(e)
			}
			wanted := "in_progress"
			if choice == "resolved" {
				wanted = "resolved"
			}
			if Str(after, "status") != wanted || Num(after, "version") != Num(before, "version")+1 {
				t.Fatalf("wrong feedback transition: %#v", after)
			}
			if choice == "human" && Str(after, "reply_mode") != "manual" {
				t.Fatal("human request did not stop auto replies")
			}
			if supportCount(t, f.s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type='feedback'", Str(f.c, "id")) != 1 || supportCount(t, f.s, "SELECT count(*) FROM dev_tasks") != 0 || supportCount(t, f.s, "SELECT count(*) FROM configs WHERE kind='identity-bindings'") != 0 {
				t.Fatal("duplicate or elevated feedback action")
			}
			if supportCount(t, f.s, "SELECT count(*) FROM approvals WHERE status='executed'") != 1 {
				t.Fatal("feedback altered developer/approval state")
			}
		})
	}
}

func TestFeedbackLarkRejectsForgedStaleAndUnrelatedActors(t *testing.T) {
	for _, name := range []string{"nonce", "remote_message", "channel", "nonparticipant", "case_injection", "approval_action", "source_recalled", "source_edited", "case_changed", "new_message", "new_reply"} {
		t.Run(name, func(t *testing.T) {
			f := newFeedbackFixture(t, "feishu")
			op := f.send(t)
			event := feedbackCardEvent(op, "resolved")
			ctx := context.Background()
			switch name {
			case "nonce":
				Object(Object(event, "action"), "value")["nonce"] = "wrong"
			case "remote_message":
				Object(event, "context")["open_message_id"] = "om_unrelated"
			case "channel":
				Object(event, "context")["open_chat_id"] = "oc_other"
			case "nonparticipant":
				Object(event, "operator")["open_id"] = "ou_random_member"
			case "case_injection":
				Object(Object(event, "action"), "value")["case_id"] = ID()
			case "approval_action":
				Object(Object(event, "action"), "value")["action"] = "approve"
			case "source_recalled":
				if _, e := f.s.DB.Exec(ctx, "UPDATE source_messages SET deleted=true,version=version+1 WHERE id=$1", f.source); e != nil {
					t.Fatal(e)
				}
			case "source_edited":
				if _, e := f.s.DB.Exec(ctx, "UPDATE source_messages SET source_revision='revision-2',version=version+1 WHERE id=$1", f.source); e != nil {
					t.Fatal(e)
				}
			case "case_changed":
				if _, e := f.s.DB.Exec(ctx, "UPDATE cases SET version=version+1 WHERE id=$1", Str(f.c, "id")); e != nil {
					t.Fatal(e)
				}
			case "new_message":
				message := supportMessage("om_followup", "The auth failure still happens", time.Now())
				Object(message, "message")["root_id"] = "om_source"
				h, b := supportSigned(t, "new-message", "im.message.receive_v1", message)
				if result, e := f.s.IngestWebhook(ctx, "feishu", f.connector, h, b); e != nil || !Bool(result, "accepted") {
					t.Fatalf("new message rejected: %#v %v", result, e)
				}
			case "new_reply":
				f.send(t)
			}
			h, b := supportSigned(t, "bad-"+name, "card.action.trigger", event)
			if _, e := f.s.IngestWebhook(ctx, "feishu", f.connector, h, b); e == nil {
				t.Fatal("unsafe feedback accepted")
			}
			if supportCount(t, f.s, "SELECT count(*) FROM case_events WHERE type='feedback'") != 0 {
				t.Fatal("unsafe callback changed a case")
			}
		})
	}
}

func githubFeedbackSigned(t *testing.T, op Data, choice string, actor, number int64) (http.Header, []byte) {
	t.Helper()
	command := "/agentowl feedback " + Str(op, "id") + " " + Str(Object(op, "request"), "card_nonce") + " " + choice
	body := JSON(Data{"action": "created", "repository": Data{"full_name": "owner/repo"}, "sender": Data{"id": actor, "type": "User"}, "issue": Data{"id": 100, "number": number}, "comment": Data{"id": 999, "body": command, "html_url": "https://github.com/owner/repo/issues/17#issuecomment-999", "updated_at": time.Now().UTC()}})
	mac := hmac.New(sha256.New, []byte("feedback-webhook-secret"))
	mac.Write(body)
	headers := http.Header{}
	headers.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	headers.Set("X-GitHub-Event", "issue_comment")
	headers.Set("X-GitHub-Delivery", "feedback-test")
	return headers, body
}

func TestFeedbackGitHubVerifiedCommentAndScope(t *testing.T) {
	f := newFeedbackFixture(t, "github")
	op := f.send(t)
	ctx := context.Background()
	f.mu.Lock()
	body := Str(f.sent[0], "body")
	f.mu.Unlock()
	if !strings.Contains(body, "/agentowl feedback "+Str(op, "id")) {
		t.Fatal("GitHub reply omitted feedback command")
	}
	for _, bad := range []struct{ actor, number int64 }{{99, 17}, {42, 18}} {
		h, b := githubFeedbackSigned(t, op, "resolved", bad.actor, bad.number)
		if _, e := f.s.IngestWebhook(ctx, "github", f.connector, h, b); e == nil {
			t.Fatal("unrelated user/issue feedback accepted")
		}
	}
	h, b := githubFeedbackSigned(t, op, "human", 42, 17)
	bad := h.Clone()
	bad.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	if _, e := f.s.IngestWebhook(ctx, "github", f.connector, bad, b); e == nil {
		t.Fatal("unsigned feedback accepted")
	}
	for i := 0; i < 2; i++ {
		result, e := f.s.IngestWebhook(ctx, "github", f.connector, h, b)
		if e != nil || !Bool(result, "feedback") {
			t.Fatalf("feedback failed: %#v %v", result, e)
		}
	}
	c, e := locked(ctx, f.s.DB, "cases", Str(f.c, "id"))
	if e != nil || Str(c, "reply_mode") != "manual" || Num(c, "version") != Num(f.c, "version")+1 {
		t.Fatalf("wrong feedback state: %#v %v", c, e)
	}
	if supportCount(t, f.s, "SELECT count(*) FROM case_events WHERE type='feedback'") != 1 {
		t.Fatal("duplicate GitHub feedback")
	}
}

func TestFeedbackWebRequiresRoleScopeVersionAndDeduplicates(t *testing.T) {
	s, actor := testService(t)
	ctx := context.Background()
	_, channel := supportLarkFixture(t, s)
	c := supportCase(t, s, actor, channel)
	for _, a := range []Actor{{ID: ID(), Role: "viewer", ChannelIDs: []string{channel}}, {ID: ID(), Role: "oncall", ChannelIDs: []string{ID()}}} {
		e := s.transaction(ctx, func(tx pgx.Tx) error {
			_, e := s.Feedback(ctx, tx, a, Str(c, "id"), Data{"action": "resolved", "expected_version": Num(c, "version")})
			return e
		})
		if e == nil {
			t.Fatal("unauthorized web feedback accepted")
		}
	}
	for _, body := range []Data{{"action": "resolved"}, {"action": "approve", "expected_version": Num(c, "version")}, {"action": "human", "expected_version": Num(c, "version"), "reason": strings.Repeat("字", 4001)}} {
		e := s.transaction(ctx, func(tx pgx.Tx) error { _, e := s.Feedback(ctx, tx, actor, Str(c, "id"), body); return e })
		if e == nil {
			t.Fatal("invalid feedback accepted")
		}
	}
	key := ID()
	input := Data{"action": "unresolved", "expected_version": Num(c, "version"), "reason": "Still failing"}
	calls := 0
	for i := 0; i < 2; i++ {
		_, _, e := s.Mutate(ctx, actor, key, Hash(JSON(input)), func(tx pgx.Tx) (Data, int, error) {
			calls++
			out, e := s.Feedback(ctx, tx, actor, Str(c, "id"), input)
			return out, 200, e
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	if calls != 1 {
		t.Fatal("web retry repeated the mutation")
	}
	after, e := locked(ctx, s.DB, "cases", Str(c, "id"))
	if e != nil || Num(after, "version") != Num(c, "version")+1 {
		t.Fatal(e, after)
	}
	e = s.transaction(ctx, func(tx pgx.Tx) error { _, e := s.Feedback(ctx, tx, actor, Str(c, "id"), input); return e })
	if !errors.Is(e, Conflict) {
		t.Fatalf("stale feedback accepted: %v", e)
	}
}

func TestManualReplyDoesNotOfferPlatformFeedback(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, channel := supportLarkFixture(t, s)
	c := supportCase(t, s, a, channel)
	request := Data{}
	e := s.transaction(ctx, func(tx pgx.Tx) error {
		return s.bindReplyFeedback(ctx, tx, request, Data{"id": ID(), "case_id": Str(c, "id"), "version": int64(1)}, Data{"case_version": Num(c, "version"), "channel_id": channel})
	})
	if e != nil || Str(request, "card_nonce") != "" {
		t.Fatal("manual case received platform feedback capability", e)
	}
	if replyFeedbackBody(Data{"kind": "reply_send", "request": request}, "answer") != "answer" {
		t.Fatal("manual answer received a command")
	}
}

func TestFeedbackCardContainsOnlyBoundActions(t *testing.T) {
	op := Data{"id": ID(), "kind": "reply_send", "request": Data{"card_nonce": Token(), "case_id": ID(), "body": "Answer body"}}
	card := string(replyFeedbackCard(op, "access_token=hidden-value"))
	if strings.Contains(card, "hidden-value") || strings.Contains(card, "case_id") || strings.Contains(card, `"approve"`) {
		t.Fatal("card leaked content or privileged controls")
	}
	for _, name := range []string{"resolved", "unresolved", "human"} {
		if !strings.Contains(card, "feedback_"+name) {
			t.Fatal("missing feedback choice")
		}
	}
	result := string(replyFeedbackResultCard(op, "resolved", false))
	if strings.Contains(result, "feedback_resolved") || !strings.Contains(result, "按钮已停用") || !strings.Contains(result, "Answer body") {
		t.Fatal("result card did not replace actions")
	}
}
