package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Service) GetAdmissionSettings(ctx context.Context, a Actor) (Data, error) {
	if !a.Has("admin") {
		return nil, Forbidden
	}
	return nil, Fail(410, "policy_moved", "请在审批策略中配置创建开发任务")
}
func (s *Service) SaveAdmissionSettings(ctx context.Context, tx pgx.Tx, a Actor, b Data) (Data, error) {
	if !a.Has("admin") {
		return nil, Forbidden
	}
	return nil, Fail(410, "policy_moved", "请在审批策略中配置创建开发任务")
}
func (s *Service) validateAdmissionRule(ctx context.Context, q DB, d Data) error {
	if err := require(d, "repository_id", "sponsor_principal_id", "channel_id", "condition"); err != nil {
		return err
	}
	for _, k := range []string{"repository_id", "sponsor_principal_id", "channel_id"} {
		if !validID(Str(d, k)) {
			return Fail(422, "validation", "请选择有效的仓库、渠道和负责人")
		}
	}
	p, err := one(ctx, q, "SELECT to_jsonb(p) FROM principals p WHERE id=$1 AND active", Str(d, "sponsor_principal_id"))
	if err != nil {
		return err
	}
	a := actorFrom(p)
	if !a.Has("developer") || !a.Repository(Str(d, "repository_id")) || !a.Channel(Str(d, "channel_id")) {
		return Fail(422, "validation", "负责人需要渠道和仓库的开发权限")
	}
	repo, _, err := s.config(ctx, q, Str(d, "repository_id"), "repositories")
	if err != nil {
		return err
	}
	if !Bool(repo, "enabled") {
		return Fail(422, "validation", "仓库未启用")
	}
	ch, _, err := s.config(ctx, q, Str(d, "channel_id"), "channels")
	if err != nil {
		return err
	}
	if !Bool(ch, "enabled") {
		return Fail(422, "validation", "渠道未启用")
	}
	co, _, err := s.config(ctx, q, Str(ch, "connector_id"), "connectors")
	if err != nil {
		return err
	}
	if connectorDisabled(co) {
		return Fail(422, "validation", "平台账号已停用")
	}
	paths := Strings(d, "allowed_paths")
	if len(paths) == 0 {
		paths = Strings(repo, "allowed_paths")
		d["allowed_paths"] = paths
	}
	for _, p := range paths {
		if !pathAllowed(p, Strings(repo, "allowed_paths")) {
			return Fail(422, "validation", "规则路径超出仓库范围")
		}
	}
	senders := Strings(d, "sender_types")
	if len(senders) == 0 {
		senders = []string{"human"}
		d["sender_types"] = senders
	}
	for _, kind := range senders {
		if !contains([]string{"human", "bot"}, kind) {
			return Fail(422, "validation", "发起者类型无效")
		}
	}
	if contains(senders, "bot") && !Bool(d, "allow_bot") {
		return Fail(422, "validation", "请明确允许机器人发起任务")
	}
	if len(Str(d, "condition")) > 4000 {
		return Fail(422, "validation", "创建条件过长")
	}
	if Str(d, "followup_mode") == "" {
		d["followup_mode"] = "manual"
	}
	if !contains([]string{"manual", "within_scope"}, Str(d, "followup_mode")) {
		return Fail(422, "validation", "请选择补充信息处理方式")
	}
	return nil
}

var admissionSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"outcome":{"type":"string","enum":["accept","ignore","needs_review"]},"goal":{"type":"string","maxLength":4000},"acceptance_criteria":{"type":"string","maxLength":4000},"reason":{"type":"string","maxLength":2000},"evidence_ids":{"type":"array","items":{"type":"string"}},"rule_id":{"type":"string"}},"required":["outcome","goal","acceptance_criteria","reason","evidence_ids","rule_id"]}`)

func (s *Service) evaluateAdmission(ctx context.Context, caseID string, requestKeys ...string) error {
	requestKey := ""
	if len(requestKeys) > 0 {
		requestKey = requestKeys[0]
	}
	release, err := s.runtimeEvaluationLock(ctx, "admission:"+caseID)
	if err != nil {
		return err
	}
	defer release()
	c, err := one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", caseID)
	if err != nil {
		return err
	}
	if Str(c, "reply_mode") == "manual" || contains([]string{"closed", "resolved"}, Str(c, "status")) {
		return nil
	}
	rules, err := many(ctx, s.DB, `SELECT to_jsonb(c) FROM configs c WHERE kind='policies' AND data->>'kind'='create_task' AND data->>'mode'<>'deny' AND data->>'channel_id'=$1 AND COALESCE((data->>'enabled')::boolean,false) AND (COALESCE(data->>'expires_at','')='' OR (data->>'expires_at')::timestamptz>now()) ORDER BY id`, Str(c, "channel_id"))
	if err != nil || len(rules) == 0 {
		return err
	}
	var active bool
	if err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dev_tasks WHERE canonical_case_id=$1 AND status NOT IN ('cancelled','completed','failed'))`, Str(c, "canonical_case_id")).Scan(&active); err != nil {
		return err
	}
	if active {
		return nil
	}
	messages, err := many(ctx, s.DB, `SELECT to_jsonb(m) FROM source_messages m WHERE case_id=$1 AND NOT deleted ORDER BY created_at,id LIMIT 100`, caseID)
	if err != nil {
		return err
	}
	snapshot := Data{"case": c, "rules": rules, "messages": messages}
	digest := Hash(JSON(snapshot))
	var evaluation Data
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		version, e := s.ModelSettingsVersion(ctx)
		if e != nil {
			return e
		}
		if requestKey == "" {
			requestKey = Hash(JSON(rules)) + fmt.Sprint(version)
		}
		evaluation, e = one(ctx, tx, `INSERT INTO admission_evaluations(id,case_id,case_version,input_digest,policy_version,model_version,snapshot,request_key,deadline) VALUES($1,$2,$3,$4,$5,$6,$7,$8,now()+interval '3 minutes') ON CONFLICT(case_id,case_version,policy_version,request_key) DO UPDATE SET case_id=EXCLUDED.case_id RETURNING to_jsonb(admission_evaluations)`, ID(), caseID, Num(c, "version"), digest, 0, version, JSON(snapshot), requestKey)
		return e
	})
	if err != nil {
		return err
	}
	if Str(evaluation, "state") != "pending" {
		return nil
	}
	result, assessErr := s.assessRuntime(ctx, "admission:"+Str(evaluation, "id"), string(JSON(snapshot)), admissionSchema)
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if e := lockExecutionSettings(ctx, tx); e != nil {
			return e
		}
		current, e := locked(ctx, tx, "cases", caseID)
		if e != nil {
			return e
		}
		ev, e := locked(ctx, tx, "admission_evaluations", Str(evaluation, "id"))
		if e != nil {
			return e
		}
		if Str(ev, "state") != "pending" {
			return nil
		}
		fresh, e := many(ctx, tx, `SELECT to_jsonb(c) FROM configs c WHERE kind='policies' AND data->>'kind'='create_task' AND data->>'mode'<>'deny' AND data->>'channel_id'=$1 AND COALESCE((data->>'enabled')::boolean,false) AND (COALESCE(data->>'expires_at','')='' OR (data->>'expires_at')::timestamptz>now()) ORDER BY id`, Str(c, "channel_id"))
		if e != nil {
			return e
		}
		mv, e := s.ModelSettingsVersion(ctx)
		if e != nil {
			return e
		}
		if Num(current, "version") != Num(c, "version") || Hash(JSON(fresh)) != Hash(JSON(rules)) || mv != Num(evaluation, "model_version") {
			_, e = tx.Exec(ctx, "UPDATE admission_evaluations SET state='stale' WHERE id=$1", Str(ev, "id"))
			return e
		}
		outcome := Str(result, "outcome")
		if assessErr != nil {
			outcome = "unavailable"
			result = Data{"reason": safeError(assessErr)}
		}
		if outcome == "accept" && (strings.TrimSpace(Str(result, "goal")) == "" || strings.TrimSpace(Str(result, "acceptance_criteria")) == "") {
			outcome = "needs_review"
		}
		rule := Data{}
		for _, raw := range rules {
			candidate := configPublic(raw)
			if Str(candidate, "id") == Str(result, "rule_id") {
				rule = candidate
				break
			}
		}
		repoID := Str(rule, "repository_id")
		if outcome == "accept" {
			valid := len(Strings(result, "evidence_ids")) > 0 && Str(result, "rule_id") == Str(rule, "id")
			for _, ref := range Strings(result, "evidence_ids") {
				found := false
				for _, m := range messages {
					if Str(m, "id") == ref {
						found = true
					}
				}
				valid = valid && found
			}
			kind := "human"
			if Bool(Object(current, "environment"), "source_bot") {
				kind = "bot"
			}
			if !contains(Strings(rule, "sender_types"), kind) || (kind == "bot" && !Bool(rule, "allow_bot")) {
				valid = false
			}
			if !valid {
				outcome = "needs_review"
				result["reason"] = "来源或判断依据不符合任务创建策略"
			}
		}
		if outcome == "accept" {
			if e = s.validateAdmissionRule(ctx, tx, rule); e != nil {
				outcome = "needs_review"
				result["reason"] = safeError(e)
			} else {
				outcome = "would_accept"
			}
		}
		decisionID := ID()
		if _, e = tx.Exec(ctx, `INSERT INTO admission_decisions(id,evaluation_id,case_id,repository_id,outcome,reason_code,evidence,authority_basis) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, decisionID, Str(ev, "id"), caseID, nullIfBlank(repoID), outcome, boundedText(Str(result, "reason"), 2000), JSON(result), JSON(Data{"rule_id": Str(rule, "id"), "rule_version": policyVersion(rule)})); e != nil {
			return e
		}
		action := Data{"case_id": caseID, "case_version": Num(current, "version"), "channel_id": Str(current, "channel_id"), "repository_id": repoID, "rule_id": Str(rule, "id"), "rule_version": policyVersion(rule), "sponsor_principal_id": Str(rule, "sponsor_principal_id"), "goal": Str(result, "goal"), "acceptance_criteria": Str(result, "acceptance_criteria"), "followup_mode": Str(rule, "followup_mode"), "allowed_paths": Strings(rule, "allowed_paths"), "input_digest": digest, "evidence_ids": Strings(result, "evidence_ids"), "reason": Str(result, "reason")}
		// A new proposal supersedes unexecuted proposals for this case.
		if _, e = tx.Exec(ctx, `UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE kind='create_task' AND action->>'case_id'=$1 AND status IN ('pending','approved','blocked')`, caseID); e != nil {
			return e
		}
		if outcome == "would_accept" {
			if _, e = s.createApproval(ctx, tx, Actor{ID: "system:admission"}, "create_task", "admission", decisionID, "创建开发任务 · "+Str(current, "title"), action); e != nil {
				return e
			}
		} else if outcome != "ignore" {
			action["notification_policy_id"] = Str(rule, "id")
			ap, e := one(ctx, tx, `INSERT INTO approvals(id,kind,subject_id,subject_type,status,summary,action,action_hash,reason,expires_at) VALUES($1,'create_task',$2,'admission','blocked',$3,$4,$5,$6,now()+interval '24 hours') RETURNING to_jsonb(approvals)`, ID(), decisionID, "创建开发任务 · "+Str(current, "title"), JSON(action), Hash(JSON(action)), Str(result, "reason"))
			if e != nil {
				return e
			}
			if e = s.enqueueApprovalNotification(ctx, tx, ap, "blocked"); e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, "UPDATE admission_evaluations SET state='done' WHERE id=$1", Str(ev, "id"))
		return e
	})
}

func (s *Service) AdmissionDecisions(ctx context.Context, a Actor, f url.Values) (Data, error) {
	args := []any{}
	where := caseWhere("c", a, &args)
	if !a.Has("admin") {
		p := param(&args, a.RepositoryIDs)
		where += " AND (d.repository_id IS NULL OR d.repository_id::text=ANY(" + p + "::text[]) OR '*'=ANY(" + p + "::text[]))"
	}
	if id := f.Get("id"); id != "" {
		if !validID(id) {
			return nil, NotFound
		}
		where += " AND d.id=" + param(&args, id)
	}
	if cursor := f.Get("cursor"); cursor != "" {
		if !validID(cursor) {
			return nil, Fail(422, "validation", "游标无效")
		}
		where += " AND (d.created_at,d.id)<(SELECT created_at,id FROM admission_decisions WHERE id=" + param(&args, cursor) + ")"
	}
	items, err := many(ctx, s.DB, `SELECT to_jsonb(d)||jsonb_build_object('case_title',c.title) FROM admission_decisions d JOIN cases c ON c.id=d.case_id WHERE `+where+` ORDER BY d.created_at DESC,d.id DESC LIMIT 50`, args...)
	next := ""
	if len(items) == 50 {
		next = Str(items[49], "id")
	}
	return Data{"items": items, "next_cursor": next}, err
}
func (s *Service) AdmissionAction(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("developer") || !validID(id) {
		return nil, Forbidden
	}
	d, err := one(ctx, tx, "SELECT to_jsonb(d) FROM admission_decisions d WHERE id=$1", id)
	if err != nil {
		return nil, err
	}
	if repository := Str(d, "repository_id"); repository != "" && !a.Repository(repository) {
		return nil, NotFound
	}
	c, err := s.Get(ctx, tx, a, "cases", Str(d, "case_id"))
	if err != nil {
		return nil, err
	}
	if Str(b, "action") == "reevaluate" {
		err = enqueue(ctx, tx, "evaluate_admission", "admission-manual:"+ID(), Data{"case_id": Str(c, "id"), "request_key": ID()}, 0)
		return Data{"queued": true}, err
	}
	return nil, Fail(410, "approval_moved", "请在审批列表中处理任务创建")
}
