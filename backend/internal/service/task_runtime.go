package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/taskcore"
	"github.com/jackc/pgx/v5"
)

func taskEvent(ctx context.Context, q DB, id, kind, actor string, metadata Data) error {
	_, err := q.Exec(ctx, "INSERT INTO task_events(id,task_id,type,actor,metadata) VALUES($1,$2,$3,$4,$5)", ID(), id, kind, actor, JSON(metadata))
	return err
}
func taskState(t Data) taskcore.State {
	return taskcore.State{Status: Str(t, "status"), Stage: Str(t, "stage"), Revision: Num(t, "revision"), Epoch: Num(t, "epoch")}
}
func transitionTask(ctx context.Context, q DB, t Data, command taskcore.Command, facts taskcore.Facts, actor string) error {
	next, err := taskcore.Decide(taskState(t), command, facts)
	if err != nil {
		return Fail(409, "invalid_task_transition", err.Error())
	}
	tag, err := q.Exec(ctx, `UPDATE dev_tasks SET status=$2,stage=$3,revision=$4,epoch=$5,version=version+1,updated_at=now() WHERE id=$1 AND version=$6`, Str(t, "id"), next.Status, next.Stage, next.Revision, next.Epoch, Num(t, "version"))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return Conflict
	}
	return taskEvent(ctx, q, Str(t, "id"), string(command), actor, Data{"revision": next.Revision, "epoch": next.Epoch, "status": next.Status})
}

func taskFact(ctx context.Context, q DB, id string, command taskcore.Command, facts taskcore.Facts, patch Data) error {
	t, e := locked(ctx, q, "dev_tasks", id)
	if e != nil {
		return e
	}
	if facts.Revision > 0 && (facts.Revision != Num(t, "revision") || facts.Epoch != Num(t, "epoch")) {
		return nil
	}
	if contains([]string{"cancelled", "completed", "paused"}, Str(t, "status")) {
		return nil
	}
	if e = transitionTask(ctx, q, t, command, facts, "system:worker"); e != nil {
		return e
	}
	if len(patch) > 0 {
		assignments := []string{}
		args := []any{id}
		for _, key := range []string{"error", "base_commit", "head_commit", "pr_url"} {
			if value, ok := patch[key]; ok {
				args = append(args, value)
				assignments = append(assignments, key+"=$"+fmt.Sprint(len(args)))
			}
		}
		if len(assignments) > 0 {
			_, e = q.Exec(ctx, "UPDATE dev_tasks SET "+strings.Join(assignments, ",")+" WHERE id=$1", args...)
		}
	}
	return e
}

