package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestLarkPollBatchAndRetry(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	var failThread atomic.Bool
	failThread.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "token", "expire": 7200})
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages" {
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		thread := r.URL.Query().Get("container_id_type") == "thread"
		if thread && failThread.Load() {
			w.WriteHeader(500)
			return
		}
		id := "om_root"
		threadID := "omt_test"
		if thread {
			id = "om_reply"
		} else if r.URL.Query().Get("page_token") != "" {
			id = "om_other"
			threadID = ""
		}
		item := Data{"message_id": id, "chat_id": "oc_test", "thread_id": threadID, "msg_type": "text", "create_time": "1760000000000", "body": Data{"content": `{"text":"auth help"}`}, "sender": Data{"id": "ou_user", "sender_type": "user"}}
		more := !thread && r.URL.Query().Get("page_token") == ""
		_ = json.NewEncoder(w).Encode(Data{"code": 0, "data": Data{"items": []Data{item}, "has_more": more, "page_token": "next"}})
	}))
	defer server.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "test", "base_url": server.URL}, Data{"app_secret": "secret"})
	channel := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", "", Data{"name": "feishu", "connector_id": connector, "external_id": "oc_test", "enabled": true, "reply_mode": "auto", "poll_interval_minutes": 1, "poll_batch_id": "forged"})
	})
	id := Str(channel, "id")
	if Str(channel, "poll_batch_id") != "" || Str(channel, "poll_cursor") != "" {
		t.Fatal("invalid initial batch", channel)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.scheduleLarkPolls(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := supportCount(t, s, `SELECT count(*) FROM jobs WHERE kind='sync_channel' AND payload->>'channel_id'=$1`, id); n != 1 {
		t.Fatalf("scheduled %d roots", n)
	}
	workSync := func() {
		t.Helper()
		_, err := s.DB.Exec(ctx, `UPDATE jobs SET available_at=now()+interval '1 day' WHERE kind<>'sync_channel'`)
		if err != nil {
			t.Fatal(err)
		}
		worked, err := s.WorkOnce(ctx)
		if err != nil || !worked {
			t.Fatalf("worker: %v %v", worked, err)
		}
	}
	workSync() // root creates page and thread
	if err := s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	ch, _, err := s.config(ctx, s.DB, id, "channels")
	if err != nil {
		t.Fatal(err)
	}
	if Str(ch, "poll_cursor") != "" {
		t.Fatal("cursor advanced before children finished")
	}
	// Make the thread fail terminally, then finish the remaining page.
	_, err = s.DB.Exec(ctx, `UPDATE jobs SET attempts=4 WHERE kind='sync_channel' AND payload->>'thread_id'='omt_test'`)
	if err != nil {
		t.Fatal(err)
	}
	workSync()
	workSync()
	if err = s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	ch, _, err = s.config(ctx, s.DB, id, "channels")
	if err != nil {
		t.Fatal(err)
	}
	if Str(ch, "poll_cursor") != "" || Str(ch, "poll_last_error") == "" || Str(ch, "poll_batch_id") != "" {
		t.Fatal("failure advanced or did not expose error", ch)
	}
	failThread.Store(false)
	_, err = s.DB.Exec(ctx, `UPDATE configs SET data=data||'{"poll_last_attempt_at":"2000-01-01T00:00:00Z"}'::jsonb WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	workSync()
	workSync()
	workSync()
	if err = s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	ch, _, err = s.config(ctx, s.DB, id, "channels")
	if err != nil {
		t.Fatal(err)
	}
	if Str(ch, "poll_cursor") == "" || Str(ch, "last_polled_at") == "" || Str(ch, "poll_last_error") != "" {
		t.Fatal("successful batch not finalized", ch)
	}
	if n := supportCount(t, s, `SELECT count(*) FROM ingest_events WHERE connector_id=$1`, connector); n != 3 {
		t.Fatalf("retries duplicated messages: %d", n)
	}
	events, err := many(ctx, s.DB, `SELECT to_jsonb(e) FROM ingest_events e WHERE connector_id=$1`, connector)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if err = s.consumeSupportEvent(ctx, Str(ev, "id")); err != nil {
			t.Fatal(err)
		}
	}
	if n := supportCount(t, s, `SELECT count(*) FROM cases WHERE reply_mode='auto'`); n != 0 {
		t.Fatal("history scheduled automatic replies")
	}
}

func TestLarkPollManualBaseline(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "lark", "app_id": "test"}, nil)
	baseline := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	id := supportConfig(t, s, "channels", Data{"name": "test", "connector_id": connector, "external_id": "oc_test", "enabled": true, "reply_mode": "draft", "last_synced_at": baseline}, nil)
	ch := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", id, Data{"expected_version": 1, "poll_interval_minutes": 10})
	})
	if Str(ch, "poll_cursor") != baseline {
		t.Fatal("manual baseline not retained", ch)
	}
	if err := s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := one(ctx, s.DB, `SELECT to_jsonb(j) FROM jobs j WHERE kind='sync_channel' AND payload->>'channel_id'=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	start, err := time.Parse(time.RFC3339Nano, Str(Object(j, "payload"), "start_time"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := time.Parse(time.RFC3339Nano, baseline)
	if !start.Equal(b.Add(-5 * time.Minute)) {
		t.Fatalf("unexpected start %v", start)
	}
}

func TestLarkPollPostmanSimulation(t *testing.T) {
	base := os.Getenv("AGENTOWL_POSTMAN_LARK_URL")
	if base == "" {
		t.Skip("AGENTOWL_POSTMAN_LARK_URL required")
	}
	s, a := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "test", "base_url": base}, Data{"app_secret": "synthetic"})
	ch := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", "", Data{"name": "fixture", "connector_id": connector, "external_id": "oc_test", "enabled": true, "poll_interval_minutes": 1})
	})
	if err := s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	worked, err := s.WorkOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("worker: %v %v", worked, err)
	}
	if err = s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	current, _, err := s.config(ctx, s.DB, Str(ch, "id"), "channels")
	if err != nil {
		t.Fatal(err)
	}
	if Str(current, "poll_cursor") == "" || supportCount(t, s, `SELECT count(*) FROM ingest_events WHERE connector_id=$1`, connector) != 1 {
		t.Fatal("fixture was not synchronized", current)
	}
}

