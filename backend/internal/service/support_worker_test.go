package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

func supportCase(t *testing.T, s *Service, a Actor, ch string) Data {
	t.Helper()
	var c Data
	e := s.transaction(context.Background(), func(tx pgx.Tx) error {
		var err error
		c, err = s.SaveCase(context.Background(), tx, a, "", Data{"title": "Auth login failed", "description": "How do I log in?", "channel_id": ch, "reply_mode": "draft", "module": "auth"})
		return err
	})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func supportKnowledge(t *testing.T, s *Service, visibility, body string) string {
	t.Helper()
	id := ID()
	_, e := s.DB.Exec(context.Background(), "INSERT INTO knowledge(id,title,body,status,visibility,author_id) VALUES($1,'Auth login',$2,'published',$3,'test')", id, body, visibility)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func supportReply(t *testing.T, s *Service, c Data) string {
	t.Helper()
	current, e := one(context.Background(), s.DB, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(c, "id"))
	if e != nil {
		t.Fatal(e)
	}
	c = current
	id := ID()
	_, e = s.DB.Exec(context.Background(), "INSERT INTO replies(id,case_id,status,case_version) VALUES($1,$2,'generating',$3)", id, Str(c, "id"), Num(c, "version"))
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func supportModelResponse(w http.ResponseWriter, content Data) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Data{"id": "completion-test", "model": "test", "choices": []any{Data{"index": 0, "finish_reason": "stop", "message": Data{"role": "assistant", "content": string(JSON(content))}}}, "usage": Data{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150}})
}

func TestClassificationFailureShowsSafeDetails(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&integration.APIError{Provider: "model", StatusCode: 401, Code: "http_error"}, "HTTP 401"},
		{integration.ErrInvalidModelOutput, "模型返回的数据格式不符合要求"},
		{errors.New("secret internal data"), "处理失败，请检查连接配置和运行记录"},
	} {
		got := classificationFailure(tc.err)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "secret internal data") {
			t.Fatalf("classificationFailure(%v) = %q", tc.err, got)
		}
	}
}

func TestClassificationFailureIsRecorded(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, ch := supportLarkFixture(t, s)
	c := supportCase(t, s, a, ch)
	if _, err := s.DB.Exec(ctx, "UPDATE cases SET reply_mode='auto' WHERE id=$1", Str(c, "id")); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"secret internal data"}}`))
	}))
	defer server.Close()
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": server.URL, "allow_http": true, "model_name": "test"})
	if err := s.ProcessSupportJob(ctx, "classify", Data{"case_id": Str(c, "id"), "historical": true}); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := s.DB.QueryRow(ctx, "SELECT body FROM case_events WHERE case_id=$1 AND type='needs_human'", Str(c, "id")).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "HTTP 401") || strings.Contains(body, "secret internal data") {
		t.Fatalf("unexpected failure detail: %q", body)
	}
}

func TestClassificationTransientErrorRetriesBeforeHumanEscalation(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		settings := executionSettingsBody(0)
		settings["job_retry_attempts"], settings["job_retry_base_seconds"], settings["job_retry_max_seconds"] = 2, 1, 1
		return s.SaveExecutionSettings(ctx, tx, a, settings)
	})
	_, ch := supportLarkFixture(t, s)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary model outage"}}`))
	}))
	defer server.Close()
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": server.URL, "allow_http": true, "model_name": "test"})
	c := supportCase(t, s, a, ch)
	if worked, err := s.WorkOnce(ctx); err != nil || !worked {
		t.Fatalf("first retry pass: worked=%v err=%v", worked, err)
	}
	if calls.Load() != 1 || supportCount(t, s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type='needs_human'", Str(c, "id")) != 0 {
		t.Fatalf("transient model error escalated before retry: calls=%d", calls.Load())
	}
	if _, err := s.DB.Exec(ctx, "UPDATE jobs SET available_at=now() WHERE kind='classify' AND payload->>'case_id'=$1", Str(c, "id")); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.WorkOnce(ctx); err != nil || !worked {
		t.Fatalf("second retry pass: worked=%v err=%v", worked, err)
	}
	if calls.Load() != 2 || supportCount(t, s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type='needs_human'", Str(c, "id")) != 1 {
		t.Fatalf("final model error was not escalated after retry limit: calls=%d", calls.Load())
	}
}

