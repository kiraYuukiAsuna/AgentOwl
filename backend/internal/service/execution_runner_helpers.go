package service

import (
	"context"
	"io"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

func (s *Service) runnerForRunID(ctx context.Context, runID string) (RunnerAPI, Data, error) {
	attempt, err := one(ctx, s.DB, "SELECT to_jsonb(r) FROM run_attempts r WHERE id=$1", runID)
	if err != nil {
		return nil, nil, err
	}
	return s.runnerForAttempt(ctx, attempt)
}

func (s *Service) acknowledgeRun(ctx context.Context, runID string) error {
	r, _, err := s.runnerForRunID(ctx, runID)
	if err != nil {
		return err
	}
	return r.Acknowledge(ctx, runID)
}

func (s *Service) inspectRun(ctx context.Context, runID string) (runner.Run, error) {
	r, _, err := s.runnerForRunID(ctx, runID)
	if err != nil {
		return runner.Run{}, err
	}
	return r.Inspect(ctx, runID)
}

func (s *Service) stopRun(ctx context.Context, runID string) (runner.Run, error) {
	r, _, err := s.runnerForRunID(ctx, runID)
	if err != nil {
		return runner.Run{}, err
	}
	return r.Stop(ctx, runID)
}

func (s *Service) collectRun(ctx context.Context, runID string) (context.Context, RunnerAPI, io.ReadCloser, error) {
	r, _, err := s.runnerForRunID(ctx, runID)
	if err != nil {
		return ctx, nil, nil, err
	}
	stream, err := r.Collect(ctx, runID)
	return ctx, r, stream, err
}

func runAttemptTerminal(r Data) bool {
	return contains([]string{"succeeded", "failed", "stopped", "blocked"}, Str(r, "status"))
}

func (s *Service) pollAttemptValidity(ctx context.Context, id string) (Data, bool, bool, error) {
	r := Data{}
	valid := false
	settled := false
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		if e := lockExecutionSettings(ctx, tx); e != nil {
			return e
		}
		current, e := locked(ctx, tx, "run_attempts", id)
		if e != nil {
			return e
		}
		r = current
		if runAttemptTerminal(r) {
			settled = true
			return nil
		}
		st, e := locked(ctx, tx, "task_steps", Str(r, "step_id"))
		if e != nil {
			return e
		}
		ap, e := locked(ctx, tx, "approvals", Str(st, "approval_id"))
		if e != nil {
			return e
		}
		if Str(st, "attempt_id") != id || Str(ap, "action_hash") != Str(r, "action_digest") {
			return nil
		}
		if e = s.validateApproval(ctx, tx, ap, true); e != nil {
			return nil
		}
		valid = true
		_, e = tx.Exec(ctx, "UPDATE run_attempts SET updated_at=now() WHERE id=$1 AND status IN ('queued','running','collecting','outcome_unknown')", id)
		return e
	})
	return r, valid, settled, err
}
