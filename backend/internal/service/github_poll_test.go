package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestGitHubPollIntervalValidation(t *testing.T) {
	for _, tc := range []struct {
		value any
		valid bool
	}{{nil, true}, {0, true}, {1, true}, {1440, true}, {-1, false}, {1441, false}, {1.5, false}, {"5", false}, {true, false}} {
		_, err := channelPollInterval(Data{"poll_interval_minutes": tc.value})
		if (err == nil) != tc.valid {
			t.Errorf("value %v: %v", tc.value, err)
		}
	}
}

func TestGitHubPollSchedulingAndConfiguration(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	connector := supportConfig(t, s, "connectors", Data{"provider": "github", "owner": "owner", "repo": "repo"}, nil)
	channel := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", "", Data{"name": "poll", "connector_id": connector, "external_id": "owner/repo", "enabled": true, "poll_interval_minutes": 5, "poll_cursor": "forged"})
	})
	id := Str(channel, "id")
	if Num(channel, "poll_interval_minutes") != 5 || Str(channel, "poll_cursor") == "" || Str(channel, "poll_cursor") == "forged" {
		t.Fatalf("invalid initial poll state: %v", channel)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.scheduleGitHubPolls(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := supportCount(t, s, "SELECT count(*) FROM jobs WHERE kind='poll_github' AND payload->>'channel_id'=$1 AND status='pending'", id); n != 1 {
		t.Fatalf("scheduled %d jobs", n)
	}
	cursor := Str(channel, "poll_cursor")
	updated := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", id, Data{"expected_version": Num(channel, "version"), "poll_interval_minutes": 7, "poll_cursor": "forged-again"})
	})
	if Str(updated, "poll_cursor") != cursor {
		t.Fatal("editing interval changed cursor or allowed a forged cursor")
	}
	if err := s.pollGitHubChannel(ctx, Data{"channel_id": id, "version": Num(channel, "version")}); err != nil {
		t.Fatal("stale scheduled job did not stop", err)
	}
	disabled := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", id, Data{"expected_version": Num(updated, "version"), "poll_interval_minutes": 0})
	})
	if Num(disabled, "poll_interval_minutes") != 0 {
		t.Fatal("polling not disabled")
	}
}

func TestGitHubPollIncrementalAndFailure(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	started := now.Add(-30 * time.Minute)
	cursor := now.Add(-10 * time.Minute)
	var fail atomic.Bool
	var issueSince, commentSince atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/repos/owner/repo/issues":
			issueSince.Store(r.URL.Query().Get("since"))
			_ = json.NewEncoder(w).Encode([]Data{
				{"id": 11, "number": 1, "title": "old issue", "body": "old body", "user": Data{"id": 101, "type": "User"}, "created_at": now.Add(-time.Hour), "updated_at": now.Add(-time.Minute)},
				{"id": 12, "number": 2, "title": "new issue", "body": "new body", "user": Data{"id": 102, "type": "User"}, "created_at": now.Add(-2 * time.Minute), "updated_at": now.Add(-2 * time.Minute)},
				{"id": 13, "number": 3, "title": "PR", "user": Data{"id": 103, "type": "User"}, "created_at": now.Add(-time.Minute), "updated_at": now.Add(-time.Minute), "pull_request": Data{"html_url": "https://example.test/pull/3"}},
			})
		case "/repos/owner/repo/issues/1/comments":
			commentSince.Store(r.URL.Query().Get("since"))
			if fail.Load() {
				w.WriteHeader(500)
				return
			}
			_ = json.NewEncoder(w).Encode([]Data{
				{"id": 21, "body": "old comment", "user": Data{"id": 101, "type": "User"}, "created_at": now.Add(-time.Hour), "updated_at": now.Add(-time.Hour)},
				{"id": 22, "body": "new comment", "user": Data{"id": 101, "type": "User"}, "created_at": now.Add(-time.Minute), "updated_at": now.Add(-time.Minute)},
			})
		case "/repos/owner/repo/issues/2/comments":
			_ = json.NewEncoder(w).Encode([]Data{})
		default:
			t.Errorf("unexpected URL %s", r.URL.String())
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "github", "owner": "owner", "repo": "repo", "base_url": server.URL, "sender_id": "102"}, nil)
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "owner/repo", "enabled": true, "reply_mode": "auto", "poll_interval_minutes": 5, "poll_started_at": started.Format(time.RFC3339Nano), "poll_cursor": cursor.Format(time.RFC3339Nano), "poll_repository": "owner/repo"}, nil)
	p := Data{"channel_id": channel, "version": 1}
	if err := s.pollGitHubChannel(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := issueSince.Load(); got != cursor.Add(-5*time.Minute).Format(time.RFC3339) {
		t.Fatalf("wrong issue since %v", got)
	}
	if got := commentSince.Load(); got != cursor.Add(-5*time.Minute).Format(time.RFC3339) {
		t.Fatalf("wrong comment since %v", got)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events WHERE connector_id=$1", connector); n != 2 {
		t.Fatalf("expected new issue and new comment, got %d", n)
	}
	events, err := many(ctx, s.DB, "SELECT to_jsonb(i) FROM ingest_events i WHERE connector_id=$1 ORDER BY created_at,id", connector)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if err = s.consumeSupportEvent(ctx, Str(ev, "id")); err != nil {
			t.Fatal(err)
		}
	}
	if n := supportCount(t, s, "SELECT count(*) FROM source_messages WHERE connector_id=$1 AND external_id IN ('issue:12','comment:22')", connector); n != 2 {
		t.Fatalf("live events not ingested (including sender's own issue): %d", n)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM cases WHERE reply_mode='auto' AND environment->>'historical'='false'"); n != 2 {
		t.Fatalf("poll events were not treated as live: %d", n)
	}
	if err = s.pollGitHubChannel(ctx, p); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events WHERE connector_id=$1", connector); n != 2 {
		t.Fatalf("overlap duplicated events: %d", n)
	}
	var before string
	if err = s.DB.QueryRow(ctx, "SELECT data->>'poll_cursor' FROM configs WHERE id=$1", channel).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err = s.pollGitHubChannel(ctx, p); err == nil {
		t.Fatal("upstream failure was ignored")
	}
	var after string
	if err = s.DB.QueryRow(ctx, "SELECT data->>'poll_cursor' FROM configs WHERE id=$1", channel).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("failed poll advanced cursor")
	}
}