func (s *Service) newTaskMandate(ctx context.Context, tx pgx.Tx, t Data, a Actor, b Data) error {
	repo, _, err := s.config(ctx, tx, Str(t, "repository_id"), "repositories")
	if err != nil {
		return err
	}
	follow := fallback(Str(b, "followup_mode"), "manual")
	if !contains([]string{"manual", "within_scope"}, follow) {
		return Fail(422, "validation", "请选择补充信息处理方式")
	}
	paths := Strings(b, "allowed_paths")
	if len(paths) == 0 {
		paths = Strings(repo, "allowed_paths")
	}
	for _, p := range paths {
		if !pathAllowed(p, Strings(repo, "allowed_paths")) {
			return Fail(422, "input_scope_conflict", "任务路径超出仓库范围")
		}
	}
	sources, err := many(ctx, tx, `SELECT jsonb_build_object('id',thread_id) FROM task_threads WHERE task_id=$1 AND routing_active ORDER BY thread_id`, Str(t, "id"))
	if err != nil {
		return err
	}
	refs := []string{}
	for _, source := range sources {
		refs = append(refs, Str(source, "id"))
	}
	scope := Data{"permitted_source_refs": refs, "permitted_stages": []string{"plan", "edit", "test", "review", "publish_pr", "track"}, "repository_paths": Strings(repo, "allowed_paths"), "repository_id": Str(t, "repository_id"), "case_id": Str(t, "case_id"), "allowed_paths": paths, "goal": Str(t, "goal"), "acceptance_criteria": Str(t, "acceptance_criteria"), "followup_mode": follow}
	decisionID := ID()
	if _, err = tx.Exec(ctx, `INSERT INTO admission_decisions(id,case_id,repository_id,outcome,authority_basis,task_id) VALUES($1,$2,$3,'accepted',$4,$5)`, decisionID, Str(t, "case_id"), Str(t, "repository_id"), JSON(Data{"kind": "human_confirmation", "actor_id": a.ID}), Str(t, "id")); err != nil {
		return err
	}
	mid := ID()
	if _, err = tx.Exec(ctx, `UPDATE task_mandates SET status='superseded' WHERE task_id=$1 AND status='active'`, Str(t, "id")); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO task_mandates(id,task_id,revision,decision_id,issuer,sponsor_principal_id,scope,scope_digest) VALUES($1,$2,$3,$4,'human_confirmation',$5,$6,$7)`, mid, Str(t, "id"), Num(t, "revision"), decisionID, a.ID, JSON(scope), Hash(JSON(scope))); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE dev_tasks SET mandate_id=$2,hold_reason='' WHERE id=$1", Str(t, "id"), mid); err != nil {
		return err
	}
	t["mandate_id"] = mid
	return taskEvent(ctx, tx, Str(t, "id"), "authorized", a.ID, Data{"mandate_id": mid, "revision": Num(t, "revision")})
}

func (s *Service) validateTaskMandate(ctx context.Context, q DB, t Data) error {
	if Str(t, "mandate_id") == "" {
		return Fail(409, "mandate_invalid", "历史任务需要重新确认目标和范围")
	}
	m, err := one(ctx, q, "SELECT to_jsonb(m) FROM task_mandates m WHERE id=$1", Str(t, "mandate_id"))
	if err != nil {
		return err
	}
	if Str(m, "status") != "active" || Num(m, "revision") != Num(t, "revision") || Str(m, "scope_digest") != Hash(JSON(Object(m, "scope"))) {
		return Fail(409, "mandate_invalid", "任务授权已失效")
	}
	if Str(m, "issuer") == "legacy_confirmation" {
		return Fail(409, "mandate_invalid", "历史任务需要重新确认目标和范围")
	}
	p, err := one(ctx, q, "SELECT to_jsonb(p) FROM principals p WHERE id=$1 AND active", Str(m, "sponsor_principal_id"))
	if err != nil {
		return Fail(403, "mandate_invalid", "任务授权成员已停用")
	}
	a := actorFrom(p)
	if !a.Has("developer") || !a.Repository(Str(t, "repository_id")) {
		return Forbidden
	}

	args := []any{Str(t, "id")}
	visibleSource := caseWhere("c", a, &args)
	var inaccessible bool
	if err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_inputs i JOIN source_messages sm ON sm.id=i.source_message_id JOIN cases c ON c.id=sm.case_id JOIN configs co ON co.id=sm.connector_id LEFT JOIN configs ch ON ch.id::text=sm.channel_id WHERE i.task_id=$1 AND i.disposition NOT IN ('withdrawn','superseded','unrelated') AND (NOT (`+visibleSource+`) OR co.data->>'status'='disabled' OR (ch.id IS NOT NULL AND NOT COALESCE((ch.data->>'enabled')::boolean,false))))`, args...).Scan(&inaccessible); err != nil {
		return err
	}
	if inaccessible {
		return Fail(403, "mandate_invalid", "任务来源的访问权限已变化")
	}
	_, err = s.Get(ctx, q, a, "development-tasks", Str(t, "id"))
	if err != nil {
		return err
	}
	repo, _, err := s.config(ctx, q, Str(t, "repository_id"), "repositories")
	if err != nil {
		return err
	}
	if !Bool(repo, "enabled") || Hash(JSON(Strings(repo, "allowed_paths"))) != Hash(JSON(Strings(Object(m, "scope"), "repository_paths"))) {
		return Fail(409, "mandate_invalid", "仓库范围已变更，请重新确认任务")
	}
	if Str(m, "issuer") == "policy_auto" {
		ref := Object(m, "policy_ref")
		rule, _, e := s.config(ctx, q, Str(ref, "id"), "policies")
		if e != nil || Str(rule, "kind") != "create_task" || !Bool(rule, "enabled") || Str(rule, "mode") != "auto" || policyVersion(rule) != Num(ref, "version") || Str(rule, "sponsor_principal_id") != a.ID {
			return Fail(409, "mandate_invalid", "任务创建策略已失效")
		}
		if expiry := Str(rule, "expires_at"); expiry != "" {
			at, e := time.Parse(time.RFC3339, expiry)
			if e != nil || !at.After(time.Now()) {
				return Fail(409, "mandate_invalid", "任务创建策略已过期")
			}
		}

	}
	var invalid bool
	err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_revision_inputs r JOIN task_inputs i ON i.id=r.input_id LEFT JOIN source_messages m ON m.id=i.source_message_id WHERE r.task_id=$1 AND r.revision=$2 AND (i.disposition IN ('withdrawn','superseded') OR (i.source_message_id IS NOT NULL AND (m.deleted OR m.version<>i.source_version))))`, Str(t, "id"), Num(t, "revision")).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid {
		return Fail(409, "input_changed", "任务引用的信息已变更")
	}
	// A verified edit/recall must fence the old input before the asynchronous consumer catches up.
	err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_revision_inputs r JOIN task_inputs i ON i.id=r.input_id JOIN source_messages m ON m.id=i.source_message_id JOIN ingest_events e ON e.connector_id=m.connector_id AND e.status='pending' AND e.payload->'event'->>'message_id'=m.external_id AND e.payload->>'channel_id'=m.channel_id WHERE r.task_id=$1 AND r.revision=$2 AND (COALESCE((e.payload->'event'->>'deleted')::boolean,false) OR COALESCE(e.payload->'event'->>'source_revision','')<>m.source_revision))`, Str(t, "id"), Num(t, "revision")).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid {
		return Fail(409, "input_changed", "来源信息正在更新")
	}
	return nil
}