func TestLarkPollConfigurationChangeDoesNotAdvanceCursor(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "test"}, nil)
	ch := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", "", Data{"name": "test", "connector_id": connector, "external_id": "oc_test", "enabled": true, "poll_interval_minutes": 1})
	})
	if err := s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	id := Str(ch, "id")
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", id, Data{"expected_version": Num(ch, "version"), "poll_interval_minutes": 2})
	})
	if _, err := s.DB.Exec(ctx, `UPDATE jobs SET status='succeeded' WHERE kind='sync_channel' AND payload->>'channel_id'=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.scheduleLarkPolls(ctx); err != nil {
		t.Fatal(err)
	}
	current, _, err := s.config(ctx, s.DB, id, "channels")
	if err != nil {
		t.Fatal(err)
	}
	if Str(current, "poll_cursor") != "" || Str(current, "poll_last_error") == "" {
		t.Fatal("changed configuration advanced cursor", current)
	}
}

func TestLarkPollIncompleteManualBaseline(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "test"}, nil)
	id := supportConfig(t, s, "channels", Data{"name": "test", "connector_id": connector, "external_id": "oc_test", "enabled": true, "reply_mode": "draft", "last_synced_at": time.Now().UTC().Format(time.RFC3339Nano)}, nil)
	if err := enqueue(ctx, s.DB, "sync_channel", "old-history", Data{"channel_id": id}, 0); err != nil {
		t.Fatal(err)
	}
	ch := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", id, Data{"expected_version": 1, "poll_interval_minutes": 5})
	})
	if Str(ch, "poll_cursor") != "" {
		t.Fatal("unfinished manual sync was used as baseline", ch)
	}
}

func TestPlatformErrorShowsInJobDetail(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_ = json.NewEncoder(w).Encode(Data{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(Data{"code": 230002, "msg": "Bot/User can NOT be out of the chat.", "error": Data{"log_id": "fixture-log"}})
	}))
	defer server.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "test", "base_url": server.URL}, Data{"app_secret": "secret"})
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "oc_test", "enabled": true}, nil)
	if err := enqueue(ctx, s.DB, "sync_channel", "diagnostic-test", Data{"channel_id": channel, "version": 1}, 0); err != nil {
		t.Fatal(err)
	}
	worked, err := s.WorkOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("worker: %v %v", worked, err)
	}
	var status, last string
	if err = s.DB.QueryRow(ctx, `SELECT status,last_error FROM jobs WHERE dedupe_key='diagnostic-test'`).Scan(&status, &last); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HTTP 400", "230002", "Bot/User", "fixture-log"} {
		if !strings.Contains(last, want) {
			t.Fatalf("job error missing %s: %s", want, last)
		}
	}
	if status != "failed" {
		t.Fatalf("permanent upstream error should stop retries: %s", status)
	}
}

func TestPlatformErrorPostmanSimulation(t *testing.T) {
	base := os.Getenv("AGENTOWL_POSTMAN_LARK_ERROR_URL")
	if base == "" {
		t.Skip("AGENTOWL_POSTMAN_LARK_ERROR_URL required")
	}
	s, _ := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "feishu", "app_id": "test", "base_url": base}, Data{"app_secret": "synthetic"})
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "oc_test", "enabled": true}, nil)
	if err := enqueue(ctx, s.DB, "sync_channel", "error-simulation", Data{"channel_id": channel, "version": 1}, 0); err != nil {
		t.Fatal(err)
	}
	worked, err := s.WorkOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("worker: %v %v", worked, err)
	}
	var last string
	if err = s.DB.QueryRow(ctx, `SELECT last_error FROM jobs WHERE dedupe_key='error-simulation'`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HTTP 400", "230002", "Bot/User", "synthetic-log-id"} {
		if !strings.Contains(last, want) {
			t.Fatalf("missing %s: %s", want, last)
		}
	}
}
