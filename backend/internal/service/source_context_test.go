package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestArchiveStoresUntargetedGroupMessagesWithoutCase(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	if _, err := s.DB.Exec(ctx, "UPDATE configs SET data=data||'{\"reply_mode\":\"mentions\"}'::jsonb WHERE id=$1", channel); err != nil {
		t.Fatal(err)
	}
	h, b := supportSigned(t, "untargeted", "im.message.receive_v1", supportMessage("om_background", "background chatter", time.Now()))
	result, err := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": Str(result, "id")}); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM source_messages WHERE external_id='om_background' AND case_id IS NULL AND content='background chatter'"); n != 1 {
		t.Fatalf("background message not archived without case: %d", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases"); n != 0 {
		t.Fatalf("background message created case: %d", n)
	}
}

func TestMentionModeRequiresBotMentionButKeepsThreadAssociation(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	if _, err := s.DB.Exec(ctx, "UPDATE configs SET data=data||'{\"reply_mode\":\"mentions\"}'::jsonb WHERE id=$1", channel); err != nil {
		t.Fatal(err)
	}
	root := supportMessage("om_thread_root", "root without bot mention", time.Now().Add(-time.Minute))
	Object(root, "message")["thread_id"] = "thread-mention"
	Object(root, "message")["mentions"] = []Data{{"id": Data{"open_id": "ou_someone_else"}}}
	h, b := supportSigned(t, "root-no-bot", "im.message.receive_v1", root)
	result, err := s.IngestWebhook(ctx, "feishu", connector, h, b)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": Str(result, "id")}); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM source_messages WHERE external_id='om_thread_root' AND case_id IS NULL"); n != 1 {
		t.Fatal("unmentioned root not archived independently", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases"); n != 0 {
		t.Fatal("unmentioned root created case", n)
	}
	follow := supportMessage("om_thread_follow", "follow mentions bot", time.Now())
	Object(follow, "message")["root_id"] = "om_thread_root"
	Object(follow, "message")["parent_id"] = "om_thread_root"
	Object(follow, "message")["thread_id"] = "thread-mention"
	Object(follow, "message")["mentions"] = []Data{{"id": Data{"open_id": "ou_bot"}}}
	h, b = supportSigned(t, "follow-with-bot", "im.message.receive_v1", follow)
	result, err = s.IngestWebhook(ctx, "feishu", connector, h, b)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSupportJob(ctx, "ingest_event", Data{"event_id": Str(result, "id")}); err != nil {
		t.Fatal(err)
	}
	var caseID string
	if err = s.DB.QueryRow(ctx, "SELECT case_id FROM source_messages WHERE external_id='om_thread_follow'").Scan(&caseID); err != nil || caseID == "" {
		t.Fatal("mentioned followup did not create case", caseID, err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM source_messages WHERE external_id='om_thread_root' AND case_id IS NULL"); n != 1 {
		t.Fatal("archived root was incorrectly adopted into case", n)
	}
}

func TestReplyAndNotifySuccessArchiveOwnOutput(t *testing.T) {
	for _, provider := range []string{"feishu", "github"} {
		t.Run(provider, func(t *testing.T) {
			f := newFeedbackFixture(t, provider)
			op := f.send(t)
			externalID := Str(op, "remote_id")
			if provider == "github" {
				externalID = "comment:" + externalID
			}
			m, err := one(context.Background(), f.s.DB, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND external_id=$3", f.connector, f.channel, externalID)
			if err != nil {
				t.Fatal(err)
			}
			if !Bool(m, "bot") || Str(m, "case_id") != Str(f.c, "id") || !strings.Contains(Str(m, "content"), "documented login flow") {
				t.Fatalf("own reply not archived as bot case message: %#v", m)
			}
		})
	}

	f := newFeedbackFixture(t, "feishu")
	ctx := context.Background()
	opID := ID()
	if _, err := f.s.DB.Exec(ctx, `INSERT INTO external_operations(id,kind,subject_id,dedupe_key,status,target,request) VALUES($1,'notify',$2,$3,'dispatching',$4,$5)`, opID, Str(f.c, "id"), "notify-success:"+opID, JSON(Data{"channel_id": f.channel, "channel_version": 1, "connector_id": f.connector, "connector_version": 1}), JSON(Data{"case_id": Str(f.c, "id"), "body": "Oncall notification body"})); err != nil {
		t.Fatal(err)
	}
	op, err := one(ctx, f.s.DB, "SELECT to_jsonb(o) FROM external_operations o WHERE id=$1", opID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.finishExternal(ctx, op, "succeeded", Data{"remote_id": "om_notify_success", "response": Data{"message_id": "om_notify_success"}}, ""); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, f.s, "SELECT count(*) FROM source_messages WHERE connector_id=$1 AND channel_id=$2 AND external_id='om_notify_success' AND bot AND content='Oncall notification body'", f.connector, f.channel); n != 1 {
		t.Fatalf("notify output not archived once: %d", n)
	}
}

func TestTaskInputSourceContextLinksQuoteThreadRecentAndKnowledge(t *testing.T) {
	s, a, task, _ := runtimeTaskFixture(t)
	ctx := context.Background()
	connector, channel := supportLarkFixture(t, s)
	caseID := Str(task, "case_id")
	base := time.Now().Add(-5 * time.Minute).UTC()
	rows := []struct {
		id, parent, root, actor, body string
		bot                           bool
		at                            time.Time
	}{
		{"om_root", "", "", "ou_user", "root question", false, base},
		{"om_bot", "om_root", "om_root", "ou_bot", "old answer", true, base.Add(time.Minute)},
		{"om_follow", "om_root", "om_root", "ou_user", "follow up", false, base.Add(2 * time.Minute)},
	}
	for _, r := range rows {
		_, err := s.DB.Exec(ctx, `INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,actor_id,content,thread_id,root_id,parent_id,platform_thread_id,actor_type,bot,occurred_at,source_revision,content_digest) VALUES($1,$2,$3,$4,$5,$6,$7,'thread-1',$8,$9,'thread-1',$10,$11,$12,$13,$14)`, ID(), connector, channel, r.id, caseID, r.actor, r.body, r.root, r.parent, map[bool]string{true: "bot", false: "user"}[r.bot], r.bot, r.at, r.at.Format(time.RFC3339Nano), Hash([]byte(r.body)))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO source_sync_status(connector_id,channel_id,thread_key,backfilled,synced_until,last_error) VALUES($1,$2,'thread-1',true,$3,'')`, connector, channel, base.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	standing := supportKnowledge(t, s, "internal", "Always mention supported versions")
	if _, err := s.DB.Exec(ctx, "UPDATE knowledge SET module='standing' WHERE id=$1", standing); err != nil {
		t.Fatal(err)
	}
	related := supportKnowledge(t, s, "internal", "follow up troubleshooting guide")
	if related == "" {
		t.Fatal("missing related knowledge")
	}
	source, err := one(ctx, s.DB, "SELECT to_jsonb(m) FROM source_messages m WHERE external_id='om_follow'")
	if err != nil {
		t.Fatal(err)
	}
	input := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.appendTaskInput(ctx, tx, task, Str(source, "content"), "source", Data{"id": "ou_user"}, source)
	})
	ctxInfo, err := s.TaskInputContext(ctx, a, Str(task, "id"), Str(input, "id"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := Object(ctxInfo, "manifest")
	if len(manifest["refs"].([]any)) == 0 {
		t.Fatal("missing refs", manifest)
	}
	quote, err := s.TaskInputContextPage(ctx, a, Str(task, "id"), Str(input, "id"), "quote_chain", "")
	if err != nil || !strings.Contains(Str(quote, "text"), "root question") {
		t.Fatalf("quote context missing: %v %v", quote, err)
	}
	recent, err := s.TaskInputContextPage(ctx, a, Str(task, "id"), Str(input, "id"), "recent_window", "")
	if err != nil || !strings.Contains(Str(recent, "text"), "follow up") || strings.Contains(Str(recent, "text"), "root question") {
		t.Fatalf("recent context wrong: %v %v", recent, err)
	}
	standingPage, err := s.TaskInputContextPage(ctx, a, Str(task, "id"), Str(input, "id"), "standing_context", "")
	if err != nil || !strings.Contains(Str(standingPage, "text"), "supported versions") {
		t.Fatalf("standing context missing: %v %v", standingPage, err)
	}
	coverage, err := s.TaskInputContextPage(ctx, a, Str(task, "id"), Str(input, "id"), "coverage", "")
	if err != nil || !strings.Contains(Str(coverage, "text"), "backfilled") {
		t.Fatalf("coverage missing: %v %v", coverage, err)
	}
	limited := Actor{ID: ID(), Role: "developer", RepositoryIDs: []string{"*"}}
	if _, err = s.TaskInputContext(ctx, limited, Str(task, "id"), Str(input, "id")); err == nil {
		t.Fatal("source context leaked without channel permission")
	} else {
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			t.Fatal("unexpected permission error", err)
		}
	}
}
