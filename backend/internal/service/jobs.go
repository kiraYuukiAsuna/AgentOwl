package service

import (
	"agentowl/backend/internal/taskcore"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

type leaseKey struct{}

func guardJob(ctx context.Context, q DB) error {
	j, ok := ctx.Value(leaseKey{}).(Data)
	if !ok {
		return nil
	}
	var valid bool
	e := q.QueryRow(ctx, "SELECT status='running' AND lease_token=$2 AND lease_until>now() FROM jobs WHERE id=$1 FOR UPDATE", Str(j, "id"), Str(j, "lease_token")).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return Fail(409, "lease_lost", "当前执行已失效，请刷新查看任务状态")
	}
	return ctx.Err()
}
func (s *Service) RunWorker(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.runLarkSockets(ctx) }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		timer := time.NewTicker(20 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if e := s.maintenance(ctx); e != nil {
					slog.Warn("recovery scan failed")
				}
				if e := s.CleanupOrphanArtifacts(ctx); e != nil {
					slog.Warn("artifact cleanup failed")
				}
				if e := s.MaintainSupport(ctx); e != nil {
					slog.Warn("support scan failed")
				}
				if e := s.scheduleLarkPolls(ctx); e != nil {
					slog.Warn("飞书定时同步扫描失败")
				}
				if e := s.schedulePRPolls(ctx); e != nil {
					slog.Warn("PR polling scan failed")
				}
				if e := s.scheduleGitHubPolls(ctx); e != nil {
					slog.Warn("GitHub polling scan failed")
				}
			}
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				worked, e := s.WorkOnce(ctx)
				if e != nil {
					slog.Warn("worker iteration failed")
				}
				if !worked {
					select {
					case <-ctx.Done():
						return
					case <-time.After(500 * time.Millisecond):
					}
				}
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}
func (s *Service) retrySettings(ctx context.Context, q DB) (Data, error) {
	settings, err := readExecutionSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	return Data{
		"job_attempts":           Num(settings, "job_retry_attempts"),
		"job_base_seconds":       Num(settings, "job_retry_base_seconds"),
		"job_max_seconds":        Num(settings, "job_retry_max_seconds"),
		"external_attempts":      Num(settings, "external_retry_attempts"),
		"external_delay_seconds": Num(settings, "external_retry_delay_seconds"),
	}, nil
}

func retryDelay(attempts, baseSeconds, maxSeconds int64) time.Duration {
	if baseSeconds < 1 {
		baseSeconds = 1
	}
	if maxSeconds < baseSeconds {
		maxSeconds = baseSeconds
	}
	seconds := baseSeconds
	for i := int64(1); i < attempts && seconds < maxSeconds; i++ {
		seconds *= 2
		if seconds > maxSeconds {
			seconds = maxSeconds
		}
	}
	return time.Duration(seconds) * time.Second
}
func jobAttempts(ctx context.Context) int64 {
	if j, ok := ctx.Value(leaseKey{}).(Data); ok {
		return Num(j, "attempts")
	}
	return 1
}

func permanentJobError(e error) bool {
	var se *Error
	if errors.As(e, &se) && se.Status >= 400 && se.Status < 500 && se.Status != 429 {
		return true
	}
	var api *integration.APIError
	return errors.As(e, &api) && api.StatusCode >= 400 && api.StatusCode < 500 && api.StatusCode != 408 && api.StatusCode != 429 && !api.OutcomeUnknown
}

func (s *Service) shouldRetryJobError(ctx context.Context, q DB, e error) (bool, error) {
	if e == nil || permanentJobError(e) {
		return false, nil
	}
	settings, err := s.retrySettings(ctx, q)
	if err != nil {
		return false, err
	}
	return jobAttempts(ctx) < Num(settings, "job_attempts"), nil
}