func TestRetryClassificationQueuesOnceAndUsesCurrentVersion(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, ch := supportLarkFixture(t, s)
	c := supportCase(t, s, a, ch)
	id := Str(c, "id")
	if _, err := s.DB.Exec(ctx, "UPDATE jobs SET status='succeeded' WHERE kind='classify' AND payload->>'case_id'=$1", id); err != nil {
		t.Fatal(err)
	}
	if err := s.ProcessSupportJob(ctx, "classify", Data{"case_id": id, "historical": true}); err != nil {
		t.Fatal(err)
	}
	retry := func(actor Actor, version int64) error {
		return s.transaction(ctx, func(tx pgx.Tx) error {
			_, err := s.CaseAction(ctx, tx, actor, id, Data{"action": "retry_classification", "expected_version": version})
			return err
		})
	}
	if err := retry(Actor{ID: ID(), Role: "viewer"}, Num(c, "version")); err != Forbidden {
		t.Fatalf("viewer retry: %v", err)
	}
	if err := retry(a, Num(c, "version")); err != nil {
		t.Fatal(err)
	}
	if err := retry(a, Num(c, "version")); err == nil || !strings.Contains(err.Error(), "正在处理") {
		t.Fatalf("duplicate retry: %v", err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='classify' AND status='pending' AND payload->>'case_id'=$1 AND (payload->>'historical')::boolean AND (payload->>'version')::bigint=1", id); n != 1 {
		t.Fatalf("expected one draft-only classification retry, got %d", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type='retry_classification'", id); n != 1 {
		t.Fatalf("expected one retry event, got %d", n)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supportModelResponse(w, Data{"support_relevance": "in_scope", "type": "security", "module": "auth", "reason": "需要人工处理", "missing_info": []string{}, "needs_human": true})
	}))
	defer server.Close()
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": server.URL, "allow_http": true, "model_name": "test"})
	if err := s.ProcessSupportJob(ctx, "classify", Data{"case_id": id, "version": Num(c, "version"), "historical": true}); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM case_events WHERE case_id=$1 AND type='classified'", id); n != 1 {
		t.Fatalf("retry did not classify case: %d", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='notify' AND payload->>'case_id'=$1", id); n != 0 {
		t.Fatalf("retry created an external notification: %d", n)
	}
	if _, err := s.DB.Exec(ctx, "UPDATE jobs SET status='succeeded' WHERE kind='classify' AND payload->>'case_id'=$1", id); err != nil {
		t.Fatal(err)
	}
	if err := retry(a, Num(c, "version")+1); err == nil || !strings.Contains(err.Error(), "没有需要重试") {
		t.Fatalf("retry accepted after successful classification: %v", err)
	}
}

func TestRetryClassificationRejectsClosedAndManualCases(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, ch := supportLarkFixture(t, s)
	c := supportCase(t, s, a, ch)
	id := Str(c, "id")
	for _, state := range []struct{ status, mode string }{{"closed", "draft"}, {"resolved", "draft"}, {"triage", "manual"}} {
		if _, err := s.DB.Exec(ctx, "UPDATE cases SET status=$2,reply_mode=$3 WHERE id=$1", id, state.status, state.mode); err != nil {
			t.Fatal(err)
		}
		err := s.transaction(ctx, func(tx pgx.Tx) error {
			_, err := s.CaseAction(ctx, tx, a, id, Data{"action": "retry_classification", "expected_version": Num(c, "version")})
			return err
		})
		if err == nil {
			t.Fatalf("retry accepted for status=%s mode=%s", state.status, state.mode)
		}
	}
}