func (s *Service) taskInputGate(ctx context.Context, q DB, t Data) error {
	if err := s.validateTaskMandate(ctx, q, t); err != nil {
		return err
	}
	var pending bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_inputs WHERE task_id=$1 AND disposition IN ('pending','review','apply') AND NOT EXISTS(SELECT 1 FROM task_revision_inputs r WHERE r.task_id=$1 AND r.revision=$2 AND r.input_id=task_inputs.id)) OR EXISTS(SELECT 1 FROM task_threads l JOIN source_threads th ON th.id=l.thread_id WHERE l.task_id=$1 AND l.routing_active AND (l.routed_generation<th.generation OR l.resolved_generation<th.generation))`, Str(t, "id"), Num(t, "revision")).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return Fail(409, "source_pending", "有补充信息待处理")
	}
	return nil
}

func (s *Service) freezeTaskInputs(ctx context.Context, q DB, t, input Data) error {
	if err := s.taskInputGate(ctx, q, t); err != nil {
		return err
	}
	if Str(t, "mandate_id") == "" {
		return nil
	}
	m, err := one(ctx, q, "SELECT to_jsonb(m) FROM task_mandates m WHERE id=$1", Str(t, "mandate_id"))
	if err != nil {
		return err
	}
	rows, err := many(ctx, q, `SELECT to_jsonb(i) FROM task_revision_inputs r JOIN task_inputs i ON i.id=r.input_id WHERE r.task_id=$1 AND r.revision=$2 ORDER BY i.seq`, Str(t, "id"), Num(t, "revision"))
	if err != nil {
		return err
	}
	if len(rows) > 128 {
		return Fail(409, "input_limit", "当前任务最多采用 128 条信息，请将较早的信息改为仅供参考")
	}
	refs := []taskcore.InputRef{}
	items := []Data{}
	for _, i := range rows {
		refs = append(refs, taskcore.InputRef{ID: Str(i, "id"), Digest: Str(i, "digest")})
		items = append(items, Data{"id": Str(i, "id"), "digest": Str(i, "digest"), "body": Str(i, "body")})
	}
	input["allowed_paths"] = Strings(Object(m, "scope"), "allowed_paths")
	watermarks, e := many(ctx, q, `SELECT jsonb_build_object('thread_id',thread_id,'routed_generation',routed_generation,'resolved_generation',resolved_generation) FROM task_threads WHERE task_id=$1 AND routing_active ORDER BY thread_id`, Str(t, "id"))
	if e != nil {
		return e
	}
	input["source_watermarks"] = watermarks
	input["mandate_id"] = Str(m, "id")
	input["mandate_digest"] = Str(m, "scope_digest")
	input["input_set_digest"] = taskcore.InputDigest(refs)
	input["task_inputs"] = items
	return nil
}

func (s *Service) appendTaskInput(ctx context.Context, tx pgx.Tx, t Data, body, kind string, actor Data, source Data) (Data, error) {
	if Str(source, "connector_id") != "" {
		identity, err := sourceIdentity(ctx, tx, source)
		if err != nil {
			return nil, err
		}
		identity["id"] = Str(actor, "id")
		actor = identity
	}
	body = strings.TrimSpace(integration.Redact(body))
	if len(body) > 64000 {
		return nil, Fail(422, "validation", "补充信息不能超过 64000 字节")
	}
	if kind == "manual" && body == "" {
		return nil, Fail(422, "validation", "请填写补充信息")
	}
	if Str(source, "id") != "" {
		old, e := one(ctx, tx, "SELECT to_jsonb(i) FROM task_inputs i WHERE task_id=$1 AND source_message_id=$2 AND source_version=$3 AND kind=$4", Str(t, "id"), Str(source, "id"), Num(source, "version"), kind)
		if e == nil {
			return old, nil
		}
		if e != NotFound {
			return nil, e
		}
	}
	var seq int64
	if err := tx.QueryRow(ctx, "UPDATE dev_tasks SET next_input_seq=next_input_seq+1,version=version+1,updated_at=now() WHERE id=$1 RETURNING next_input_seq", Str(t, "id")).Scan(&seq); err != nil {
		return nil, err
	}
	id := ID()
	disposition := "pending"
	if Bool(source, "deleted") {
		disposition = "withdrawn"
	}
	_, err := tx.Exec(ctx, `INSERT INTO task_inputs(id,task_id,seq,source_message_id,source_version,kind,actor_ref,body,digest,disposition,thread_id,generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, Str(t, "id"), seq, nullIfBlank(Str(source, "id")), Num(source, "version"), kind, JSON(actor), body, Hash([]byte(body)), disposition, nullIfBlank(Str(source, "thread_ref")), Num(source, "generation"))
	if err != nil {
		return nil, err
	}
	if err = taskEvent(ctx, tx, Str(t, "id"), "input_received", Str(actor, "id"), Data{"input_id": id, "seq": seq}); err != nil {
		return nil, err
	}
	if err = enqueue(ctx, tx, "evaluate_task_inputs", "inputs:"+id, Data{"task_id": Str(t, "id")}, 0); err != nil {
		return nil, err
	}
	return one(ctx, tx, "SELECT to_jsonb(i) FROM task_inputs i WHERE id=$1", id)
}

