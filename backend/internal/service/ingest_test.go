package service

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

func supportConfig(t *testing.T, s *Service, kind string, data, secret Data) string {
	t.Helper()
	id := ID()
	enc := ""
	var e error
	if len(secret) > 0 {
		enc, e = s.Vault.Seal(JSON(secret), []byte(kind+":"+id))
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = s.DB.Exec(context.Background(), "INSERT INTO configs(id,kind,name,data,secrets) VALUES($1,$2,$3,$4,$5)", id, kind, id, JSON(data), enc)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func supportLarkFixture(t *testing.T, s *Service) (string, string) {
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "cli_test", "tenant_id": "tenant_test", "status": "verified", "bot_open_id": "ou_bot"}, Data{"app_secret": "secret", "verification_token": "verify-test", "encrypt_key": "encryption-test"})
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "oc_test", "enabled": true, "visibility": "internal", "reply_mode": "draft"}, nil)
	return connector, channel
}

func supportRuntimeMachine(t *testing.T, s *Service, baseURL string) string {
	t.Helper()
	return supportConfig(t, s, "runtime-machines", Data{"name": "runtime-" + ID(), "base_url": baseURL, "callback_base_url": "http://api.internal", "status": "active", "scheduling_weight": 1}, Data{"token": "runtime-service-token-1234567890", "callback_token": "runtime-callback-token-123456789"})
}

func supportCapabilityServer(t *testing.T, capabilities runner.Capabilities) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capabilities" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(capabilities)
	}))
	t.Cleanup(server.Close)
	return server, server.URL
}
func supportSigned(t *testing.T, eventID, kind string, event Data) (http.Header, []byte) {
	t.Helper()
	body := JSON(Data{"schema": "2.0", "header": Data{"event_id": eventID, "event_type": kind, "app_id": "cli_test", "tenant_key": "tenant_test", "token": "verify-test", "create_time": strconv.FormatInt(time.Now().UnixMilli(), 10)}, "event": event})
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha256.Sum256([]byte(timestamp + "nonce" + "encryption-test" + string(body)))
	return http.Header{"X-Lark-Request-Timestamp": {timestamp}, "X-Lark-Request-Nonce": {"nonce"}, "X-Lark-Signature": {hex.EncodeToString(sum[:])}}, body
}
func supportCardActionSigned(t *testing.T, body Data) (http.Header, []byte) {
	t.Helper()
	raw := JSON(body)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	h := sha1.New()
	h.Write([]byte(timestamp + "nonce" + "verify-test"))
	h.Write(raw)
	return http.Header{"X-Lark-Request-Timestamp": {timestamp}, "X-Lark-Request-Nonce": {"nonce"}, "X-Lark-Signature": {hex.EncodeToString(h.Sum(nil))}}, raw
}
func supportMessage(id, content string, when time.Time) Data {
	return Data{"sender": Data{"sender_id": Data{"open_id": "ou_user"}, "sender_type": "user", "tenant_key": "tenant_test"}, "message": Data{"message_id": id, "chat_id": "oc_test", "message_type": "text", "content": string(JSON(Data{"text": content})), "create_time": strconv.FormatInt(when.UnixMilli(), 10)}}
}
func supportCount(t *testing.T, s *Service, sql string, args ...any) int {
	t.Helper()
	var n int
	if e := s.DB.QueryRow(context.Background(), sql, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestSupportIngestAtomicDedupAndRecall(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	h, b := supportSigned(t, "event-one", "im.message.receive_v1", supportMessage("om_one", "auth failed access_token=top-secret-value", time.Now().Add(-time.Minute)))
	result, e := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if e != nil {
		t.Fatal(e)
	}
	eventID := Str(result, "id")
	if !Bool(result, "accepted") {
		t.Fatal(result)
	}
	result, e = s.IngestWebhook(ctx, "feishu", connector, h, b)
	if e != nil || !Bool(result, "duplicate") {
		t.Fatalf("duplicate: %v %v", result, e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events"); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='ingest_event'"); n != 1 {
		t.Fatal(n)
	}
	var payload string
	if e = s.DB.QueryRow(ctx, "SELECT payload::text FROM ingest_events WHERE id=$1", eventID).Scan(&payload); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(payload, "top-secret-value") {
		t.Fatal("secret persisted")
	}
	if e = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": eventID}); e != nil {
		t.Fatal(e)
	}
	if e = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": eventID}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases"); n != 1 {
		t.Fatal(n)
	}
	var caseID string
	if e = s.DB.QueryRow(ctx, "SELECT case_id FROM source_messages WHERE external_id='om_one'").Scan(&caseID); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases WHERE id=$1 AND channel_id=$2", caseID, channel); n != 1 {
		t.Fatal(n)
	}
	if _, e = s.DB.Exec(ctx, "INSERT INTO replies(id,case_id,body,status,case_version) SELECT $1,id,'old answer','pending',version FROM cases WHERE id=$2", ID(), caseID); e != nil {
		t.Fatal(e)
	}
	h, b = supportSigned(t, "event-recall", "im.message.recalled_v1", Data{"message_id": "om_one", "recall_time": strconv.FormatInt(time.Now().UnixMilli(), 10)})
	result, e = s.IngestWebhook(ctx, "feishu", connector, h, b)
	if e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM replies WHERE case_id=$1 AND status='stale'", caseID); n != 1 {
		t.Fatal("accepted recall did not invalidate reply before worker")
	}
	if e = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": Str(result, "id")}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM source_messages WHERE external_id='om_one' AND deleted AND content=''"); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM replies WHERE case_id=$1 AND status='stale'", caseID); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM source_message_versions"); n != 2 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases WHERE id=$1 AND title='已撤回的用户问题' AND description='' AND summary='' AND environment->>'last_message_id'=''", caseID); n != 1 {
		t.Fatal("recalled source remained in derived prompt fields")
	}
	h.Set("X-Lark-Signature", strings.Repeat("0", 64))
	if _, e = s.IngestWebhook(ctx, "feishu", connector, h, b); e == nil {
		t.Fatal("invalid signature accepted")
	}
	// A queue failure must also roll back the accepted inbox row.
	if _, e = s.DB.Exec(ctx, "CREATE FUNCTION reject_support_job() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic queue failure'; END $$; CREATE TRIGGER reject_support_job BEFORE INSERT ON jobs FOR EACH ROW EXECUTE FUNCTION reject_support_job()"); e != nil {
		t.Fatal(e)
	}
	h, b = supportSigned(t, "atomic-failure", "im.message.receive_v1", supportMessage("om_two", "token refresh", time.Now()))
	if _, e = s.IngestWebhook(ctx, "feishu", connector, h, b); e == nil {
		t.Fatal("failed queue acknowledged")
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events WHERE event_id='atomic-failure'"); n != 0 {
		t.Fatal("inbox committed without job")
	}
}