func (s *Service) WorkOnce(ctx context.Context) (bool, error) {
	token := Token()
	j, e := one(ctx, s.DB, `WITH candidate AS (SELECT id FROM jobs WHERE available_at<=now() AND (status='pending' OR (status='running' AND lease_until<now())) ORDER BY CASE WHEN kind IN ('poll_run','stop_task') THEN 0 ELSE 1 END,available_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE jobs j SET status='running',lease_token=$1,lease_until=now()+interval '45 seconds',attempts=attempts+1,updated_at=now() FROM candidate c WHERE j.id=c.id RETURNING to_jsonb(j)`, token)
	if e == NotFound {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	j["lease_token"] = token
	jobctx, cancel := context.WithTimeout(context.WithValue(ctx, leaseKey{}, j), 5*time.Minute)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTicker(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-jobctx.Done():
				return
			case <-timer.C:
				tag, e := s.DB.Exec(jobctx, "UPDATE jobs SET lease_until=now()+interval '45 seconds' WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>now()", Str(j, "id"), token)
				if e != nil || tag.RowsAffected() != 1 {
					cancel()
					return
				}
			}
		}
	}()
	e = s.processJob(jobctx, Str(j, "kind"), Object(j, "payload"))
	cancel()
	<-done
	finishCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	if e == nil {
		_, e = s.DB.Exec(finishCtx, "UPDATE jobs SET status='succeeded',lease_until=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>now()", Str(j, "id"), token)
		return true, e
	}
	var deferred *Error
	if errors.As(e, &deferred) && deferred.Code == "source_pending" {
		_, err := s.DB.Exec(finishCtx, "UPDATE jobs SET status='pending',attempts=GREATEST(attempts-1,0),available_at=now()+interval '5 seconds',lease_until=NULL,last_error=$3 WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>now()", Str(j, "id"), token, deferred.Message)
		return true, err
	}
	permanent := permanentJobError(e)
	settings, settingsErr := s.retrySettings(finishCtx, s.DB)
	if settingsErr != nil {
		return true, settingsErr
	}
	attempts := Num(j, "attempts")
	if permanent || attempts >= Num(settings, "job_attempts") {
		return true, s.failJob(finishCtx, j, e)
	}
	delay := retryDelay(attempts, Num(settings, "job_base_seconds"), Num(settings, "job_max_seconds"))
	_, err := s.DB.Exec(finishCtx, "UPDATE jobs SET status='pending',available_at=$3,last_error=$4,lease_until=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>now()", Str(j, "id"), token, time.Now().Add(delay), safeError(e))
	return true, err
}
func safeError(e error) string {
	if e == nil {
		return ""
	}
	var se *Error
	if errors.As(e, &se) {
		return se.Code + ": " + se.Message
	}
	var upstream *integration.APIError
	if errors.As(e, &upstream) {
		return upstream.Diagnostic()
	}
	return "处理失败，请检查连接配置和运行记录"
}
func (s *Service) processJob(ctx context.Context, kind string, p Data) error {
	switch kind {
	case "prepare_step":
		return s.prepareStep(ctx, p)
	case "execute_approval":
		return s.executeApproval(ctx, Str(p, "approval_id"))
	case "poll_run":
		return s.pollRun(ctx, Str(p, "run_id"))
	case "stop_task":
		return s.stopTask(ctx, Str(p, "task_id"))
	case "dispatch_external":
		return s.dispatchExternal(ctx, Str(p, "operation_id"))
	case "reconcile_external":
		return s.reconcileExternal(ctx, Str(p, "operation_id"))
	case "track_pr":
		return s.trackPR(ctx, Str(p, "task_id"))
	default:
		return s.ProcessSupportJob(ctx, kind, p)
	}
}
func (s *Service) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = guardJob(ctx, tx); e != nil {
		return e
	}
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Service) ReserveBudget(ctx context.Context, scope string, amount int64) (string, error) {
	id := ID()
	e := s.transaction(ctx, func(tx pgx.Tx) error {
		if amount < 1 {
			return Fail(422, "invalid_budget", "预算上限必须为正数")
		}
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(728690104)"); e != nil {
			return e
		}
		var used int64
		if e := tx.QueryRow(ctx, "SELECT COALESCE(sum(COALESCE(actual,amount)),0) FROM budget_reservations WHERE created_at>=date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'").Scan(&used); e != nil {
			return e
		}
		limit, err := s.modelDailyTokenLimit(ctx, tx)
		if err != nil {
			return err
		}
		if amount > limit-used {
			return Fail(429, "budget_exceeded", "今日模型预算已达到上限")
		}
		_, e := tx.Exec(ctx, "INSERT INTO budget_reservations(id,scope,amount) VALUES($1,$2,$3)", id, scope, amount)
		return e
	})
	return id, e
}
func (s *Service) SettleBudget(ctx context.Context, id string, actual int64) error {
	if actual < 0 {
		return nil
	}
	_, e := s.DB.Exec(ctx, "UPDATE budget_reservations SET actual=$2,status='settled' WHERE id=$1 AND status='reserved'", id, actual)
	return e
}