func (s *Service) AddTaskInput(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("developer") {
		return nil, Forbidden
	}
	if err := lockExecutionSettings(ctx, tx); err != nil {
		return nil, err
	}
	t, err := s.Get(ctx, tx, a, "development-tasks", id)
	if err != nil {
		return nil, err
	}
	if err = checkVersion(t, b); err != nil {
		return nil, err
	}
	if Str(t, "status") == "cancelled" || Str(t, "status") == "completed" {
		return nil, Fail(409, "terminal_task", "请先重新确认开发任务")
	}
	return s.appendTaskInput(ctx, tx, t, Str(b, "body"), "manual", Data{"id": a.ID, "principal_id": a.ID, "name": a.Name}, Data{})
}

func (s *Service) TaskInputAction(ctx context.Context, tx pgx.Tx, a Actor, id, inputID string, b Data) (Data, error) {
	if !a.Has("developer") || !validID(inputID) {
		return nil, Forbidden
	}
	if err := lockExecutionSettings(ctx, tx); err != nil {
		return nil, err
	}
	t, err := s.Get(ctx, tx, a, "development-tasks", id)
	if err != nil {
		return nil, err
	}
	if err = checkVersion(t, b); err != nil {
		return nil, err
	}
	input, err := one(ctx, tx, "SELECT to_jsonb(i) FROM task_inputs i WHERE id=$1 AND task_id=$2 FOR UPDATE", inputID, id)
	if err != nil {
		return nil, err
	}
	action := Str(b, "action")
	if !contains([]string{"context", "unrelated", "apply", "review"}, action) {
		return nil, Fail(422, "validation", "未知的补充信息操作")
	}
	if contains([]string{"withdrawn", "superseded"}, Str(input, "disposition")) {
		return nil, Fail(409, "input_changed", "这条信息已失效")
	}
	var frozen bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM task_revision_inputs WHERE task_id=$1 AND revision=$2 AND input_id=$3)", id, Num(t, "revision"), inputID).Scan(&frozen); err != nil {
		return nil, err
	}
	if frozen {
		return nil, Fail(409, "input_frozen", "已纳入当前任务，请通过修订调整")
	}
	if _, err = tx.Exec(ctx, "UPDATE task_inputs SET disposition=$2,reason_code='human' WHERE id=$1", inputID, action); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "UPDATE dev_tasks SET version=version+1,updated_at=now() WHERE id=$1", id); err != nil {
		return nil, err
	}
	if err = s.resolveTaskWatermarks(ctx, tx, id); err != nil {
		return nil, err
	}
	if err = taskEvent(ctx, tx, id, "input_decided", a.ID, Data{"input_id": inputID, "disposition": action}); err != nil {
		return nil, err
	}
	return one(ctx, tx, "SELECT to_jsonb(i) FROM task_inputs i WHERE id=$1", inputID)
}

