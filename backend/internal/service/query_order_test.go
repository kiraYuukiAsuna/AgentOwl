package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCaseDetailOrdersRepliesOldestFirst(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	c := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveCase(ctx, tx, a, "", Data{"title": "Reply order"})
	})
	older, newer := ID(), ID()
	if _, err := s.DB.Exec(ctx, `INSERT INTO replies(id,case_id,body,status,case_version,created_at) VALUES
		($1,$3,'older','draft',$4,'2026-01-01T00:00:00Z'),
		($2,$3,'newer','draft',$4,'2026-01-01T00:01:00Z')`, older, newer, Str(c, "id"), Num(c, "version")); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Detail(ctx, a, "cases", Str(c, "id"))
	if err != nil {
		t.Fatal(err)
	}
	replies, ok := detail["replies"].([]Data)
	if !ok || len(replies) != 2 {
		t.Fatalf("unexpected replies: %#v", detail["replies"])
	}
	if Str(replies[0], "body") != "older" || Str(replies[1], "body") != "newer" {
		t.Fatalf("replies not chronological: %#v", replies)
	}
}