func TestSupportAnswerPermissionsAndVersionGate(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	connector, ch := supportLarkFixture(t, s)
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", ch, JSON(Data{"visibility": "external", "reply_mode": "auto"})); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "tenant_access_token"):
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
		case r.URL.Path == "/open-apis/im/v1/messages/om_attachment/resources/image_test" && r.URL.Query().Get("type") == "image":
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nvision"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", connector, JSON(Data{"base_url": server.URL})); e != nil {
		t.Fatal(e)
	}
	c := supportCase(t, s, a, ch)
	if _, e := s.DB.Exec(ctx, "UPDATE cases SET reply_mode='auto' WHERE id=$1", Str(c, "id")); e != nil {
		t.Fatal(e)
	}
	public := supportKnowledge(t, s, "public", "PUBLIC_LOGIN_GUIDANCE")
	supportKnowledge(t, s, "internal", "PRIVATE_TEAM_PROCEDURE")
	privateSource := supportConfig(t, s, "knowledge-sources", Data{"kind": "manual", "status": "synced", "visibility": "internal"}, nil)
	for range 9 {
		k := supportKnowledge(t, s, "public", "PRIVATE_SOURCE_PROCEDURE")
		if _, e := s.DB.Exec(ctx, "UPDATE knowledge SET source_id=$2 WHERE id=$1", k, privateSource); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.DB.Exec(ctx, "UPDATE cases SET environment=$2 WHERE id=$1", Str(c, "id"), JSON(Data{"attachments": []integration.Attachment{{Key: "image_test", Type: "image", Name: "screen.png"}}, "attachment_message_id": "om_attachment", "attachment_source_revision": "rev-1", "attachment_connector_id": connector, "attachment_app_id": "cli_test", "attachment_tenant_id": "tenant_test"})); e != nil {
		t.Fatal(e)
	}
	when := time.Now().Add(-time.Minute)
	msg := supportMessage("om_attachment", "", when)
	Object(msg, "message")["root_id"] = Str(c, "id")
	if _, err := s.DB.Exec(ctx, `INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,actor_id,content,thread_id,source_revision) VALUES($1,$2,$3,'seed',$4::uuid,'ou_user','How do I log in?',$4::text,'seed')`, ID(), connector, ch, Str(c, "id")); err != nil {
		t.Fatal(err)
	}
	Object(msg, "message")["message_type"] = "image"
	Object(msg, "message")["content"] = string(JSON(Data{"image_key": "image_test"}))
	h, b := supportSigned(t, ID(), "im.message.receive_v1", msg)
	result, e := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": Str(result, "id")}); e != nil {
		t.Fatal(e)
	}
	var invalidate atomic.Bool
	var seen atomic.Int32
	serverModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "PRIVATE_TEAM_PROCEDURE") {
			t.Error("private evidence sent to model for external channel")
		}
		if strings.Contains(string(b), "PRIVATE_SOURCE_PROCEDURE") {
			t.Error("public knowledge widened private source scope")
		}
		if !strings.Contains(string(b), "PUBLIC_LOGIN_GUIDANCE") || !strings.Contains(string(b), "image_test") || !strings.Contains(string(b), "image_test") || !strings.Contains(string(b), "data:image/png;base64,") {
			t.Error("authorized evidence or image missing")
		}
		seen.Add(1)
		if invalidate.Load() {
			if _, e := s.DB.Exec(ctx, "UPDATE cases SET version=version+1 WHERE id=$1", Str(c, "id")); e != nil {
				t.Error(e)
			}
		}
		supportModelResponse(w, Data{"disposition": "answer", "text": "Use the documented login command.", "citation_ids": []string{public, "image_test"}, "missing_info": []string{}})
	}))
	defer serverModel.Close()
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": serverModel.URL, "allow_http": true, "model_name": "test"})
	reply := supportReply(t, s, c)
	if e = s.ProcessSupportJob(ctx, "answer", Data{"reply_id": reply, "automatic": true}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM approvals WHERE subject_id=$1 AND kind='reply_send' AND status='pending'", reply); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM budget_reservations WHERE actual=150 AND status='settled'"); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM replies WHERE id=$1 AND jsonb_array_length(image_evidence)=1", reply); n != 1 {
		t.Fatal(n)
	}
	invalidate.Store(true)
	reply = supportReply(t, s, c)
	if e = s.ProcessSupportJob(ctx, "answer", Data{"reply_id": reply, "automatic": true}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM replies WHERE id=$1 AND status='stale'", reply); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM approvals WHERE subject_id=$1", reply); n != 0 {
		t.Fatal("stale model output became approvable")
	}
	if seen.Load() != 2 {
		t.Fatal(seen.Load())
	}
}

