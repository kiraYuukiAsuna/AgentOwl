package service

import (
	"context"
	"github.com/jackc/pgx/v5"
	"testing"
)

func TestPRTrackingKeyIgnoresStatusTimestamp(t *testing.T) {
	a := Data{"state": "open", "head_sha": "abc", "ci": Data{"state": "pending", "statuses": []Data{{"context": "build", "state": "pending", "updated_at": "one"}}}}
	b := decodeData(a)
	Object(b, "ci")["statuses"] = []Data{{"context": "build", "state": "pending", "updated_at": "two"}}
	if prTrackingKey(a) != prTrackingKey(b) {
		t.Fatal("timestamp-only change created an event")
	}
	Object(b, "ci")["state"] = "success"
	if prTrackingKey(a) == prTrackingKey(b) {
		t.Fatal("CI state change was ignored")
	}
}
func TestPRPollingScheduleDisabledAndRestart(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "repositories", "", Data{"name": "repo", "owner": "owner", "repo": "repo", "pr_poll_interval_minutes": 0})
	})
	c := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveCase(ctx, tx, a, "", Data{"title": "test"}) })
	task := ID()
	if _, err := s.DB.Exec(ctx, `INSERT INTO dev_tasks(id,case_id,canonical_case_id,repository_id,title,goal,acceptance_criteria,status,confirmed_by) VALUES($1,$2,$2,$3,'test','goal','criteria','pr_open',$4)`, task, Str(c, "id"), Str(repo, "id"), a.ID); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind='track_pr' AND payload->>'task_id'=$1`, task).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := s.schedulePRPolls(ctx); err != nil || count() != 0 {
		t.Fatalf("disabled: %v", err)
	}
	repo = mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(ctx, tx, a, "repositories", Str(repo, "id"), Data{"expected_version": Num(repo, "version"), "pr_poll_interval_minutes": 5})
	})
	if err := s.schedulePRPolls(ctx); err != nil || count() != 1 {
		t.Fatalf("enable: %v", err)
	}
	if err := s.schedulePRPolls(ctx); err != nil || count() != 1 {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE jobs SET status='succeeded',updated_at=now()-interval '6 minutes' WHERE kind='track_pr'`); err != nil {
		t.Fatal(err)
	}
	if err := s.schedulePRPolls(ctx); err != nil || count() != 2 {
		t.Fatalf("next interval: %v", err)
	}
}
