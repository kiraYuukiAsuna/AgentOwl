package service

import (
	"context"
	"math"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

func validateSnapshotQuota(files []runner.File, limits runner.Limits) error {
	if limits.WorkspaceBytes < 0 || limits.MaxFiles < 0 || limits.MaxFiles > 0 && len(files) > limits.MaxFiles {
		return Fail(422, "snapshot_too_large", "代码快照超过任务配置的文件额度")
	}
	var total int64
	for _, file := range files {
		size := int64(len(file.Data))
		if size > math.MaxInt64-total || limits.WorkspaceBytes > 0 && size > limits.WorkspaceBytes-total {
			return Fail(422, "snapshot_too_large", "代码快照超过任务配置的文件额度")
		}
		total += size
	}
	return nil
}

// Unlimited task tokens still share the separately configured daily budget.
// Reserve the available balance and bind it to the run digest. Concurrent work
// cannot spend that reservation, and unknown outcomes retain it until settled.
func (s *Service) reserveRunTokenBudget(ctx context.Context, tx pgx.Tx, spec *runner.RunSpec) error {
	amount := int64(spec.Limits.MaxTokens)
	if amount < 0 {
		return Fail(422, "invalid_budget", "任务 Token 额度不能为负数")
	}
	if spec.Stage == "test" {
		amount = 0 // Tests do not invoke the model.
	} else {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690104)"); err != nil {
			return err
		}
		var used int64
		if err := tx.QueryRow(ctx, "SELECT COALESCE(sum(COALESCE(actual,amount)),0) FROM budget_reservations WHERE created_at>=date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'").Scan(&used); err != nil {
			return err
		}
		limit, err := s.modelDailyTokenLimit(ctx, tx)
		if err != nil {
			return err
		}
		remaining := limit - used
		if remaining <= 0 || amount > remaining {
			return Fail(429, "budget_exceeded", "今日模型预算已达到上限")
		}
		if amount == 0 {
			amount = remaining
			spec.DailyTokenBudget = remaining
		}
	}
	_, err := tx.Exec(ctx, "INSERT INTO budget_reservations(id,scope,amount) VALUES($1,$2,$3)", spec.RunID, "run:"+spec.RunID, amount)
	return err
}
