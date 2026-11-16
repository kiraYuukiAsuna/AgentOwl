package service

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
)

func (s *Service) validateTaskCreationApproval(ctx context.Context, q DB, ap Data, executing bool) error {
	action := Object(ap, "action")
	if Str(ap, "kind") != "create_task" {
		return Forbidden
	}
	c, err := one(ctx, q, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(action, "case_id"))
	if err != nil {
		return err
	}
	if Num(c, "version") != Num(action, "case_version") || Str(c, "reply_mode") == "manual" || contains([]string{"closed", "resolved"}, Str(c, "status")) {
		return Fail(409, "proposal_stale", "问题内容已变化，请重新生成任务建议")
	}
	rule, _, err := s.config(ctx, q, Str(action, "rule_id"), "policies")
	if err != nil || Str(rule, "kind") != "create_task" || !Bool(rule, "enabled") || policyVersion(rule) != Num(action, "rule_version") {
		return Fail(409, "policy_changed", "任务创建策略已变化，请重新生成任务建议")
	}
	if expiry := Str(rule, "expires_at"); expiry != "" {
		at, e := time.Parse(time.RFC3339, expiry)
		if e != nil || !at.After(time.Now()) {
			return Fail(409, "policy_expired", "任务创建策略已过期")
		}
	}
	if err = s.validateAdmissionRule(ctx, q, rule); err != nil {
		return err
	}
	if Str(rule, "repository_id") != Str(action, "repository_id") || Str(rule, "channel_id") != Str(c, "channel_id") || Hash(JSON(Strings(rule, "allowed_paths"))) != Hash(JSON(Strings(action, "allowed_paths"))) {
		return Conflict
	}
	if err = require(action, "goal", "acceptance_criteria"); err != nil {
		return err
	}
	d, err := one(ctx, q, `SELECT to_jsonb(e) FROM admission_evaluations e JOIN admission_decisions d ON d.evaluation_id=e.id WHERE d.id=$1`, Str(ap, "subject_id"))
	if err != nil {
		return err
	}
	messages, err := many(ctx, q, `SELECT to_jsonb(m) FROM source_messages m WHERE case_id=$1 AND NOT deleted ORDER BY created_at,id LIMIT 100`, Str(c, "id"))
	if err != nil {
		return err
	}
	if Hash(JSON(messages)) != Hash(JSON(Object(d, "snapshot")["messages"])) {
		return Fail(409, "source_changed", "来源消息已变化，请重新生成任务建议")
	}
	if executing && Str(ap, "policy_id") == "" {
		p, err := one(ctx, q, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1 AND active", Str(ap, "actor_id"))
		if err != nil {
			return Forbidden
		}
		a := actorFrom(p)
		if !a.Has("developer") || !a.Repository(Str(action, "repository_id")) || !s.canCase(a, c) {
			return Forbidden
		}
	}
	return nil
}

func (s *Service) executeTaskCreationApproval(ctx context.Context, tx pgx.Tx, ap Data) error {
	action := Object(ap, "action")
	actorID := Str(ap, "actor_id")
	if Str(ap, "policy_id") != "" {
		actorID = Str(action, "sponsor_principal_id")
	}
	p, err := one(ctx, tx, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1 AND active", actorID)
	if err != nil {
		return err
	}
	body := Data{"expected_version": Num(action, "case_version"), "repository_id": Str(action, "repository_id"), "goal": Str(action, "goal"), "acceptance_criteria": Str(action, "acceptance_criteria"), "followup_mode": Str(action, "followup_mode"), "allowed_paths": Strings(action, "allowed_paths")}
	task, err := s.ConfirmDevelopment(ctx, tx, actorFrom(p), Str(action, "case_id"), body)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE admission_decisions SET task_id=$2,outcome='accepted' WHERE id=$1`, Str(ap, "subject_id"), Str(task, "id")); err != nil {
		return err
	}
	if Str(ap, "policy_id") != "" {
		// Never replace the authority of a task already created by a different request.
		if _, err = tx.Exec(ctx, `UPDATE task_mandates SET issuer='policy_auto',decision_id=$2,policy_ref=$3 WHERE id=$1 AND issuer='human_confirmation' AND decision_id IN(SELECT id FROM admission_decisions WHERE task_id=$4 AND evaluation_id IS NULL AND created_at>=transaction_timestamp())`, Str(task, "mandate_id"), Str(ap, "subject_id"), JSON(Data{"id": Str(action, "rule_id"), "version": Num(action, "rule_version")}), Str(task, "id")); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE approvals SET status='executed',version=version+1,updated_at=now() WHERE id=$1`, Str(ap, "id"))
	return err
}
