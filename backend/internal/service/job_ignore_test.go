package service

import (
	"context"
	"github.com/jackc/pgx/v5"
	"net/url"
	"testing"
)

func TestIgnoreJobPreservesFailureAndDashboard(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	id := ID()
	if _, err := s.DB.Exec(ctx, `INSERT INTO jobs(id,kind,dedupe_key,payload,status,attempts,last_error) VALUES($1::uuid,'sync_channel',$1::text,'{}','failed',5,'upstream error')`, id); err != nil {
		t.Fatal(err)
	}
	dashboard, err := s.Dashboard(ctx, a)
	if err != nil || Num(dashboard, "failed_jobs") != 1 {
		t.Fatalf("initial dashboard: %v %v", dashboard, err)
	}
	ignored := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.IgnoreJob(ctx, tx, a, id, Data{"expected_attempts": 5, "ignored": true})
	})
	if Str(ignored, "status") != "failed" || Str(ignored, "last_error") != "upstream error" || Str(ignored, "ignored_at") == "" || Str(ignored, "ignored_by") != a.ID {
		t.Fatalf("failure not retained: %v", ignored)
	}
	dashboard, err = s.Dashboard(ctx, a)
	if err != nil || Num(dashboard, "failed_jobs") != 0 {
		t.Fatalf("ignored dashboard: %v %v", dashboard, err)
	}
	unignored, err := s.List(ctx, a, "jobs", url.Values{"status": {"failed"}, "ignored": {"false"}})
	if err != nil || Num(unignored, "total") != 0 {
		t.Fatalf("unignored filter: %v %v", unignored, err)
	}
	listed, err := s.List(ctx, a, "jobs", url.Values{"status": {"failed"}, "ignored": {"true"}})
	if err != nil || Num(listed, "total") != 1 {
		t.Fatalf("ignored filter: %v %v", listed, err)
	}
	all, err := s.List(ctx, a, "jobs", url.Values{"status": {"failed"}})
	if err != nil || Num(all, "total") != 1 {
		t.Fatalf("existing filter: %v %v", all, err)
	}
	restored := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.IgnoreJob(ctx, tx, a, id, Data{"expected_attempts": 5, "ignored": false})
	})
	if Str(restored, "ignored_at") != "" {
		t.Fatal("not restored")
	}
	dashboard, err = s.Dashboard(ctx, a)
	if err != nil || Num(dashboard, "failed_jobs") != 1 {
		t.Fatalf("restored dashboard: %v %v", dashboard, err)
	}
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.IgnoreJob(ctx, tx, a, id, Data{"expected_attempts": 5, "ignored": true})
	})
	retried := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.RetryJob(ctx, tx, a, id, Data{"expected_attempts": 5}) })
	if Str(retried, "status") != "pending" || Str(retried, "ignored_at") != "" {
		t.Fatalf("retry retained ignore: %v", retried)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE jobs SET status='failed',attempts=1,last_error='new failure' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	dashboard, err = s.Dashboard(ctx, a)
	if err != nil || Num(dashboard, "failed_jobs") != 1 {
		t.Fatalf("new failure not counted: %v %v", dashboard, err)
	}
	_, _, err = s.Mutate(ctx, a, ID(), ID(), func(tx pgx.Tx) (Data, int, error) {
		d, e := s.IgnoreJob(ctx, tx, a, id, Data{"expected_attempts": 5, "ignored": true})
		return d, 200, e
	})
	if err != Conflict {
		t.Fatalf("stale change: %v", err)
	}
	viewer := Actor{ID: ID(), Role: "viewer"}
	_, _, err = s.Mutate(ctx, viewer, ID(), ID(), func(tx pgx.Tx) (Data, int, error) {
		d, e := s.IgnoreJob(ctx, tx, viewer, id, Data{"expected_attempts": 1, "ignored": true})
		return d, 200, e
	})
	if err != Forbidden {
		t.Fatalf("unauthorized ignore: %v", err)
	}
}