func (s *Service) resolveTaskWatermarks(ctx context.Context, q DB, id string) error {
	_, err := q.Exec(ctx, `UPDATE task_threads l SET resolved_generation=LEAST(l.routed_generation,COALESCE((SELECT min(i.generation)-1 FROM task_inputs i WHERE i.task_id=l.task_id AND i.thread_id=l.thread_id AND i.disposition IN ('pending','review','apply') AND NOT EXISTS(SELECT 1 FROM task_revision_inputs r JOIN dev_tasks t ON t.id=r.task_id AND t.revision=r.revision WHERE r.input_id=i.id)),l.routed_generation)) WHERE l.task_id=$1`, id)
	return err
}

func (s *Service) initializeTaskInputs(ctx context.Context, tx pgx.Tx, t Data, a Actor) error {
	rows, err := many(ctx, tx, "SELECT to_jsonb(m) FROM source_messages m WHERE case_id=$1 AND NOT deleted ORDER BY created_at,id", Str(t, "case_id"))
	if err != nil {
		return err
	}
	c, err := one(ctx, tx, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(t, "case_id"))
	if err != nil {
		return err
	}
	for _, m := range rows {
		thread := fallback(Str(m, "thread_id"), Str(m, "external_id"))
		if Bool(Object(c, "environment"), "p2p_chat") {
			thread = "p2p:" + Str(m, "actor_id")
		}
		if _, err = tx.Exec(ctx, "UPDATE task_threads l SET routing_active=false FROM dev_tasks t WHERE l.task_id=t.id AND t.status IN ('completed','cancelled')"); err != nil {
			return err
		}
		th, e := one(ctx, tx, `INSERT INTO source_threads(id,connector_id,channel_key,external_thread_id) VALUES($1,$2,$3,$4) ON CONFLICT(connector_id,channel_key,external_thread_id) DO UPDATE SET channel_key=EXCLUDED.channel_key RETURNING to_jsonb(source_threads)`, ID(), Str(m, "connector_id"), Str(m, "channel_id"), thread)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO task_threads(task_id,thread_id,repository_id,relation,routed_generation,resolved_generation,attached_by) VALUES($1,$2,$3,'owner',$4,$4,$5) ON CONFLICT(task_id,thread_id) DO NOTHING`, Str(t, "id"), Str(th, "id"), Str(t, "repository_id"), Num(th, "processed_generation"), a.ID); e != nil {
			return e
		}
		m["thread_ref"] = Str(th, "id")
		m["generation"] = Num(th, "processed_generation")
		in, e := s.appendTaskInput(ctx, tx, t, Str(m, "content"), "source", Data{"id": Str(m, "actor_id")}, m)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE task_inputs SET disposition='apply',reason_code='initial_confirmation' WHERE id=$1", Str(in, "id")); e != nil {
			return e
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_revision_inputs(task_id,revision,input_id,content_digest,source_version) SELECT task_id,$2,id,digest,source_version FROM task_inputs WHERE task_id=$1 AND disposition='apply' ON CONFLICT DO NOTHING`, Str(t, "id"), Num(t, "revision"))
	return err
}

