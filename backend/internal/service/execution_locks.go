package service

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Take these before execution row locks, in the same order as settings saves.
// Callers must commit before making external requests, except the existing
// admission absence check, which holds only its run row to fence new permits.
func lockExecutionSettings(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690107)"); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('system_settings:model',0))")
	return err
}

// Manual task changes can invalidate several steps. Lock their rows before the
// task so a concurrent run completion or admission absence check cannot wait in
// the opposite order. Call only after lockExecutionSettings.
func lockTaskExecutionRows(ctx context.Context, tx pgx.Tx, taskID string) error {
	active, err := tx.Exec(ctx, "SELECT id FROM run_attempts WHERE task_id=$1 AND status IN ('queued','running','collecting','outcome_unknown') ORDER BY id FOR UPDATE", taskID)
	if err != nil || active.RowsAffected() == 0 {
		// Completed attempts need no permit fence. Preserve the existing task
		// order for PR tracking, which only operates after execution finishes.
		return err
	}
	for _, query := range []string{
		"SELECT id FROM task_steps WHERE task_id=$1 ORDER BY id FOR UPDATE",
		"SELECT id FROM approvals WHERE subject_type='step' AND subject_id IN (SELECT id FROM task_steps WHERE task_id=$1) ORDER BY id FOR UPDATE",
	} {
		if _, err := tx.Exec(ctx, query, taskID); err != nil {
			return err
		}
	}
	return nil
}