func TestSupportAnswerKeepsEarlierImageAfterTextFollowup(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	connector, ch := supportLarkFixture(t, s)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "tenant_access_token"):
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
		case r.URL.Path == "/open-apis/im/v1/messages/om_attachment/resources/image_test" && r.URL.Query().Get("type") == "image":
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nvision"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", connector, JSON(Data{"base_url": server.URL})); e != nil {
		t.Fatal(e)
	}
	c := supportCase(t, s, a, ch)
	if _, e := s.DB.Exec(ctx, "UPDATE cases SET reply_mode='auto' WHERE id=$1", Str(c, "id")); e != nil {
		t.Fatal(e)
	}
	public := supportKnowledge(t, s, "public", "PUBLIC_LOGIN_GUIDANCE")
	msg := supportMessage("om_attachment", "", time.Now().Add(-time.Minute))
	Object(msg, "message")["root_id"] = Str(c, "id")
	if _, err := s.DB.Exec(ctx, `INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,actor_id,content,thread_id,source_revision) VALUES($1,$2,$3,'seed',$4::uuid,'ou_user','How do I log in?',$4::text,'seed')`, ID(), connector, ch, Str(c, "id")); err != nil {
		t.Fatal(err)
	}
	Object(msg, "message")["message_type"] = "image"
	Object(msg, "message")["content"] = string(JSON(Data{"image_key": "image_test"}))
	h, b := supportSigned(t, ID(), "im.message.receive_v1", msg)
	result, e := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": Str(result, "id")}); e != nil {
		t.Fatal(e)
	}
	followup := supportMessage("om_followup", "still broken", time.Now())
	Object(followup, "message")["root_id"] = Str(c, "id")
	h, b = supportSigned(t, ID(), "im.message.receive_v1", followup)
	result, e = s.IngestWebhook(ctx, "feishu", connector, h, b)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": Str(result, "id")}); e != nil {
		t.Fatal(e)
	}
	seen := atomic.Int32{}
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "data:image/png;base64,") || !strings.Contains(string(body), "image_test") {
			t.Error("earlier image was not retained")
		}
		seen.Add(1)
		supportModelResponse(w, Data{"disposition": "answer", "text": "Use the documented login command.", "citation_ids": []string{public, "image_test"}, "missing_info": []string{}})
	}))
	defer modelServer.Close()
	saveModelSettingTest(t, s, a, Data{"expected_version": 0, "enabled": true, "base_url": modelServer.URL, "allow_http": true, "model_name": "test"})
	reply := supportReply(t, s, c)
	if e = s.ProcessSupportJob(ctx, "answer", Data{"reply_id": reply, "automatic": true}); e != nil {
		t.Fatal(e)
	}
	if seen.Load() != 1 {
		t.Fatal(seen.Load())
	}
}

func TestSupportMissingModelAndNotificationAreDurable(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, ch := supportLarkFixture(t, s)
	c := supportCase(t, s, a, ch)
	supportConfig(t, s, "owner-routes", Data{"module": "auth", "primary_id": a.ID, "channel_id": ch, "enabled": true, "claim_minutes": 15, "resolve_minutes": 60}, nil)
	if e := s.ProcessSupportJob(ctx, "classify", Data{"case_id": Str(c, "id")}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM replies"); n != 0 {
		t.Fatal("missing model generated a reply")
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='notify'"); n != 1 {
		t.Fatal(n)
	}
	if e := s.ProcessSupportJob(ctx, "notify", Data{"case_id": Str(c, "id"), "reason": "Missing model"}); e != nil {
		t.Fatal(e)
	}
	if e := s.ProcessSupportJob(ctx, "notify", Data{"case_id": Str(c, "id"), "reason": "Missing model"}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM external_operations WHERE kind='notify' AND status='pending'"); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='dispatch_external'"); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE dedupe_key LIKE 'notify-timeout:%'"); n != 2 {
		t.Fatal(n)
	}
	if e := s.MaintainSupport(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.MaintainSupport(ctx); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE dedupe_key LIKE 'notify-timeout:%'"); n != 2 {
		t.Fatal("maintenance duplicated SLA jobs")
	}
}

