package service

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Recovery only inspects stable operation IDs; it never invents a replacement write.
func (s *Service) maintenance(ctx context.Context) error {
	if err := s.backfillTaskRuntime(ctx); err != nil {
		return err
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		var acquired bool
		if e := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(728690105)").Scan(&acquired); e != nil || !acquired {
			return e
		}
		runs, e := many(ctx, tx, "SELECT to_jsonb(r) FROM run_attempts r WHERE status IN ('queued','running','collecting','outcome_unknown') AND updated_at<now()-interval '15 seconds' ORDER BY updated_at LIMIT 100")
		if e != nil {
			return e
		}
		bucket := time.Now().Unix() / 20
		for _, r := range runs {
			if e = enqueue(ctx, tx, "poll_run", fmt.Sprintf("recover-run:%s:%d", Str(r, "id"), bucket), Data{"run_id": Str(r, "id")}, 0); e != nil {
				return e
			}
		}
		ops, e := many(ctx, tx, "SELECT to_jsonb(o) FROM external_operations o WHERE status IN ('pending','dispatching','outcome_unknown') AND updated_at<now()-interval '60 seconds' ORDER BY updated_at LIMIT 100")
		if e != nil {
			return e
		}
		for _, o := range ops {
			kind := "reconcile_external"
			if Str(o, "status") == "pending" {
				kind = "dispatch_external"
			}
			if e = enqueue(ctx, tx, kind, fmt.Sprintf("recover-op:%s:%d", Str(o, "id"), bucket), Data{"operation_id": Str(o, "id")}, 0); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "DELETE FROM sessions WHERE expires_at<now(); DELETE FROM login_transactions WHERE expires_at<now(); DELETE FROM lark_login_transactions WHERE expires_at<now()"); e != nil {
			return e
		}
		return nil
	})
}
