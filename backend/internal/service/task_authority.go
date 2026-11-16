package service

import (
	"agentowl/backend/internal/taskcore"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

func (s *Service) revalidateTaskAuthorities(ctx context.Context, tx pgx.Tx, changedID string) error {
	rows, err := many(ctx, tx, `SELECT to_jsonb(t) FROM dev_tasks t JOIN task_mandates m ON m.id=t.mandate_id WHERE t.status NOT IN ('completed','cancelled') AND m.status='active' AND (t.repository_id::text=$1 OR m.sponsor_principal_id::text=$1 OR m.policy_ref->>'id'=$1 OR EXISTS(SELECT 1 FROM task_threads l JOIN source_threads th ON th.id=l.thread_id WHERE l.task_id=t.id AND (th.connector_id::text=$1 OR th.channel_key=$1))) ORDER BY t.id`, changedID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := Str(row, "id")
		if err = lockTaskExecutionRows(ctx, tx, id); err != nil {
			return err
		}
		t, err := locked(ctx, tx, "dev_tasks", id)
		if err != nil {
			return err
		}
		if err = s.validateTaskMandate(ctx, tx, t); err == nil {
			continue
		}
		var failure *Error
		if !errors.As(err, &failure) {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE task_mandates SET status='revoked',revoked_at=now(),reason='authorization_changed' WHERE id=$1`, Str(t, "mandate_id")); err != nil {
			return err
		}
		if err = transitionTask(ctx, tx, t, taskcore.Revoke, taskcore.Facts{}, "system:authorization"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE dev_tasks SET hold_reason='authorization_changed',error='任务授权已变化，请重新确认' WHERE id=$1`, id); err != nil {
			return err
		}
		if err = s.invalidateTask(ctx, tx, id); err != nil {
			return err
		}
		if err = enqueue(ctx, tx, "stop_task", "authority-stop:"+ID(), Data{"task_id": id}, 0); err != nil {
			return err
		}
	}
	return nil
}