func (s *Service) bindRevisionInputs(ctx context.Context, tx pgx.Tx, t Data, b Data) error {
	id := Str(t, "id")
	revision := Num(t, "revision")
	_, err := tx.Exec(ctx, `INSERT INTO task_revision_inputs SELECT r.task_id,$2::bigint,r.input_id,r.content_digest,r.source_version FROM task_revision_inputs r JOIN task_inputs i ON i.id=r.input_id WHERE r.task_id=$1 AND r.revision=$2::bigint-1 AND NOT (r.input_id::text=ANY($3::text[])) AND i.disposition NOT IN ('withdrawn','superseded') ON CONFLICT DO NOTHING`, id, revision, Strings(b, "excluded_input_ids"))
	if err != nil {
		return err
	}
	for _, inputID := range Strings(b, "excluded_input_ids") {
		if !validID(inputID) {
			return Fail(422, "validation", "补充信息标识无效")
		}
		tag, e := tx.Exec(ctx, `UPDATE task_inputs SET disposition='context',reason_code='human_excluded' WHERE id=$1 AND task_id=$2 AND disposition NOT IN ('withdrawn','superseded')`, inputID, id)
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 0 {
			return NotFound
		}
	}
	for _, inputID := range Strings(b, "input_ids") {
		if !validID(inputID) {
			return Fail(422, "validation", "补充信息标识无效")
		}
		in, e := one(ctx, tx, "SELECT to_jsonb(i) FROM task_inputs i WHERE id=$1 AND task_id=$2 FOR UPDATE", inputID, id)
		if e != nil {
			return e
		}
		if contains([]string{"withdrawn", "superseded"}, Str(in, "disposition")) {
			return Fail(409, "input_changed", "补充信息已失效")
		}
		if _, e = tx.Exec(ctx, "UPDATE task_inputs SET disposition='apply',reason_code='revision' WHERE id=$1", inputID); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO task_revision_inputs(task_id,revision,input_id,content_digest,source_version) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, id, revision, inputID, Str(in, "digest"), Num(in, "source_version")); e != nil {
			return e
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE task_inputs i SET disposition='pending',reason_code='revision_changed' WHERE task_id=$1 AND disposition='apply' AND NOT EXISTS(SELECT 1 FROM task_revision_inputs r WHERE r.task_id=$1 AND r.revision=$2 AND r.input_id=i.id)`, id, revision); err != nil {
		return err
	}
	if err = enqueue(ctx, tx, "evaluate_task_inputs", fmt.Sprintf("revision-inputs:%s:%d", id, revision), Data{"task_id": id}, 0); err != nil {
		return err
	}
	return s.resolveTaskWatermarks(ctx, tx, id)
}

func (s *Service) TaskRuntimeDetail(ctx context.Context, a Actor, id string, cursors ...string) (Data, error) {
	t, err := s.Get(ctx, s.DB, a, "development-tasks", id)
	if err != nil {
		return nil, err
	}
	result := Data{"version": Num(t, "version"), "hold_reason": Str(t, "hold_reason")}
	args := []any{id}
	where := "i.task_id=$1"
	visible := caseWhere("c", a, &args)
	where += " AND (i.source_message_id IS NULL OR EXISTS(SELECT 1 FROM source_messages sm JOIN cases c ON c.id=sm.case_id WHERE sm.id=i.source_message_id AND " + visible + "))"
	countWhere, countArgs := where, append([]any{}, args...)
	if len(cursors) > 0 && cursors[0] != "" {
		cursor, e := strconv.ParseInt(cursors[0], 10, 64)
		if e != nil || cursor < 1 {
			return nil, Fail(422, "validation", "分页位置无效")
		}
		where += " AND i.seq<" + param(&args, cursor)
	}
	inputs, e := many(ctx, s.DB, `SELECT to_jsonb(i)||jsonb_build_object('applied_revision',(SELECT max(revision) FROM task_revision_inputs r WHERE r.input_id=i.id),'receipts',(SELECT COALESCE(jsonb_agg(to_jsonb(x)),'[]'::jsonb) FROM (SELECT run_id,phase,count(*) AS parts FROM run_input_receipts WHERE input_id=i.id GROUP BY run_id,phase) x)) FROM task_inputs i WHERE `+where+` ORDER BY seq DESC LIMIT 100`, args...)
	if e != nil {
		return nil, e
	}
	result["inputs"] = inputs
	if len(inputs) == 100 {
		result["next_cursor"] = fmt.Sprint(Num(inputs[99], "seq"))
	}
	var waiting int64
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM task_inputs i WHERE `+countWhere+` AND disposition IN ('pending','review','apply') AND NOT EXISTS(SELECT 1 FROM task_revision_inputs r JOIN dev_tasks t ON t.id=r.task_id AND t.revision=r.revision WHERE r.input_id=i.id)`, countArgs...).Scan(&waiting); err != nil {
		return nil, err
	}
	result["pending_inputs"] = waiting
	if waiting > 0 {
		result["hold_reason"] = "pending_inputs"
	}
	for key, query := range map[string]string{
		"threads": `SELECT to_jsonb(l)||jsonb_build_object('source',to_jsonb(th)) FROM task_threads l JOIN source_threads th ON th.id=l.thread_id WHERE task_id=$1 ORDER BY l.created_at LIMIT 100`,
		"events":  `SELECT to_jsonb(e) FROM task_events e WHERE task_id=$1 ORDER BY sequence DESC LIMIT 100`,
	} {
		queryArgs := []any{id}
		if key == "threads" {
			visible := caseWhere("c", a, &queryArgs)
			query = strings.Replace(query, "WHERE task_id=$1 ORDER BY", `WHERE task_id=$1 AND EXISTS(SELECT 1 FROM source_messages sm JOIN cases c ON c.id=sm.case_id WHERE sm.connector_id=th.connector_id AND sm.channel_id=th.channel_key AND (sm.thread_id=th.external_thread_id OR th.external_thread_id='p2p:'||sm.actor_id) AND `+visible+`) ORDER BY`, 1)
		}
		items, e := many(ctx, s.DB, query, queryArgs...)
		if e != nil {
			return nil, e
		}
		result[key] = items
	}
	if Str(t, "mandate_id") != "" {
		result["authorization"], err = one(ctx, s.DB, "SELECT to_jsonb(m) FROM task_mandates m WHERE id=$1", Str(t, "mandate_id"))
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
