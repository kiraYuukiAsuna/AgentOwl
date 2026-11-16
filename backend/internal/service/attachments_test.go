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
	"time"

	"agentowl/backend/internal/integration"
)

func attachmentFixture(t *testing.T, resource http.HandlerFunc) (*Service, Actor, string, string, Data) {
	t.Helper()
	s, a := testService(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/open-apis/im/v1/messages/om_attachment/resources/file_test" || r.URL.Query().Get("type") != "file" {
			t.Errorf("unexpected resource request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		resource(w, r)
	}))
	t.Cleanup(server.Close)
	co, ch := supportLarkFixture(t, s)
	if _, e := s.DB.Exec(context.Background(), "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", co, JSON(Data{"base_url": server.URL})); e != nil {
		t.Fatal(e)
	}
	msg := supportMessage("om_attachment", "", time.Now().Add(-time.Minute))
	Object(msg, "message")["message_type"] = "file"
	Object(msg, "message")["content"] = string(JSON(Data{"file_key": "file_test", "file_name": "auth.log"}))
	h, b := supportSigned(t, ID(), "im.message.receive_v1", msg)
	result, e := s.IngestWebhook(context.Background(), "feishu", co, h, b)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ProcessSupportJob(context.Background(), "ingest_event", Data{"event_id": Str(result, "id")}); e != nil {
		t.Fatal(e)
	}
	c, e := one(context.Background(), s.DB, "SELECT to_jsonb(c) FROM cases c WHERE channel_id=$1", ch)
	if e != nil {
		t.Fatal(e)
	}
	return s, a, co, ch, c
}

func attachmentStatus(t *testing.T, e error, status int) {
	t.Helper()
	var p *Error
	if !errors.As(e, &p) || p.Status != status {
		t.Fatalf("want %d, got %v", status, e)
	}
}

func TestAttachmentReadProvenanceAndBounds(t *testing.T) {
	ctx := context.Background()
	var reads atomic.Int32
	s, a, co, ch, c := attachmentFixture(t, func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		_, _ = w.Write([]byte("original diagnostic text access_token=not-redacted-for-download"))
	})
	got, e := s.ReadCaseAttachment(ctx, a, Str(c, "id"), 0, Num(c, "version"))
	if e != nil || got.Filename != "auth.log" || got.MediaType != "text/plain; charset=utf-8" || !strings.Contains(string(got.Bytes), "not-redacted-for-download") {
		t.Fatalf("%+v %v", got, e)
	}
	current, _, e := s.config(ctx, s.DB, co, "connectors")
	if e != nil || Num(current, "version") != 1 || Str(Object(Object(current, "capabilities"), "attachment_read"), "status") != "observed" || !contains(Strings(Object(Object(current, "capabilities"), "attachment_read"), "channel_ids"), ch) {
		t.Fatalf("observation: %v %v", current, e)
	}
	if entry := Object(Object(current, "capabilities"), "receive_messages"); Str(entry, "status") != "observed" || len(Strings(entry, "channel_ids")) != 1 || Strings(entry, "channel_ids")[0] != ch {
		t.Fatalf("signed receipt scope: %v", entry)
	}
	_, e = s.ReadCaseAttachment(ctx, a, Str(c, "id"), 0, 0)
	attachmentStatus(t, e, 422)
	_, e = s.ReadCaseAttachment(ctx, a, Str(c, "id"), 0, Num(c, "version")+1)
	attachmentStatus(t, e, 409)
	_, e = s.ReadCaseAttachment(ctx, a, Str(c, "id"), 1, Num(c, "version"))
	attachmentStatus(t, e, 404)
	if reads.Load() != 1 {
		t.Fatal("invalid selection made a network request")
	}
	// The editable case environment is never sufficient to prove resource access.
	env := Object(c, "environment")
	for _, mutation := range []string{"key", "message", "app", "revision"} {
		copy := copyData(env)
		switch mutation {
		case "key":
			copy["attachments"] = []integration.Attachment{{Key: "file_other_group", Type: "file", Name: "auth.log"}}
		case "message":
			copy["attachment_message_id"] = "om_other_group"
		case "app":
			copy["attachment_app_id"] = "different_app"
		case "revision":
			copy["attachment_source_revision"] = "different_revision"
		}
		if _, e = s.DB.Exec(ctx, "UPDATE cases SET environment=$2 WHERE id=$1", Str(c, "id"), JSON(copy)); e != nil {
			t.Fatal(e)
		}
		_, e = s.ReadCaseAttachment(ctx, a, Str(c, "id"), 0, Num(c, "version"))
		attachmentStatus(t, e, 409)
	}
	if reads.Load() != 1 {
		t.Fatal("forged environment reached provider")
	}
}