func TestSupportIngestScopeAndCardBinding(t *testing.T) {
	s, actor := testService(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	unknown := supportMessage("om_unknown", "auth", time.Now())
	Object(unknown, "message")["chat_id"] = "oc_other"
	h, b := supportSigned(t, "outside", "im.message.receive_v1", unknown)
	r, e := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if e != nil || !Bool(r, "ignored") {
		t.Fatalf("scope: %v %v", r, e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events"); n != 0 {
		t.Fatal(n)
	}
	var c Data
	e = s.transaction(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = s.SaveCase(ctx, tx, actor, "", Data{"title": "Test card", "channel_id": channel})
		return err
	})
	if e != nil {
		t.Fatal(e)
	}
	supportConfig(t, s, "identity-bindings", Data{"provider": "feishu", "app_id": "cli_test", "subject_id": "ou_admin", "principal_id": actor.ID}, nil)
	opID := ID()
	nonce := "server-secret-nonce"
	req := Data{"body": "claim", "card_nonce": nonce, "card_actions": Data{"claim": Data{"case_id": Str(c, "id"), "expected_version": Num(c, "version")}}}
	_, e = s.DB.Exec(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,status,target,request,remote_id) VALUES($1,'notify',$2,$5,'succeeded',$3,$4,'om_card')", opID, Str(c, "id"), JSON(Data{"channel_id": channel}), JSON(req), opID)
	if e != nil {
		t.Fatal(e)
	}
	makeCard := func(n, tenant string) Data {
		return Data{"open_id": "ou_admin", "tenant_key": tenant, "open_message_id": "om_card", "open_chat_id": "oc_test", "token": "verify-test", "action": Data{"name": "claim", "value": Data{"operation_id": opID, "nonce": n, "action": "claim"}}}
	}
	h, b = supportCardActionSigned(t, makeCard("wrong", "tenant_test"))
	if _, e = s.IngestCardAction(ctx, "feishu", connector, h, b); e == nil {
		t.Fatal("wrong nonce accepted")
	}
	h, b = supportCardActionSigned(t, makeCard(nonce, "external-tenant"))
	if _, e = s.IngestCardAction(ctx, "feishu", connector, h, b); e != nil {
		t.Fatal(e)
	}
	duplicateResult, e := s.IngestCardAction(ctx, "feishu", connector, h, b)
	if e != nil {
		t.Fatal(e)
	}
	card := Object(duplicateResult, "card")
	if Str(card, "type") != "raw" || !strings.Contains(string(JSON(card["data"])), "无需重复点击") {
		t.Fatalf("duplicate card was not replaced: %#v", duplicateResult)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases WHERE id=$1 AND assignee_id=$2 AND status='in_progress'", Str(c, "id"), actor.ID); n != 1 {
		t.Fatal(n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events WHERE kind='card.action.trigger'"); n != 1 {
		t.Fatal(n)
	}
	if _, e = s.IngestWebhook(ctx, "feishu", connector, h, b); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM configs WHERE kind='identity-bindings' AND data ? 'tenant_id'"); n != 0 {
		t.Fatal("tenant_id retained in identity binding", n)
	}
}

func TestSupportCardRenderingAndSourceClock(t *testing.T) {
	card := notificationCard("operation", Data{"body": "<malicious>", "card_nonce": "nonce", "card_actions": Data{"claim": Data{"case_id": "server"}, "delete_everything": Data{"admin": true}}})
	var parsed Data
	if e := json.Unmarshal(card, &parsed); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(card), "delete_everything") {
		t.Fatal("unrecognized action rendered")
	}
	if !strings.Contains(string(card), "plain_text") {
		t.Fatal("notification body is executable markdown")
	}
	result := string(notificationResultCard(Data{"request": Data{"body": "notify body"}}, "claim", false))
	if strings.Contains(result, "\"claim\"") || !strings.Contains(result, "已认领") || !strings.Contains(result, "按钮已停用") || !strings.Contains(result, "notify body") {
		t.Fatal("notification result card did not disable actions")
	}
	a := time.Now().UTC().Truncate(time.Millisecond)
	for _, input := range []string{a.Format(time.RFC3339Nano), strconv.FormatInt(a.UnixMilli(), 10), a.Format(time.RFC3339Nano) + ":deleted:hash"} {
		if !sourceClock(input).Equal(a) {
			t.Fatal(fmt.Sprintf("clock %s", input))
		}
	}
	for _, u := range []string{"javascript:alert(1)", "https://user:secret@example.com/a", "https://example.com/a?access_token=secret"} {
		if safeReferenceURL(u) {
			t.Fatal(u)
		}
	}
}

func TestSupportPATEchoBeforeRemoteIDIsRecorded(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "github", "owner": "owner", "repo": "repo"}, nil)
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "owner/repo", "enabled": true, "visibility": "public"}, nil)
	opID := ID()
	body := "Documented answer"
	marker, _ := integration.OperationMarker(opID)
	if _, e := s.DB.Exec(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,status,target,request) VALUES($1,'reply_send','reply',$2,'dispatching',$3,$4)", opID, opID, JSON(Data{"channel_id": channel}), JSON(Data{"body": body})); e != nil {
		t.Fatal(e)
	}
	var eventID string
	e := s.transaction(ctx, func(tx pgx.Tx) error {
		var err error
		eventID, _, err = s.persistEvent(ctx, tx, connector, Data{"id": channel}, integration.Event{ID: "pat-echo", Kind: "github.issue_comment", Repository: "owner/repo", ChannelID: "issue:1", MessageID: "comment:42", ActorID: "7", SourceRevision: time.Now().UTC().Format(time.RFC3339Nano), Text: body + "\n\n" + marker})
		return err
	})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": eventID}); e != nil {
		t.Fatal(e)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases"); n != 0 {
		t.Fatal("early PAT echo started another support cycle")
	}
}