func (s *Service) RetryJob(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("admin") {
		return nil, Forbidden
	}
	j, e := locked(ctx, tx, "jobs", id)
	if e != nil {
		return nil, e
	}
	if Str(j, "status") != "failed" || Num(j, "attempts") != Num(b, "expected_attempts") {
		return nil, Conflict
	}
	p := Object(j, "payload")
	if Str(j, "kind") == "prepare_step" {
		t, e := locked(ctx, tx, "dev_tasks", Str(p, "task_id"))
		if e != nil {
			return nil, e
		}
		if Num(t, "epoch") != Num(p, "epoch") || Num(t, "revision") != Num(p, "revision") {
			return nil, Fail(409, "superseded_job", "已有更新的任务版本，请前往开发任务页面处理")
		}
		if contains([]string{"paused", "cancelled", "completed"}, Str(t, "status")) {
			return nil, Fail(409, "inactive_task", "请先在开发任务页面恢复任务")
		}
		if e = taskFact(ctx, tx, Str(t, "id"), taskcore.Retry, taskcore.Facts{}, Data{"error": ""}); e != nil {
			return nil, e
		}
	}
	if e = audit(ctx, tx, a, "retry", "jobs", id, Str(b, "reason")); e != nil {
		return nil, e
	}
	return one(ctx, tx, "UPDATE jobs SET status='pending',attempts=0,last_error='',ignored_at=NULL,ignored_by='',lease_token='',lease_until=NULL,available_at=now(),updated_at=now() WHERE id=$1 RETURNING to_jsonb(jobs)", id)
}

// Ignore changes only the acknowledgement state, not the execution result.
func (s *Service) IgnoreJob(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("admin") {
		return nil, Forbidden
	}
	ignored, ok := b["ignored"].(bool)
	if !ok {
		return nil, Fail(422, "validation", "请选择是否忽略")
	}
	if _, ok := b["expected_attempts"]; !ok {
		return nil, Fail(422, "validation", "缺少执行次数")
	}
	j, err := locked(ctx, tx, "jobs", id)
	if err != nil {
		return nil, err
	}
	if Str(j, "status") != "failed" || Num(j, "attempts") != Num(b, "expected_attempts") {
		return nil, Conflict
	}
	action := "unignore"
	if ignored {
		action = "ignore"
	}
	if err = audit(ctx, tx, a, action, "jobs", id, ""); err != nil {
		return nil, err
	}
	return one(ctx, tx, `UPDATE jobs SET ignored_at=CASE WHEN $2 THEN now() ELSE NULL END,ignored_by=CASE WHEN $2 THEN $3 ELSE '' END,updated_at=now() WHERE id=$1 RETURNING to_jsonb(jobs)`, id, ignored, a.ID)
}

func (s *Service) failJob(ctx context.Context, j Data, cause error) error {
	return s.transaction(context.WithValue(ctx, leaseKey{}, j), func(tx pgx.Tx) error {
		p := Object(j, "payload")
		reason := safeError(cause)
		if Str(j, "kind") == "poll_github" {
			if _, e := tx.Exec(ctx, "UPDATE configs SET data=data||$3::jsonb,updated_at=now() WHERE id=$1 AND kind='channels' AND version=$2", Str(p, "channel_id"), Num(p, "version"), JSON(Data{"poll_last_error": reason})); e != nil {
				return e
			}
		}
		if Str(j, "kind") == "prepare_step" {
			if e := taskFact(ctx, tx, Str(p, "task_id"), taskcore.Block, taskcore.Facts{Revision: Num(p, "revision"), Epoch: Num(p, "epoch")}, Data{"error": reason}); e != nil {
				return e
			}
		}
		if Str(j, "kind") == "execute_approval" {
			ap, e := locked(ctx, tx, "approvals", Str(p, "approval_id"))
			if e != nil {
				return e
			}
			if Str(ap, "status") == "approved" {
				if _, e = tx.Exec(ctx, "UPDATE approvals SET status='stale',reason=$2,version=version+1,updated_at=now() WHERE id=$1", Str(ap, "id"), reason); e != nil {
					return e
				}
				action := Object(ap, "action")
				if Str(ap, "subject_type") == "step" {
					if _, e = tx.Exec(ctx, "UPDATE task_steps SET status='blocked',error=$2,updated_at=now() WHERE id=$1 AND approval_id=$3", Str(ap, "subject_id"), reason, Str(ap, "id")); e != nil {
						return e
					}
					if e = taskFact(ctx, tx, Str(action, "task_id"), taskcore.Block, taskcore.Facts{Revision: Num(action, "revision"), Epoch: Num(action, "epoch")}, Data{"error": reason}); e != nil {
						return e
					}
				}
			}
		}
		_, e := tx.Exec(ctx, "UPDATE jobs SET status='failed',last_error=$3,lease_until=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>now()", Str(j, "id"), Str(j, "lease_token"), reason)
		return e
	})
}