func TestSupportSourceSyncDraftAndPermissionWithdrawal(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	var deny atomic.Bool
	var revision atomic.Int64
	revision.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		if deny.Load() {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"code":99991672}`))
			return
		}
		switch r.URL.Path {
		case "/open-apis/docx/v1/documents/doc_test":
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "data": Data{"document": Data{"document_id": "doc_test", "revision_id": revision.Load(), "title": "Auth reference"}}})
		case "/open-apis/docx/v1/documents/doc_test/raw_content":
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "data": Data{"content": "Reference text revision " + string(rune('0'+revision.Load()))}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "test", "base_url": server.URL}, Data{"app_secret": "secret"})
	source := supportConfig(t, s, "knowledge-sources", Data{"kind": "lark", "connector_id": connector, "document_id": "doc_test", "url": "https://example.feishu.cn/docx/doc_test", "visibility": "public"}, nil)
	if e := s.ProcessSupportJob(ctx, "sync_source", Data{"source_id": source}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM knowledge WHERE source_id=$1 AND status='draft'", source); n != 1 {
		t.Fatal(n)
	}
	if _, e := s.DB.Exec(ctx, "UPDATE knowledge SET status='published' WHERE source_id=$1", source); e != nil {
		t.Fatal(e)
	}
	if e := s.ProcessSupportJob(ctx, "sync_source", Data{"source_id": source}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM knowledge WHERE source_id=$1 AND status='published'", source); n != 1 {
		t.Fatal("unchanged sync withdrew published evidence")
	}
	revision.Store(2)
	if e := s.ProcessSupportJob(ctx, "sync_source", Data{"source_id": source}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM knowledge WHERE source_id=$1 AND status='draft' AND version=2", source); n != 1 {
		t.Fatal(n)
	}
	if _, e := s.DB.Exec(ctx, "UPDATE knowledge SET status='published' WHERE source_id=$1", source); e != nil {
		t.Fatal(e)
	}
	deny.Store(true)
	if e := s.ProcessSupportJob(ctx, "sync_source", Data{"source_id": source}); e == nil || !strings.Contains(e.Error(), "99991672") {
		t.Fatalf("permission failure must be returned to the job: %v", e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM knowledge WHERE source_id=$1 AND status='withdrawn'", source); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM configs WHERE id=$1 AND data->>'status'='failed'", source); n != 1 {
		t.Fatal(n)
	}
	deny.Store(false)
	if e := s.ProcessSupportJob(ctx, "sync_source", Data{"source_id": source}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM knowledge WHERE source_id=$1 AND status='draft'", source); n != 1 {
		t.Fatal("restored access must require another review")
	}
}

func TestSupportDispatchUnknownAndReconciliationNeverReplay(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	var posts atomic.Int32
	var marker string
	body := "Approved answer"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(503)
			return
		}
		if r.Method != http.MethodGet {
			t.Error(r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Data{{"id": 42, "user": Data{"id": 7, "login": "agentowl"}, "body": body + "\n\n" + marker, "html_url": "https://github.com/owner/repo/issues/1#issuecomment-42"}})
	}))
	defer server.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "github", "owner": "owner", "repo": "repo", "base_url": server.URL, "sender_id": "7"}, Data{"token": "test-token"})
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "owner/repo", "enabled": true, "visibility": "public"}, nil)
	c := supportCase(t, s, a, channel)
	if _, e := s.DB.Exec(ctx, "UPDATE cases SET environment='{\"source_number\":1}'::jsonb WHERE id=$1", Str(c, "id")); e != nil {
		t.Fatal(e)
	}
	opID := ID()
	marker, _ = integration.OperationMarker(opID)
	op := Data{"id": opID, "kind": "reply_send", "status": "dispatching", "target": Data{"channel_id": channel}, "request": Data{"case_id": Str(c, "id"), "body": body}}
	_, e := s.DispatchMessage(ctx, op)
	var api *integration.APIError
	if !errors.As(e, &api) || !api.OutcomeUnknown {
		t.Fatalf("expected uncertain write: %v", e)
	}
	if posts.Load() != 1 {
		t.Fatal("send was replayed")
	}
	result, found, e := s.ReconcileMessage(ctx, op)
	if e != nil || !found || Str(result, "remote_id") != "42" {
		t.Fatalf("reconcile %v %v %v", result, found, e)
	}
	if posts.Load() != 1 {
		t.Fatal("reconcile wrote remotely")
	}
}

func TestSupportTopicHistoryPersistsIndependentPages(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	var chats, threads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/open-apis/im/v1/messages" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		q := r.URL.Query()
		var items []Data
		more := false
		cursor := ""
		msg := func(id, thread string) Data {
			return Data{"message_id": id, "chat_id": "oc_test", "thread_id": thread, "msg_type": "text", "create_time": "1760000000000", "body": Data{"content": `{"text":"auth help"}`}, "sender": Data{"id": "ou_user", "sender_type": "user"}}
		}
		if q.Get("container_id_type") == "thread" {
			threads.Add(1)
			if q.Get("start_time") != "" || q.Get("end_time") != "" {
				t.Error("thread request uses unsupported time filter")
			}
			items = []Data{msg("om_reply", "omt_test")}
		} else {
			chats.Add(1)
			if q.Get("page_token") == "" {
				items = []Data{msg("om_root", "omt_test")}
				more = true
				cursor = "next-page"
			} else if q.Get("page_token") == "next-page" {
				items = []Data{msg("om_other", "")}
			} else {
				t.Error("unexpected cursor")
			}
		}
		_ = json.NewEncoder(w).Encode(Data{"code": 0, "data": Data{"items": items, "has_more": more, "page_token": cursor}})
	}))
	defer server.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "test", "base_url": server.URL}, Data{"app_secret": "secret"})
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "oc_test", "enabled": true, "visibility": "internal", "reply_mode": "auto"}, nil)
	if e := s.ProcessSupportJob(ctx, "sync_channel", Data{"channel_id": channel, "version": 1}); e != nil {
		t.Fatal(e)
	}
	jobs, e := many(ctx, s.DB, "SELECT to_jsonb(j) FROM jobs j WHERE kind='sync_channel' ORDER BY created_at")
	if e != nil {
		t.Fatal(e)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected root next page and topic child, got %d", len(jobs))
	}
	for _, j := range jobs {
		if e = s.ProcessSupportJob(ctx, "sync_channel", Object(j, "payload")); e != nil {
			t.Fatal(e)
		}
	}
	if chats.Load() != 2 || threads.Load() != 1 {
		t.Fatalf("chat pages=%d thread pages=%d", chats.Load(), threads.Load())
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events"); n != 3 {
		t.Fatal(n)
	}
	events, e := many(ctx, s.DB, "SELECT to_jsonb(e) FROM ingest_events e ORDER BY created_at")
	if e != nil {
		t.Fatal(e)
	}
	for _, ev := range events {
		if e = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": Str(ev, "id")}); e != nil {
			t.Fatal(e)
		}
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases"); n != 2 {
		t.Fatalf("topic root/reply did not group: %d", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases WHERE reply_mode='draft' AND environment->>'historical'='true'"); n != 2 {
		t.Fatal("backfill activated automatic replies")
	}
	classifications, e := many(ctx, s.DB, "SELECT to_jsonb(j) FROM jobs j WHERE kind='classify'")
	if e != nil {
		t.Fatal(e)
	}
	for _, j := range classifications {
		if e = s.ProcessSupportJob(ctx, "classify", Object(j, "payload")); e != nil {
			t.Fatal(e)
		}
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='notify'"); n != 0 {
		t.Fatal("historical import paged oncall")
	}
}

func TestSupportEvidenceSourceScopesAndDependencyRevocation(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	_, channel := supportLarkFixture(t, s)
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=data||'{\"visibility\":\"external\"}'::jsonb WHERE id=$1", channel); e != nil {
		t.Fatal(e)
	}
	c := Data{"channel_id": channel}
	source := supportConfig(t, s, "knowledge-sources", Data{"kind": "manual", "status": "synced", "visibility": "public", "channel_ids": []string{channel}}, nil)
	knowledge := supportKnowledge(t, s, "public", "Reviewed public knowledge")
	if _, e := s.DB.Exec(ctx, "UPDATE knowledge SET source_id=$2 WHERE id=$1", knowledge, source); e != nil {
		t.Fatal(e)
	}
	check := func(allowed bool) {
		t.Helper()
		_, e := s.evidence(ctx, s.DB, c, []string{knowledge})
		if allowed && e != nil {
			t.Fatal(e)
		}
		if !allowed && e == nil {
			t.Fatal("invalid source allowed evidence")
		}
	}
	check(true)
	for _, patch := range []Data{{"enabled": false}, {"enabled": true, "visibility": "internal"}, {"visibility": "public", "channel_ids": []string{ID()}}} {
		if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb WHERE id=$1", source, JSON(patch)); e != nil {
			t.Fatal(e)
		}
		check(false)
	}
	repo := supportConfig(t, s, "repositories", Data{"enabled": true, "owner": "owner", "repo": "repo"}, nil)
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=$2 WHERE id=$1", source, JSON(Data{"kind": "repository", "status": "synced", "visibility": "public", "repository_id": repo, "dependency_versions": Data{repo: 1}})); e != nil {
		t.Fatal(e)
	}
	check(true)
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=data||'{\"enabled\":false}'::jsonb WHERE id=$1", repo); e != nil {
		t.Fatal(e)
	}
	check(false)
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=data||'{\"enabled\":true}'::jsonb,version=version+1 WHERE id=$1", repo); e != nil {
		t.Fatal(e)
	}
	check(false)
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=jsonb_set(data,'{dependency_versions}',$2) WHERE id=$1", source, JSON(Data{repo: 2})); e != nil {
		t.Fatal(e)
	}
	check(true)
	connector := supportConfig(t, s, "connectors", Data{"provider": "github", "status": "disabled"}, nil)
	if _, e := s.DB.Exec(ctx, "UPDATE configs SET data=$2 WHERE id=$1", source, JSON(Data{"kind": "lark", "status": "synced", "visibility": "public", "connector_id": connector, "dependency_versions": Data{connector: 1}})); e != nil {
		t.Fatal(e)
	}
	check(false)
}

func TestSupportKnowledgeVersionsRespectSnapshotScope(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	id := supportKnowledge(t, s, "public", "Current public edition")
	snapshots := []Data{{"id": id, "version": 1, "title": "Private history", "body": "SECRET_CHANNEL_HISTORY", "visibility": "channel", "channel_ids": []string{ID()}, "author_id": ID()}, {"id": id, "version": 2, "title": "Public edition", "body": "PUBLIC_HISTORY", "visibility": "public", "author_id": ID()}}
	for _, snap := range snapshots {
		if _, e := s.DB.Exec(ctx, "INSERT INTO knowledge_versions(knowledge_id,version,snapshot) VALUES($1,$2,$3)", id, Num(snap, "version"), JSON(snap)); e != nil {
			t.Fatal(e)
		}
	}
	result, e := s.KnowledgeVersions(ctx, Actor{ID: ID(), Role: "viewer"}, id)
	if e != nil {
		t.Fatal(e)
	}
	if Num(result, "total") != 1 || strings.Contains(string(JSON(result)), "SECRET_CHANNEL_HISTORY") {
		t.Fatal("restricted historical snapshot leaked")
	}
}