// The opt-in Postman simulation drives the actual GitHub client and poll path.
func TestGitHubPollPostmanSimulation(t *testing.T) {
	base := os.Getenv("AGENTOWL_POSTMAN_GITHUB_URL")
	if base == "" {
		t.Skip("AGENTOWL_POSTMAN_GITHUB_URL required")
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatal("simulation must use loopback")
	}
	s, _ := testService(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	connector := supportConfig(t, s, "connectors", Data{"provider": "github", "owner": "owner", "repo": "repo", "base_url": base}, nil)
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "owner/repo", "enabled": true, "poll_interval_minutes": 5, "poll_started_at": now.Add(-10 * time.Minute).Format(time.RFC3339), "poll_cursor": now.Add(-5 * time.Minute).Format(time.RFC3339), "poll_repository": "owner/repo"}, nil)
	if err := s.pollGitHubChannel(ctx, Data{"channel_id": channel, "version": 1}); err != nil {
		t.Fatal(err)
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events WHERE connector_id=$1", connector); n != 2 {
		t.Fatal(fmt.Sprintf("expected simulated issue and comment, got %d", n))
	}
}

func TestGitHubPollDiscardsResultsAfterChannelChange(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	requested := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/issues" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		close(requested)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	connector := supportConfig(t, s, "connectors", Data{"provider": "github", "owner": "owner", "repo": "repo", "base_url": server.URL}, nil)
	channel := supportConfig(t, s, "channels", Data{"name": "original", "connector_id": connector, "external_id": "owner/repo", "enabled": true, "reply_mode": "draft", "poll_interval_minutes": 5, "poll_started_at": started.Format(time.RFC3339), "poll_cursor": started.Format(time.RFC3339), "poll_repository": "owner/repo"}, nil)
	done := make(chan error, 1)
	go func() { done <- s.pollGitHubChannel(ctx, Data{"channel_id": channel, "version": 1}) }()
	<-requested
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "channels", channel, Data{"expected_version": 1, "name": "changed", "poll_interval_minutes": 0})
	})
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var cursor string
	if err := s.DB.QueryRow(ctx, "SELECT data->>'poll_cursor' FROM configs WHERE id=$1", channel).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != started.Format(time.RFC3339) {
		t.Fatal("disabled channel advanced an old poll")
	}
	if n := supportCount(t, s, "SELECT count(*) FROM ingest_events WHERE connector_id=$1", connector); n != 0 {
		t.Fatal("stale poll persisted events")
	}
}

func TestGitHubPollRejectsChangedRepository(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	connector := supportConfig(t, s, "connectors", Data{"provider": "github", "owner": "other", "repo": "repo"}, nil)
	channel := supportConfig(t, s, "channels", Data{"connector_id": connector, "external_id": "issue:1", "enabled": true, "poll_interval_minutes": 5, "poll_started_at": started, "poll_cursor": started, "poll_repository": "owner/repo"}, nil)
	err := s.pollGitHubChannel(ctx, Data{"channel_id": channel, "version": 1})
	var fault *Error
	if !errors.As(err, &fault) || fault.Code != "poll_repository_changed" {
		t.Fatalf("repository change not detected: %v", err)
	}
}
