package service

import (
	"agentowl/backend/internal/taskcore"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var stages = []string{"plan", "edit", "test", "review", "publish_pr", "track"}

func developmentApprovalKind(kind string) bool {
	return contains([]string{"plan", "edit", "test", "review", "publish_pr"}, kind)
}

func frozenApprovalMode(action Data) string {
	return Str(action, "approval_mode")
}

func (s *Service) evaluateApprovalMode(ctx context.Context, q DB, kind string, action Data) (string, Data, error) {
	if developmentApprovalKind(kind) {
		settings, err := readExecutionSettings(ctx, q)
		if err != nil {
			return "", nil, err
		}
		if Num(action, "execution_settings_version") != Num(settings, "version") || frozenApprovalMode(action) != "policy" {
			return "", nil, Fail(409, "execution_settings_changed", "任务执行设置已更新，请重新准备审批")
		}
	}
	return s.evaluatePolicy(ctx, q, kind, action)
}

func (s *Service) createApproval(ctx context.Context, tx pgx.Tx, a Actor, kind, subjectType, subjectID, summary string, action Data) (Data, error) {
	if developmentApprovalKind(kind) && subjectType != "step" {
		return nil, Forbidden
	}
	mode, policy, e := s.evaluateApprovalMode(ctx, tx, kind, action)
	if e != nil {
		return nil, e
	}
	id := ID()
	action["notification_policy_id"] = Str(policy, "id")
	digest := Hash(JSON(action))
	_, e = tx.Exec(ctx, "INSERT INTO approvals(id,kind,subject_id,subject_type,summary,action,action_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,now()+interval '24 hours')", id, kind, subjectID, subjectType, summary, JSON(action), digest)
	if e != nil {
		return nil, e
	}
	if mode == "auto" {
		actor := "system:policy:" + Str(policy, "id")
		reason := "自动审批规则匹配"
		if _, e = tx.Exec(ctx, "UPDATE approvals SET status='approved',actor_id=$2,policy_id=$3,policy_version=$4 WHERE id=$1", id, actor, Str(policy, "id"), policyVersion(policy)); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO approval_decisions(id,approval_id,decision,actor_id,reason,policy_id,evidence_hash,policy_version) VALUES($1,$2,'approve',$3,$4,$5,$6,$7)", ID(), id, actor, reason, Str(policy, "id"), digest, policyVersion(policy)); e != nil {
			return nil, e
		}
		if e = audit(ctx, tx, Actor{ID: actor}, "auto_approve", "approvals", id, reason); e != nil {
			return nil, e
		}
		if e = enqueue(ctx, tx, "execute_approval", "approval:"+id, Data{"approval_id": id, "policy_version": policyVersion(policy), "execution_settings_version": Num(action, "execution_settings_version")}, 0); e != nil {
			return nil, e
		}
	}
	if mode == "deny" {
		if _, e = tx.Exec(ctx, "UPDATE approvals SET status='rejected',reason='当前策略禁止执行',policy_id=$2,policy_version=$3 WHERE id=$1", id, Str(policy, "id"), policyVersion(policy)); e != nil {
			return nil, e
		}
	}
	if e = audit(ctx, tx, a, "request_approval", "approvals", id, summary); e != nil {
		return nil, e
	}
	if mode == "manual" && Str(policy, "id") != "" {
		if _, e = tx.Exec(ctx, "UPDATE approvals SET policy_id=$2,policy_version=$3 WHERE id=$1", id, Str(policy, "id"), policyVersion(policy)); e != nil {
			return nil, e
		}
	}
	ap, e := one(ctx, tx, "SELECT to_jsonb(ap) FROM approvals ap WHERE id=$1", id)
	if e != nil {
		return nil, e
	}
	if e = s.enqueueApprovalNotification(ctx, tx, ap, map[string]string{"manual": "pending", "auto": "auto_approved", "deny": "denied"}[mode]); e != nil {
		return nil, e
	}
	return ap, nil
}
func (s *Service) evaluatePolicy(ctx context.Context, q DB, kind string, action Data) (string, Data, error) {
	raw, e := many(ctx, q, "SELECT to_jsonb(c) FROM configs c WHERE kind='policies' AND data->>'kind'=$1 AND data->>'enabled'='true' ORDER BY id", kind)
	if e != nil {
		return "", nil, e
	}
	matched := []Data{}
	for _, r := range raw {
		p := configPublic(r)
		if kind == "create_task" && (Str(p, "channel_id") != Str(action, "channel_id") || Str(p, "repository_id") != Str(action, "repository_id")) {
			continue
		}
		if kind == "reply_send" && !Bool(action, "private_channel") {
			if ids := Strings(p, "channel_ids"); len(ids) > 0 && !contains(ids, Str(action, "channel_id")) {
				continue
			}
		}
		if developmentApprovalKind(kind) {
			if ids := Strings(p, "repository_ids"); len(ids) > 0 && !contains(ids, Str(action, "repository_id")) {
				continue
			}
		}
		if x := Str(p, "expires_at"); x != "" {
			t, e := time.Parse(time.RFC3339, x)
			if e != nil || !t.After(time.Now()) {
				continue
			}
		}
		matched = append(matched, p)
		if Str(p, "mode") == "deny" {
			return "deny", p, nil
		}
	}
	for _, p := range matched {
		if developmentApprovalKind(kind) && Num(action, "max_tokens") == 0 && Num(p, "max_tokens") > 0 {
			return "manual", p, nil
		}
		if kind == "edit" && Num(p, "max_files") > 0 && (Num(Object(action, "limits"), "max_files") == 0 || Num(Object(action, "limits"), "max_files") > Num(p, "max_files")) {
			return "manual", p, nil
		}
		if Str(p, "mode") == "manual" {
			return "manual", p, nil
		}
	}
	if len(matched) == 0 {
		return "manual", Data{}, nil
	}
	if kind == "create_task" {
		for _, p := range matched {
			if Str(p, "id") == Str(action, "rule_id") && policyVersion(p) == Num(action, "rule_version") {
				return "auto", p, nil
			}
		}
		return "manual", Data{}, nil
	}
	var chosen Data
	for _, p := range matched {
		// Zero means unlimited for development; a finite policy cannot approve it.
		if developmentApprovalKind(kind) && Num(action, "timeout_seconds") <= 0 && Num(p, "max_seconds") > 0 {
			return "manual", p, nil
		}
		if Num(action, "file_count") > Num(p, "max_files") || Num(action, "max_tokens") > Num(p, "max_tokens") || Num(action, "timeout_seconds") > Num(p, "max_seconds") {
			return "manual", p, nil
		}
		if contains([]string{"review", "publish_pr"}, kind) && Bool(p, "require_tests") && !Bool(action, "tests_passed") {
			return "manual", p, nil
		}
		allowed := Strings(p, "allowed_paths")
		if kind == "edit" {
			for _, path := range Strings(action, "allowed_paths") {
				if len(allowed) == 0 || !pathAllowed(path, allowed) {
					return "manual", p, nil
				}
			}
		}
		for _, f := range Strings(action, "changed_paths") {
			if len(allowed) == 0 || !pathAllowed(f, allowed) {
				return "manual", p, nil
			}
		}
		if kind == "reply_send" {
			if Bool(action, "manual") || lenArray(action["evidence"]) == 0 || !Bool(action, "answer_verified") {
				return "manual", p, nil
			}
		}
		chosen = p
	}
	if chosen == nil {
		return "manual", Data{}, nil
	}
	if kind == "reply_send" && !Bool(action, "private_channel") {
		observed, err := s.SendCapabilityObserved(ctx, q, Str(action, "channel_id"))
		if err != nil {
			return "", nil, err
		}
		if !observed {
			return "manual", chosen, nil
		}
	}
	return "auto", chosen, nil
}
func lenArray(v any) int {
	switch x := v.(type) {
	case []any:
		return len(x)
	case []Data:
		return len(x)
	}
	return 0
}
func pathAllowed(name string, allowed []string) bool {
	if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	for _, p := range allowed {
		if p == "." || name == p || strings.HasPrefix(name, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

func (s *Service) ConfirmDevelopment(ctx context.Context, tx pgx.Tx, a Actor, cid string, b Data) (Data, error) {
	if err := lockExecutionSettings(ctx, tx); err != nil {
		return nil, err
	}
	if !a.Has("developer") {
		return nil, Forbidden
	}
	if e := require(b, "repository_id", "goal", "acceptance_criteria"); e != nil {
		return nil, e
	}
	if !a.Repository(Str(b, "repository_id")) {
		return nil, Forbidden
	}
	if _, e := s.Get(ctx, tx, a, "cases", cid); e != nil {
		return nil, e
	}
	c, e := locked(ctx, tx, "cases", cid)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(c, b); e != nil {
		return nil, e
	}
	mode, _, e := s.evaluatePolicy(ctx, tx, "create_task", Data{"channel_id": Str(c, "channel_id"), "repository_id": Str(b, "repository_id")})
	if e != nil {
		return nil, e
	}
	if mode == "deny" {
		return nil, Fail(403, "policy_denied", "当前策略禁止创建开发任务")
	}
	canonical := Str(c, "canonical_case_id")
	if _, e = locked(ctx, tx, "cases", canonical); e != nil {
		return nil, e
	}
	repo, _, e := s.config(ctx, tx, Str(b, "repository_id"), "repositories")
	if e != nil {
		return nil, e
	}
	if !Bool(repo, "enabled") {
		return nil, Fail(409, "repository_disabled", "仓库未启用")
	}
	existing, e := one(ctx, tx, "SELECT to_jsonb(t) FROM dev_tasks t WHERE canonical_case_id=$1 AND repository_id=$2 AND status NOT IN ('completed','cancelled','failed')", canonical, Str(b, "repository_id"))
	if e == nil {
		return existing, nil
	}
	if e != NotFound {
		return nil, e
	}
	id := ID()
	initial, e := taskcore.Decide(taskcore.State{}, taskcore.Confirm, taskcore.Facts{})
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, "INSERT INTO dev_tasks(id,case_id,canonical_case_id,repository_id,title,goal,acceptance_criteria,confirmed_by,status,stage,revision,epoch) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)", id, cid, canonical, Str(b, "repository_id"), Str(c, "title"), Str(b, "goal"), Str(b, "acceptance_criteria"), a.ID, initial.Status, initial.Stage, initial.Revision, initial.Epoch)
	if e != nil {
		return nil, e
	}
	task, e := locked(ctx, tx, "dev_tasks", id)
	if e != nil {
		return nil, e
	}
	if e = s.initializeTaskInputs(ctx, tx, task, a); e != nil {
		return nil, e
	}
	if e = s.newTaskMandate(ctx, tx, task, a, b); e != nil {
		return nil, e
	}
	task, e = locked(ctx, tx, "dev_tasks", id)
	if e != nil {
		return nil, e
	}
	if e = s.validateTaskMandate(ctx, tx, task); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO task_revisions(task_id,revision,snapshot,actor_id) VALUES($1,1,$2,$3)", id, JSON(task), a.ID); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "UPDATE cases SET status='waiting_fix',version=version+1,updated_at=now() WHERE id=$1", cid); e != nil {
		return nil, e
	}
	if e = enqueue(ctx, tx, "prepare_step", "prepare:"+id+":1:plan", Data{"task_id": id, "stage": "plan", "revision": 1, "epoch": 1}, 0); e != nil {
		return nil, e
	}
	if e = caseEvent(ctx, tx, cid, "development_confirmed", a.ID, Str(b, "goal"), Data{"task_id": id}); e != nil {
		return nil, e
	}
	if e = audit(ctx, tx, a, "confirm_development", "development-tasks", id, ""); e != nil {
		return nil, e
	}
	return task, nil
}
func (s *Service) ReviseTask(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("developer") {
		return nil, Forbidden
	}
	if e := lockExecutionSettings(ctx, tx); e != nil {
		return nil, e
	}
	if _, e := s.Get(ctx, tx, a, "development-tasks", id); e != nil {
		return nil, e
	}
	if e := lockTaskExecutionRows(ctx, tx, id); e != nil {
		return nil, e
	}
	t, e := locked(ctx, tx, "dev_tasks", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(t, b); e != nil {
		return nil, e
	}
	var unsettled int
	if e = tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM run_attempts WHERE task_id=$1 AND status IN ('queued','running','collecting','outcome_unknown'))+(SELECT count(*) FROM external_operations WHERE subject_id=$1::text AND kind='publish_pr' AND status IN ('dispatching','outcome_unknown'))", id).Scan(&unsettled); e != nil {
		return nil, e
	}
	if unsettled > 0 {
		return nil, Fail(409, "unsettled_execution", "执行或远端写入结果尚未确认，请先暂停并核对结果")
	}
	if Str(t, "status") == "running" {
		return nil, Fail(409, "running_task", "请先暂停执行并确认已停止")
	}
	if Str(t, "status") == "cancelled" {
		return nil, Fail(409, "cancelled_task", "已取消任务需要重新确认创建")
	}
	if e = require(b, "goal", "acceptance_criteria"); e != nil {
		return nil, e
	}
	if e = transitionTask(ctx, tx, t, taskcore.Revise, taskcore.Facts{Unsettled: unsettled > 0}, a.ID); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "UPDATE dev_tasks SET goal=$2,acceptance_criteria=$3,error='',confirmed_by=$4 WHERE id=$1", id, Str(b, "goal"), Str(b, "acceptance_criteria"), a.ID); e != nil {
		return nil, e
	}
	t, e = locked(ctx, tx, "dev_tasks", id)
	if e != nil {
		return nil, e
	}
	if e = s.newTaskMandate(ctx, tx, t, a, b); e != nil {
		return nil, e
	}
	if e = s.bindRevisionInputs(ctx, tx, t, b); e != nil {
		return nil, e
	}
	if e = s.validateTaskMandate(ctx, tx, t); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO task_revisions(task_id,revision,snapshot,actor_id) VALUES($1,$2,$3,$4)", id, Num(t, "revision"), JSON(t), a.ID); e != nil {
		return nil, e
	}
	if e = s.invalidateTask(ctx, tx, id); e != nil {
		return nil, e
	}
	if e = enqueue(ctx, tx, "prepare_step", fmt.Sprintf("prepare:%s:%d:plan", id, Num(t, "revision")), Data{"task_id": id, "stage": "plan", "revision": Num(t, "revision"), "epoch": Num(t, "epoch")}, 0); e != nil {
		return nil, e
	}
	return t, audit(ctx, tx, a, "revise", "development-tasks", id, "")
}
func (s *Service) invalidateTask(ctx context.Context, q DB, id string) error {
	if _, e := q.Exec(ctx, "UPDATE external_operations SET status='cancelled',error='任务版本已失效',version=version+1,updated_at=now() WHERE subject_id=$1 AND kind='publish_pr' AND status='pending'", id); e != nil {
		return e
	}
	_, e := q.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE subject_type='step' AND subject_id IN (SELECT id FROM task_steps WHERE task_id=$1) AND status IN ('pending','approved')", id)
	return e
}
func (s *Service) TaskAction(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("developer") {
		return nil, Forbidden
	}
	if e := lockExecutionSettings(ctx, tx); e != nil {
		return nil, e
	}
	if _, e := s.Get(ctx, tx, a, "development-tasks", id); e != nil {
		return nil, e
	}
	if e := lockTaskExecutionRows(ctx, tx, id); e != nil {
		return nil, e
	}
	t, e := locked(ctx, tx, "dev_tasks", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(t, b); e != nil {
		return nil, e
	}
	action := Str(b, "action")
	if contains([]string{"cancelled", "completed"}, Str(t, "status")) {
		return nil, Fail(409, "terminal_task", "任务已结束")
	}
	switch action {
	case "pause", "cancel", "takeover", "revoke":
		state := "paused"
		if action == "cancel" {
			state = "cancelled"
		}
		command := taskcore.Pause
		if state == "cancelled" {
			command = taskcore.Cancel
		}
		if action == "revoke" {
			command = taskcore.Revoke
			_, e = tx.Exec(ctx, "UPDATE task_mandates SET status='revoked',revoked_at=now(),reason=$2 WHERE id=$1", Str(t, "mandate_id"), Str(b, "reason"))
			if e != nil {
				return nil, e
			}
		}
		if e = transitionTask(ctx, tx, t, command, taskcore.Facts{}, a.ID); e != nil {
			return nil, e
		}
		if e = s.invalidateTask(ctx, tx, id); e != nil {
			return nil, e
		}
		if e = enqueue(ctx, tx, "stop_task", fmt.Sprintf("stop:%s:%d", id, Num(t, "epoch")+1), Data{"task_id": id}, 0); e != nil {
			return nil, e
		}
	case "resume":
		if !contains([]string{"paused", "blocked", "failed"}, Str(t, "status")) {
			return nil, Fail(409, "invalid_state", "当前状态不能恢复")
		}
		var active int
		if e = tx.QueryRow(ctx, "SELECT count(*) FROM run_attempts WHERE task_id=$1 AND status IN ('queued','running','collecting','outcome_unknown')", id).Scan(&active); e != nil {
			return nil, e
		}
		if active > 0 {
			return nil, Fail(409, "run_unsettled", "上次执行尚未确认停止")
		}
		if e = s.taskInputGate(ctx, tx, t); e != nil {
			return nil, e
		}
		if e = transitionTask(ctx, tx, t, taskcore.Resume, taskcore.Facts{Unsettled: active > 0}, a.ID); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "DELETE FROM task_steps WHERE task_id=$1 AND revision=$2 AND stage=$3 AND NOT EXISTS(SELECT 1 FROM run_attempts r WHERE r.step_id=task_steps.id)", id, Num(t, "revision"), Str(t, "stage")); e != nil {
			return nil, e
		}
		if e = enqueue(ctx, tx, "prepare_step", fmt.Sprintf("resume:%s:%d", id, Num(t, "epoch")+1), Data{"task_id": id, "stage": Str(t, "stage"), "revision": Num(t, "revision"), "epoch": Num(t, "epoch") + 1}, 0); e != nil {
			return nil, e
		}
	default:
		return nil, Fail(422, "validation", "未知的任务操作")
	}
	if e = audit(ctx, tx, a, action, "development-tasks", id, Str(b, "reason")); e != nil {
		return nil, e
	}
	return s.Get(ctx, tx, a, "development-tasks", id)
}
func (s *Service) Decide(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	visible, e := s.Get(ctx, tx, a, "approvals", id)
	if e != nil {
		return nil, e
	}
	if Str(visible, "subject_type") == "admission" {
		if e = lockExecutionSettings(ctx, tx); e != nil {
			return nil, e
		}
	}
	if Str(visible, "subject_type") == "step" {
		if e = lockExecutionSettings(ctx, tx); e != nil {
			return nil, e
		}
		step, err := one(ctx, tx, "SELECT to_jsonb(st) FROM task_steps st WHERE id=$1", Str(visible, "subject_id"))
		if err != nil {
			return nil, err
		}
		if e = lockTaskExecutionRows(ctx, tx, Str(step, "task_id")); e != nil {
			return nil, e
		}
	}
	ap, e := locked(ctx, tx, "approvals", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(ap, b); e != nil {
		return nil, e
	}
	decision := Str(b, "decision")
	if !contains([]string{"approve", "reject", "revoke"}, decision) {
		return nil, Fail(422, "validation", "无效审批决定")
	}
	switch Str(ap, "subject_type") {
	case "knowledge":
		if !a.Has("knowledge") {
			return nil, Forbidden
		}
	case "reply":
		if !a.Has("oncall", "developer") {
			return nil, Forbidden
		}
	case "step", "admission":
		if !a.Has("developer") {
			return nil, Forbidden
		}
	default:
		return nil, Forbidden
	}
	if decision == "revoke" {
		if Str(ap, "status") != "approved" {
			return nil, Fail(409, "invalid_state", "只能撤销已批准动作")
		}
	} else if Str(ap, "status") != "pending" {
		return nil, Fail(409, "invalid_state", "审批已处理或失效")
	}
	if decision == "approve" {
		if e = s.validateApproval(ctx, tx, ap, false); e != nil {
			return nil, e
		}
		mode, _, e := s.evaluatePolicy(ctx, tx, Str(ap, "kind"), Object(ap, "action"))
		if e != nil {
			return nil, e
		}
		if mode == "deny" {
			return nil, Fail(403, "policy_denied", "当前策略禁止执行")
		}
	}
	status := map[string]string{"approve": "approved", "reject": "rejected", "revoke": "revoked"}[decision]
	if _, e = tx.Exec(ctx, "UPDATE approvals SET status=$2,actor_id=$3,reason=$4,policy_id='',policy_version=0,version=version+1,updated_at=now() WHERE id=$1", id, status, a.ID, Str(b, "reason")); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO approval_decisions(id,approval_id,decision,actor_id,reason,evidence_hash) VALUES($1,$2,$3,$4,$5,$6)", ID(), id, decision, a.ID, Str(b, "reason"), Str(ap, "action_hash")); e != nil {
		return nil, e
	}
	if decision == "approve" {
		if e = enqueue(ctx, tx, "execute_approval", "approval:"+id, Data{"approval_id": id}, 0); e != nil {
			return nil, e
		}
	} else if Str(ap, "subject_type") == "step" {
		st, e := locked(ctx, tx, "task_steps", Str(ap, "subject_id"))
		if e != nil {
			return nil, e
		}
		tid := Str(st, "task_id")
		if e = taskFact(ctx, tx, tid, taskcore.Revoke, taskcore.Facts{}, Data{"error": "审批" + status}); e != nil {
			return nil, e
		}
		if e = enqueue(ctx, tx, "stop_task", "revoke:"+id, Data{"task_id": tid}, 0); e != nil {
			return nil, e
		}
	}
	if e = audit(ctx, tx, a, decision, "approvals", id, Str(b, "reason")); e != nil {
		return nil, e
	}
	result, e := s.Get(ctx, tx, a, "approvals", id)
	if e != nil {
		return nil, e
	}
	if e = s.enqueueApprovalNotification(ctx, tx, result, status); e != nil {
		return nil, e
	}
	return result, nil
}