func TestAttachmentRevocationBeforeAndDuringRead(t *testing.T) {
	for _, mutation := range []string{"pending_recall", "deleted", "connector", "principal", "acl", "during_version", "during_acl", "during_recall"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			var during func()
			var reads atomic.Int32
			s, a, co, _, c := attachmentFixture(t, func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if during != nil {
					during()
				}
				_, _ = w.Write([]byte("diagnostic text"))
			})
			change := func(sql string, args ...any) {
				if _, e := s.DB.Exec(ctx, sql, args...); e != nil {
					t.Error(e)
				}
			}
			want := 409
			switch mutation {
			case "pending_recall":
				h, b := supportSigned(t, ID(), "im.message.recalled_v1", Data{"message_id": "om_attachment", "recall_time": "9999999999999"})
				if _, e := s.IngestWebhook(ctx, "feishu", co, h, b); e != nil {
					t.Fatal(e)
				}
				c, _ = one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(c, "id"))
			case "deleted":
				change("UPDATE source_messages SET deleted=true WHERE case_id=$1", Str(c, "id"))
			case "connector":
				change("UPDATE configs SET data=data||'{\"status\":\"disabled\"}'::jsonb WHERE id=$1", co)
			case "principal":
				change("UPDATE principals SET active=false WHERE id=$1", a.ID)
				want = 403
			case "acl":
				change("UPDATE principals SET role='viewer',roles='{}',channel_ids='{}' WHERE id=$1", a.ID)
				want = 404 // case ACL deliberately conceals an inaccessible case
			case "during_version":
				during = func() { change("UPDATE cases SET version=version+1 WHERE id=$1", Str(c, "id")) }
			case "during_acl":
				during = func() { change("UPDATE principals SET active=false WHERE id=$1", a.ID) }
				want = 403
			case "during_recall":
				during = func() {
					change("UPDATE source_messages SET deleted=true,version=version+1 WHERE case_id=$1", Str(c, "id"))
				}
			}
			got, e := s.ReadCaseAttachment(ctx, a, Str(c, "id"), 0, Num(c, "version"))
			attachmentStatus(t, e, want)
			if len(got.Bytes) != 0 || !strings.HasPrefix(mutation, "during_") && reads.Load() != 0 {
				t.Fatal("revoked attachment returned or fetched")
			}
		})
	}
}

func TestAttachmentRejectLargeAndUnsafeContent(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{{"large", strings.Repeat("x", MaxAttachmentBytes+1), 413}, {"html", "<!DOCTYPE html><script>alert(1)</script>", 415}, {"svg", "<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>", 415}, {"binary", "\x7fELF\x00binary", 415}} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, _, _, c := attachmentFixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) })
			_, e := s.ReadCaseAttachment(context.Background(), a, Str(c, "id"), 0, Num(c, "version"))
			attachmentStatus(t, e, tc.status)
		})
	}
}

func TestCapabilityReadProbesAreScopedAndNeverSend(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	var tokens atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			tokens.Add(1)
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		if r.Method != http.MethodGet {
			t.Error("verification attempted a remote write")
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/open-apis/bot/v3/info":
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "bot": Data{"open_id": "ou_bot", "activate_status": 2}})
		case "/open-apis/im/v1/messages":
			if r.URL.Query().Get("container_id") == "oc_denied" {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"code":99991672}`))
				return
			}
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "data": Data{"items": []Data{{"chat_id": "oc_test", "thread_id": "omt_test", "message_id": "om_sample"}}, "has_more": false}})
		default:
			t.Errorf("unexpected probe %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	co, ch := supportLarkFixture(t, s)
	denied := supportConfig(t, s, "channels", Data{"connector_id": co, "external_id": "oc_denied", "enabled": true}, nil)
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", co, JSON(Data{"base_url": server.URL})); e != nil {
		t.Fatal(e)
	}
	if e := s.verifySupportConnector(ctx, Data{"connector_id": co}); e != nil {
		t.Fatal(e)
	}
	c, _, e := s.config(ctx, s.DB, co, "connectors")
	if e != nil {
		t.Fatal(e)
	}
	matrix := Object(c, "capabilities")
	if len(matrix) != len(capabilityKeys) || Num(c, "version") != 1 {
		t.Fatalf("matrix/config: %v", c)
	}
	for _, key := range []string{"send", "card_callback", "receive_messages", "receive_mentions", "attachment_read"} {
		if Str(Object(matrix, key), "status") != "unverified" {
			t.Fatalf("unproven %s claimed: %v", key, matrix[key])
		}
	}
	for _, key := range []string{"history_read", "thread_read"} {
		entry := Object(matrix, key)
		if Str(entry, "status") != "verified" || !contains(Strings(entry, "channel_ids"), ch) || contains(Strings(entry, "channel_ids"), denied) {
			t.Fatalf("scope %s: %v", key, entry)
		}
	}
	if e = s.verifySupportConnector(ctx, Data{"connector_id": co}); e != nil || tokens.Load() != 1 {
		t.Fatalf("metadata observation discarded token cache: tokens=%d err=%v", tokens.Load(), e)
	}
	if ok, e := s.SendCapabilityObserved(ctx, s.DB, ch); e != nil || ok {
		t.Fatalf("read probes authorized automatic write: %v %v", ok, e)
	}
	op := ID()
	if _, e = s.DB.Exec(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request,status) VALUES($1,'notify','test',$2,$3,'{}','succeeded')", op, "capability:"+op, JSON(Data{"channel_id": ch, "channel_version": 1, "connector_version": 1})); e != nil {
		t.Fatal(e)
	}
	if ok, e := s.SendCapabilityObserved(ctx, s.DB, ch); e != nil || !ok {
		t.Fatalf("durable current success not recognized: %v %v", ok, e)
	}
	if _, e = s.DB.Exec(ctx, "UPDATE configs SET data=data||'{\"status\":\"failed\"}'::jsonb WHERE id=$1", co); e != nil {
		t.Fatal(e)
	}
	if ok, e := s.SendCapabilityObserved(ctx, s.DB, ch); e != nil || ok {
		t.Fatalf("failed verification reused old send proof: %v %v", ok, e)
	}
}
