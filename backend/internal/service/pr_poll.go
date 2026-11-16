package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

func prPollInterval(repo Data) int64 {
	if _, ok := repo["pr_poll_interval_minutes"]; !ok {
		return 5
	}
	return Num(repo, "pr_poll_interval_minutes")
}

// The worker scan can restart polling after it has been disabled.
func (s *Service) schedulePRPolls(ctx context.Context) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		var acquired bool
		if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(728690109)").Scan(&acquired); err != nil || !acquired {
			return err
		}
		rows, err := many(ctx, tx, `SELECT to_jsonb(t) || jsonb_build_object('poll_interval',COALESCE((c.data->>'pr_poll_interval_minutes')::int,5),'last_poll',(SELECT max(j.updated_at) FROM jobs j WHERE j.kind='track_pr' AND j.payload->>'task_id'=t.id::text)) FROM dev_tasks t JOIN configs c ON c.id=t.repository_id WHERE t.status='pr_open' AND c.kind='repositories' AND COALESCE((c.data->>'pr_poll_interval_minutes')::int,5)>0 ORDER BY t.updated_at LIMIT 1000 FOR UPDATE OF t`)
		if err != nil {
			return err
		}
		for _, t := range rows {
			last, _ := time.Parse(time.RFC3339Nano, Str(t, "last_poll"))
			if !last.IsZero() && time.Now().Before(last.Add(time.Duration(Num(t, "poll_interval"))*time.Minute)) {
				continue
			}
			var active bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE kind='track_pr' AND status IN ('pending','running') AND payload->>'task_id'=$1)`, Str(t, "id")).Scan(&active); err != nil {
				return err
			}
			if !active {
				if err = enqueue(ctx, tx, "track_pr", "track-pr-scan:"+ID(), Data{"task_id": Str(t, "id")}, 0); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Only changes meaningful to a reader create timeline events.
func prTrackingKey(t Data) string {
	ci := Object(t, "ci")
	statuses := []Data{}
	var rawStatuses []Data
	_ = json.Unmarshal(JSON(ci["statuses"]), &rawStatuses)
	for _, x := range rawStatuses {
		statuses = append(statuses, Data{"context": Str(x, "context"), "state": Str(x, "state"), "target_url": Str(x, "target_url")})
	}
	return Hash(JSON(Data{"head_sha": Str(t, "head_sha"), "state": Str(t, "state"), "merged": Bool(t, "merged"), "merge_commit_sha": Str(t, "merge_commit_sha"), "ci_state": Str(ci, "state"), "ci_sha": Str(ci, "sha"), "statuses": statuses, "checks": t["checks"], "checks_complete": Bool(t, "checks_complete"), "reviews": t["reviews"], "reviews_complete": Bool(t, "reviews_complete"), "ci_error": Str(t, "ci_error"), "checks_error": Str(t, "checks_error"), "reviews_error": Str(t, "reviews_error")}))
}