func (s *Service) validateApproval(ctx context.Context, q DB, ap Data, executing bool) error {
	expires, e := time.Parse(time.RFC3339Nano, Str(ap, "expires_at"))
	if e != nil || !expires.After(time.Now()) {
		return Fail(409, "approval_expired", "审批已过期")
	}
	if Hash(JSON(Object(ap, "action"))) != Str(ap, "action_hash") {
		return Fail(409, "evidence_changed", "批准证据不匹配")
	}
	if executing && Str(ap, "status") != "approved" {
		return Fail(409, "approval_invalid", "没有有效批准")
	}
	action := Object(ap, "action")
	// Already-started legacy attempts retain their frozen approval; new requests only use policies.
	executionAuto := false
	if executing && strings.HasPrefix(Str(ap, "actor_id"), "system:execution-settings:") {
		version := Num(action, "execution_settings_version")
		if Str(ap, "subject_type") != "step" || !developmentApprovalKind(Str(ap, "kind")) ||
			frozenApprovalMode(action) != "auto" || version < 1 || Str(ap, "policy_id") != "" ||
			Num(ap, "policy_version") != 0 || Str(ap, "actor_id") != fmt.Sprintf("system:execution-settings:%d", version) {
			return Forbidden
		}
		if e = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_decisions
			WHERE approval_id=$1 AND decision='approve' AND actor_id=$2 AND policy_id=''
			AND policy_version=0 AND evidence_hash=$3)`, Str(ap, "id"), Str(ap, "actor_id"), Str(ap, "action_hash")).Scan(&executionAuto); e != nil {
			return e
		}
		if !executionAuto {
			return Forbidden
		}
	}
	if executing {
		mode, policy, err := s.evaluatePolicy(ctx, q, Str(ap, "kind"), action)
		if err != nil {
			return err
		}
		if mode == "deny" {
			return Fail(403, "policy_denied", "当前策略禁止执行")
		}
		if executionAuto && mode != "auto" && Str(policy, "id") != "" {
			return Fail(409, "policy_changed", "当前策略要求人工审批")
		}
	}
	if executing && Str(ap, "policy_id") == "" && !executionAuto {
		var active bool
		e = q.QueryRow(ctx, "SELECT active FROM principals WHERE id::text=$1", Str(ap, "actor_id")).Scan(&active)
		if e != nil || !active {
			return Forbidden
		}
		u, e := one(ctx, q, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1", Str(ap, "actor_id"))
		if e != nil {
			return e
		}
		a := actorFrom(u)
		if contains([]string{"step", "admission"}, Str(ap, "subject_type")) && !a.Has("developer") || Str(ap, "subject_type") == "knowledge" && !a.Has("knowledge") || Str(ap, "subject_type") == "reply" && !a.Has("oncall", "developer") {
			return Forbidden
		}
	}
	if executing && Str(ap, "policy_id") != "" {
		var recorded bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_decisions WHERE approval_id=$1 AND decision='approve' AND actor_id=$2 AND policy_id=$3 AND policy_version=$4 AND evidence_hash=$5)`, Str(ap, "id"), Str(ap, "actor_id"), Str(ap, "policy_id"), Num(ap, "policy_version"), Str(ap, "action_hash")).Scan(&recorded); err != nil {
			return err
		}
		if !recorded {
			return Forbidden
		}
		if developmentApprovalKind(Str(ap, "kind")) && !contains([]string{"policy", "auto"}, frozenApprovalMode(action)) {
			return Fail(409, "policy_changed", "本次开发审批未使用策略模式")
		}
		p, _, e := s.config(ctx, q, Str(ap, "policy_id"), "policies")
		if e != nil || !Bool(p, "enabled") || Str(p, "mode") != "auto" || policyVersion(p) != Num(ap, "policy_version") || Str(ap, "actor_id") != "system:policy:"+Str(ap, "policy_id") {
			return Fail(409, "policy_changed", "审批策略已失效")
		}
		mode, _, e := s.evaluatePolicy(ctx, q, Str(ap, "kind"), action)
		if e != nil {
			return e
		}
		if mode != "auto" {
			return Fail(409, "policy_changed", "不再满足自动审批规则")
		}
	}
	switch Str(ap, "subject_type") {
	case "admission":
		return s.validateTaskCreationApproval(ctx, q, ap, executing)
	case "knowledge":
		k, e := one(ctx, q, "SELECT to_jsonb(k) FROM knowledge k WHERE id=$1", Str(ap, "subject_id"))
		if e != nil {
			return e
		}
		if Num(k, "version") != Num(action, "knowledge_version") || Str(k, "status") != "submitted" {
			return Conflict
		}
	case "reply":
		r, e := one(ctx, q, "SELECT to_jsonb(r) FROM replies r WHERE id=$1", Str(ap, "subject_id"))
		if e != nil {
			return e
		}
		c, e := one(ctx, q, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(r, "case_id"))
		if e != nil {
			return e
		}
		if executing && Str(ap, "policy_id") == "" {
			u, err := one(ctx, q, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1", Str(ap, "actor_id"))
			if err != nil || !s.canCase(actorFrom(u), c) {
				return Forbidden
			}
		}
		if Num(r, "version") != Num(action, "reply_version") || Num(c, "version") != Num(action, "case_version") {
			return Conflict
		}
		if Str(c, "reply_mode") == "manual" && !Bool(action, "manual") {
			return Fail(409, "human_takeover", "已由人工接管")
		}
		ev, e := s.evidence(ctx, q, c, Strings(r, "evidence_ids"))
		if e != nil {
			return e
		}
		if Hash(JSON(ev)) != Hash(JSON(action["evidence"])) {
			return Fail(409, "stale_evidence", "引用知识已变化")
		}
	case "step":
		st, e := one(ctx, q, "SELECT to_jsonb(st) FROM task_steps st WHERE id=$1", Str(ap, "subject_id"))
		if e != nil {
			return e
		}
		t, e := one(ctx, q, "SELECT to_jsonb(t) FROM dev_tasks t WHERE id=$1", Str(st, "task_id"))
		if e != nil {
			return e
		}
		if e = s.validateTaskMandate(ctx, q, t); e != nil {
			return e
		}
		if Hash(JSON(Object(st, "input"))) != Str(action, "input_hash") {
			return Conflict
		}
		if Str(ap, "kind") != Str(st, "stage") || Str(action, "stage") != Str(st, "stage") {
			return Conflict
		}
		checkSettings := Str(st, "attempt_id") == "" && developmentApprovalKind(Str(st, "stage"))
		if checkSettings && Str(st, "stage") == "publish_pr" {
			var started bool
			if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM external_operations WHERE approval_id=$1 AND kind='publish_pr' AND status NOT IN ('pending','cancelled'))", Str(ap, "id")).Scan(&started); err != nil {
				return err
			}
			checkSettings = !started
		}
		if checkSettings {
			settings, err := readExecutionSettings(ctx, q)
			if err != nil {
				return err
			}
			var modelVersion int64
			if err = q.QueryRow(ctx, "SELECT COALESCE((SELECT version FROM system_settings WHERE name='model'),0)").Scan(&modelVersion); err != nil {
				return err
			}
			if Num(action, "execution_settings_version") != Num(settings, "version") || Num(action, "model_settings_version") != modelVersion || Str(action, "resource_limits") != Str(settings, "resource_limits") || frozenApprovalMode(action) != Str(settings, "approval_mode") {
				return Fail(409, "execution_settings_changed", "模型或执行设置已更新，请重新准备并审批任务")
			}
		}
		if Num(t, "revision") != Num(action, "revision") || Num(t, "epoch") != Num(action, "epoch") || contains([]string{"paused", "cancelled", "completed", "blocked"}, Str(t, "status")) {
			return Conflict
		}
		repo, _, e := s.config(ctx, q, Str(t, "repository_id"), "repositories")
		if e != nil {
			return e
		}
		if !Bool(repo, "enabled") || Num(repo, "version") != Num(action, "repository_version") {
			return Fail(409, "repository_changed", "仓库执行配置已变更")
		}
		u, e := one(ctx, q, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1 AND active", Str(t, "confirmed_by"))
		if e != nil {
			return Forbidden
		}
		actor := actorFrom(u)
		if _, err := s.Get(ctx, q, actor, "cases", Str(t, "case_id")); err != nil {
			return Forbidden
		}
		if !actor.Has("developer") || !actor.Repository(Str(t, "repository_id")) {
			return Forbidden
		}
		if executing && Str(ap, "policy_id") == "" && !executionAuto {
			u, e = one(ctx, q, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1", Str(ap, "actor_id"))
			if e == nil {
				_, e = s.Get(ctx, q, actorFrom(u), "cases", Str(t, "case_id"))
			}
			if e != nil || !actorFrom(u).Repository(Str(t, "repository_id")) {
				return Forbidden
			}
		}
	default:
		return Forbidden
	}
	return nil
}

func decodeData(v any) Data { m := Data{}; _ = json.Unmarshal(JSON(v), &m); return m }
