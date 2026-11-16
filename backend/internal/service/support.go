package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

func (s *Service) SaveCase(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("oncall", "developer") {
		return nil, Forbidden
	}
	create := id == ""
	m := Data{"status": "triage", "priority": "P2", "module": "general", "category": "question", "support_relevance": "uncertain", "reply_mode": "draft", "source": "manual", "environment": Data{}}
	if create {
		id = ID()
	} else {
		var e error
		m, e = s.Get(ctx, tx, a, "cases", id)
		if e != nil {
			return nil, e
		}
		m, e = locked(ctx, tx, "cases", id)
		if e != nil {
			return nil, e
		}
		if e = checkVersion(m, b); e != nil {
			return nil, e
		}
	}
	for _, k := range []string{"title", "description", "summary", "status", "priority", "module", "category", "support_relevance", "assignee_id", "channel_id", "environment"} {
		if v, ok := b[k]; ok {
			m[k] = v
		}
	}
	if e := require(m, "title"); e != nil {
		return nil, e
	}
	if len(Str(m, "title")) > 500 || len(Str(m, "description")) > 64000 {
		return nil, Fail(422, "validation", "问题内容过长")
	}
	if !contains([]string{"triage", "open", "awaiting_info", "in_progress", "waiting_user", "waiting_fix", "waiting_release", "awaiting_release", "resolved", "closed"}, Str(m, "status")) {
		return nil, Fail(422, "validation", "状态无效")
	}
	if !contains([]string{"P0", "P1", "P2", "P3"}, Str(m, "priority")) {
		return nil, Fail(422, "validation", "优先级无效")
	}
	if ch := Str(m, "channel_id"); ch != "" {
		if !a.Channel(ch) {
			return nil, Forbidden
		}
		if _, _, e := s.config(ctx, tx, ch, "channels"); e != nil {
			return nil, e
		}
	}
	if assignee := Str(m, "assignee_id"); assignee != "" {
		if !validID(assignee) {
			return nil, Fail(422, "validation", "负责人无效")
		}
		var n int
		if e := tx.QueryRow(ctx, "SELECT count(*) FROM principals WHERE id=$1 AND active", assignee).Scan(&n); e != nil {
			return nil, e
		}
		if n != 1 {
			return nil, Fail(422, "validation", "负责人未启用")
		}
	}
	var e error
	if create {
		_, e = tx.Exec(ctx, "INSERT INTO cases(id,canonical_case_id,title,description,summary,status,priority,module,category,support_relevance,assignee_id,channel_id,reply_mode,source,environment,created_by) VALUES($1,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)", id, Str(m, "title"), integration.Redact(Str(m, "description")), integration.Redact(Str(m, "summary")), Str(m, "status"), Str(m, "priority"), Str(m, "module"), Str(m, "category"), Str(m, "support_relevance"), Str(m, "assignee_id"), Str(m, "channel_id"), Str(m, "reply_mode"), Str(m, "source"), JSON(Object(m, "environment")), a.ID)
	} else {
		_, e = tx.Exec(ctx, "UPDATE cases SET title=$2,description=$3,summary=$4,status=$5,priority=$6,module=$7,category=$8,support_relevance=$9,assignee_id=$10,channel_id=$11,environment=$12,version=version+1,updated_at=now() WHERE id=$1", id, Str(m, "title"), integration.Redact(Str(m, "description")), integration.Redact(Str(m, "summary")), Str(m, "status"), Str(m, "priority"), Str(m, "module"), Str(m, "category"), Str(m, "support_relevance"), Str(m, "assignee_id"), Str(m, "channel_id"), JSON(Object(m, "environment")))
	}
	if e != nil {
		return nil, e
	}
	if e = caseEvent(ctx, tx, id, "updated", a.ID, "问题已更新", Data{}); e != nil {
		return nil, e
	}
	if e = audit(ctx, tx, a, "save", "cases", id, ""); e != nil {
		return nil, e
	}
	if create {
		if e = enqueue(ctx, tx, "classify", "classify:"+id+":1", Data{"case_id": id}, 0); e != nil {
			return nil, e
		}
	}
	return s.Get(ctx, tx, a, "cases", id)
}
func (s *Service) CaseAction(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("oncall", "developer") {
		return nil, Forbidden
	}
	if _, e := s.Get(ctx, tx, a, "cases", id); e != nil {
		return nil, e
	}
	c, e := locked(ctx, tx, "cases", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(c, b); e != nil {
		return nil, e
	}
	action := Str(b, "action")
	switch action {
	case "retry_classification":
		if Str(c, "status") == "closed" || Str(c, "status") == "resolved" {
			return nil, Fail(409, "case_closed", "请先重新打开问题，再重新分类")
		}
		if Str(c, "reply_mode") == "manual" {
			return nil, Fail(409, "manual_takeover", "问题已由人工接管，请先恢复自动处理")
		}
		var active bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE kind='classify' AND payload->>'case_id'=$1 AND status IN ('pending','running'))", id).Scan(&active); e != nil {
			return nil, e
		}
		if active {
			return nil, Fail(409, "classification_in_progress", "分类任务正在处理，请稍后查看")
		}
		var failed bool
		if e = tx.QueryRow(ctx, `SELECT COALESCE((SELECT type='needs_human' FROM case_events
			WHERE case_id=$1 AND (type='classified' OR (type='needs_human' AND (
			body LIKE '分类服务暂不可用，已转交人工判断%' OR body LIKE '模型配置暂不可用，已转交人工判断%' OR
			body LIKE '尚未配置模型，已转交人工判断%' OR body LIKE '模型预算不可用，已转交人工判断%')))
			ORDER BY created_at DESC, id DESC LIMIT 1), false)`, id).Scan(&failed); e != nil {
			return nil, e
		}
		if !failed {
			return nil, Fail(409, "classification_not_failed", "当前没有需要重试的分类失败记录")
		}
		if e = enqueue(ctx, tx, "classify", "classify-retry:"+ID(), Data{"case_id": id, "version": Num(c, "version"), "historical": true}, 0); e != nil {
			return nil, e
		}
	case "claim":
		if Str(c, "assignee_id") != "" && Str(c, "assignee_id") != a.ID {
			return nil, Fail(409, "already_claimed", "已被其他处理人认领")
		}
		_, e = tx.Exec(ctx, "UPDATE cases SET assignee_id=$2,status='in_progress',version=version+1,updated_at=now() WHERE id=$1", id, a.ID)
	case "takeover", "resume":
		mode := "manual"
		if action == "resume" {
			mode = "draft"
			if ch, _, ce := s.config(ctx, tx, Str(c, "channel_id"), "channels"); ce == nil {
				mode = fallback(Str(ch, "reply_mode"), "draft")
			}
		}
		_, e = tx.Exec(ctx, "UPDATE cases SET reply_mode=$2,version=version+1,updated_at=now() WHERE id=$1", id, mode)
		if e == nil {
			_, e = tx.Exec(ctx, "UPDATE replies SET status='stale',version=version+1,updated_at=now() WHERE case_id=$1 AND status IN ('generating','pending','approved')", id)
		}
		if e == nil {
			_, e = tx.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1 WHERE subject_type='reply' AND subject_id IN (SELECT id FROM replies WHERE case_id=$1) AND status IN ('pending','approved')", id)
		}
	case "close", "resolve", "reopen":
		status := "closed"
		if action == "resolve" {
			status = "resolved"
		}
		if action == "reopen" {
			status = "triage"
		}
		_, e = tx.Exec(ctx, "UPDATE cases SET status=$2,version=version+1,updated_at=now() WHERE id=$1", id, status)
	default:
		return nil, Fail(422, "validation", "未知的问题操作")
	}
	if e != nil {
		return nil, e
	}
	reason := Str(b, "reason")
	if action == "retry_classification" {
		reason = "已提交重新分类，结果会显示在这里"
	}
	if e = caseEvent(ctx, tx, id, action, a.ID, reason, Data{}); e != nil {
		return nil, e
	}
	if e = audit(ctx, tx, a, action, "cases", id, ""); e != nil {
		return nil, e
	}
	return s.Get(ctx, tx, a, "cases", id)
}
func (s *Service) Feedback(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("oncall", "developer") {
		return nil, Forbidden
	}
	if _, e := s.Get(ctx, tx, a, "cases", id); e != nil {
		return nil, e
	}
	c, e := locked(ctx, tx, "cases", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(c, b); e != nil {
		return nil, e
	}
	if latest, err := one(ctx, tx, "SELECT metadata FROM case_events WHERE case_id=$1 AND type='feedback' ORDER BY created_at DESC,id DESC LIMIT 1", id); err != nil && err != NotFound {
		return nil, err
	} else if err == nil && duplicateWebFeedback(c, latest, Str(b, "action")) {
		return nil, Fail(409, "duplicate_feedback", "相同反馈已记录，请刷新页面查看最新状态")
	}
	if e = s.applyFeedback(ctx, tx, c, a.ID, Str(b, "action"), Str(b, "reason"), Data{"source": "web"}); e != nil {
		return nil, e
	}
	return s.Get(ctx, tx, a, "cases", id)
}
func (s *Service) Relate(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if err := lockExecutionSettings(ctx, tx); err != nil {
		return nil, err
	}
	if !a.Has("oncall", "developer") {
		return nil, Forbidden
	}
	target := Str(b, "target_id")
	if id == target {
		return nil, Fail(422, "validation", "不能关联自身")
	}
	for _, cid := range []string{id, target} {
		if _, e := s.Get(ctx, tx, a, "cases", cid); e != nil {
			return nil, e
		}
	}
	if _, e := tx.Exec(ctx, "SELECT id FROM cases WHERE id IN ($1,$2) ORDER BY id FOR UPDATE", id, target); e != nil {
		return nil, e
	}
	c, e := locked(ctx, tx, "cases", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(c, b); e != nil {
		return nil, e
	}
	t, e := locked(ctx, tx, "cases", target)
	if e != nil {
		return nil, e
	}
	oldroot, newroot := Str(c, "canonical_case_id"), Str(t, "canonical_case_id")
	if oldroot == newroot {
		return c, nil
	}
	// Serialize group merges as well as development confirmation on the canonical root.
	if _, e = tx.Exec(ctx, "SELECT id FROM cases WHERE id IN ($1,$2) ORDER BY id FOR UPDATE", oldroot, newroot); e != nil {
		return nil, e
	}
	members, e := many(ctx, tx, "SELECT to_jsonb(c) FROM cases c WHERE canonical_case_id=$1", oldroot)
	if e != nil {
		return nil, e
	}
	for _, m := range members {
		if !s.canCase(a, m) {
			return nil, Forbidden
		}
	}
	var overlap bool
	e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dev_tasks a JOIN dev_tasks b ON a.repository_id=b.repository_id WHERE a.canonical_case_id=$1 AND b.canonical_case_id=$2 AND a.status NOT IN ('completed','cancelled','failed') AND b.status NOT IN ('completed','cancelled','failed'))", oldroot, newroot).Scan(&overlap)
	if e != nil {
		return nil, e
	}
	if overlap {
		return nil, Fail(409, "active_tasks_conflict", "两个问题组已有活动开发任务，请先人工整合")
	}
	if _, e = tx.Exec(ctx, "UPDATE cases SET canonical_case_id=$2,version=version+1,updated_at=now() WHERE canonical_case_id=$1", oldroot, newroot); e != nil {
		return nil, e
	}
	tasks, e := many(ctx, tx, "SELECT to_jsonb(t) FROM dev_tasks t WHERE canonical_case_id=$1 ORDER BY id FOR UPDATE", oldroot)
	if e != nil {
		return nil, e
	}
	for _, task := range tasks {
		if e = taskEvent(ctx, tx, Str(task, "id"), "case_rebound", a.ID, Data{"canonical_case_id": newroot}); e != nil {
			return nil, e
		}
	}
	if _, e = tx.Exec(ctx, "UPDATE dev_tasks SET canonical_case_id=$2,version=version+1,updated_at=now() WHERE canonical_case_id=$1", oldroot, newroot); e != nil {
		return nil, e
	}
	if e = caseEvent(ctx, tx, id, "related", a.ID, Str(b, "reason"), Data{"target_id": target}); e != nil {
		return nil, e
	}
	return s.Get(ctx, tx, a, "cases", id)
}
func (s *Service) CreateReply(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("oncall", "developer") {
		return nil, Forbidden
	}
	c, e := s.Get(ctx, tx, a, "cases", id)
	if e != nil {
		return nil, e
	}
	if n := Num(b, "expected_version"); n != 0 && n != Num(c, "version") {
		return nil, Conflict
	}
	body := integration.Redact(Str(b, "body"))
	if len(body) > 16000 {
		return nil, Fail(422, "validation", "回复过长")
	}
	status := "draft"
	if Bool(b, "generate") {
		model, err := s.currentModel(ctx, tx)
		if err != nil {
			return nil, err
		}
		if model == nil {
			return nil, Fail(503, "model_unconfigured", "尚未配置模型接口")
		}
		status = "generating"
	} else if strings.TrimSpace(body) == "" {
		return nil, Fail(422, "validation", "回复内容不能为空")
	}
	rid := ID()
	evidence, e := s.evidence(ctx, tx, c, Strings(b, "evidence_ids"))
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, "INSERT INTO replies(id,case_id,body,status,author_id,evidence_ids,evidence,case_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", rid, id, body, status, a.ID, Strings(b, "evidence_ids"), JSON(evidence), Num(c, "version"))
	if e != nil {
		return nil, e
	}
	if status == "generating" {
		if e = enqueue(ctx, tx, "answer", "answer:"+rid, Data{"reply_id": rid}, 0); e != nil {
			return nil, e
		}
	}
	return one(ctx, tx, "SELECT to_jsonb(r) FROM replies r WHERE id=$1", rid)
}
func (s *Service) evidence(ctx context.Context, q DB, c Data, ids []string) ([]Data, error) {
	out := []Data{}
	external := false
	if ch := Str(c, "channel_id"); ch != "" {
		v, _, e := s.config(ctx, q, ch, "channels")
		if e != nil {
			return nil, e
		}
		external = Str(v, "visibility") == "public" || Str(v, "visibility") == "external"
	}
	for _, id := range ids {
		if !validID(id) {
			return nil, Fail(422, "invalid_evidence", "知识引用无效")
		}
		k, e := one(ctx, q, "SELECT to_jsonb(k) FROM knowledge k WHERE id=$1 AND status='published' AND (valid_until IS NULL OR valid_until>now())", id)
		if e != nil {
			return nil, Fail(409, "stale_evidence", "知识已过期或不可引用")
		}
		if sourceID := Str(k, "source_id"); sourceID != "" {
			src, _, err := s.config(ctx, q, sourceID, "knowledge-sources")
			checked, parseErr := time.Parse(time.RFC3339Nano, Str(k, "permission_checked_at"))
			if err != nil || !contains([]string{"synced", "manual"}, Str(src, "status")) || parseErr != nil || time.Since(checked) > 24*time.Hour {
				return nil, Fail(409, "stale_source", "知识来源需要重新验证权限")
			}
			if err = s.sourceAudience(ctx, q, src, Str(c, "channel_id"), external); err != nil {
				return nil, err
			}
		}
		if external && Str(k, "visibility") != "public" {
			return nil, Forbidden
		}
		if Str(k, "visibility") == "channel" && !contains(Strings(k, "channel_ids"), Str(c, "channel_id")) {
			return nil, Forbidden
		}
		if e = knowledgeApplicability(k, c); e != nil {
			return nil, e
		}
		out = append(out, Data{"id": id, "version": Num(k, "version"), "source_revision": Str(k, "source_revision"), "title": Str(k, "title"), "body": Str(k, "body"), "source_url": Str(k, "source_url"), "cli_version": Str(k, "cli_version"), "environment": Str(k, "environment")})
	}
	return out, nil
}
func (s *Service) SubmitReply(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("oncall", "developer") {
		return nil, Forbidden
	}
	r, e := s.Get(ctx, tx, a, "replies", id)
	if e != nil {
		return nil, e
	}
	r, e = locked(ctx, tx, "replies", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(r, b); e != nil {
		return nil, e
	}
	if Str(r, "status") != "draft" {
		return nil, Fail(409, "invalid_state", "当前回复不能提交")
	}
	c, e := locked(ctx, tx, "cases", Str(r, "case_id"))
	if e != nil {
		return nil, e
	}
	if version := Num(b, "case_version"); version > 0 && version != Num(c, "version") {
		return nil, Conflict
	}
	if contains([]string{"closed", "resolved"}, Str(c, "status")) {
		return nil, Fail(409, "invalid_state", "问题已结束，不能发送回复")
	}
	ev, e := s.evidence(ctx, tx, c, Strings(r, "evidence_ids"))
	if e != nil {
		return nil, e
	}
	channel, connector, e := s.caseReplyChannel(ctx, tx, c)
	if e != nil {
		return nil, e
	}
	action := Data{"channel_version": Num(channel, "version"), "connector_id": Str(connector, "id"), "connector_version": Num(connector, "version"), "reply_id": id, "reply_version": Num(r, "version"), "case_id": Str(c, "id"), "case_version": Num(c, "version"), "channel_id": Str(channel, "id"), "body": Str(r, "body"), "evidence": ev, "manual": true}
	if Bool(channel, "private") {
		action["private_channel"], action["external_id"], action["private_actor_id"] = true, Str(channel, "external_id"), Str(channel, "private_actor_id")
	}
	ap, e := s.createApproval(ctx, tx, a, "reply_send", "reply", id, "发送回复："+Str(c, "title"), action)
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, "UPDATE replies SET status='pending',approval_id=$2,evidence=$3,case_version=$4,updated_at=now() WHERE id=$1", id, Str(ap, "id"), JSON(ev), Num(c, "version"))
	return ap, e
}

func (s *Service) SaveKnowledge(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("knowledge", "developer") {
		return nil, Forbidden
	}
	create := id == ""
	m := Data{"module": "general", "visibility": "internal", "channel_ids": []string{}, "body": ""}
	if create {
		id = ID()
	} else {
		var e error
		m, e = s.Get(ctx, tx, a, "knowledge", id)
		if e != nil {
			return nil, e
		}
		m, e = locked(ctx, tx, "knowledge", id)
		if e != nil {
			return nil, e
		}
		if e = checkVersion(m, b); e != nil {
			return nil, e
		}
	}
	for _, k := range []string{"title", "body", "module", "visibility", "channel_ids", "cli_version", "environment", "source_url", "source_revision", "source_id", "valid_until"} {
		if v, ok := b[k]; ok {
			m[k] = v
		}
	}
	if e := require(m, "title", "body"); e != nil {
		return nil, e
	}
	if len(Str(m, "body")) > 200000 {
		return nil, Fail(422, "validation", "知识正文过长")
	}
	if !contains([]string{"public", "internal", "channel"}, Str(m, "visibility")) {
		return nil, Fail(422, "validation", "可见范围无效")
	}
	if e := validateKnowledgeApplicability(m); e != nil {
		return nil, e
	}
	var until any
	if v := Str(m, "valid_until"); v != "" {
		t, e := time.Parse(time.RFC3339, v)
		if e != nil {
			return nil, Fail(422, "validation", "有效期无效")
		}
		until = t
	}
	body := integration.Redact(Str(m, "body"))
	var e error
	if create {
		_, e = tx.Exec(ctx, "INSERT INTO knowledge(id,title,body,module,visibility,channel_ids,cli_version,environment,source_url,source_revision,source_id,valid_until,author_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)", id, Str(m, "title"), body, Str(m, "module"), Str(m, "visibility"), Strings(m, "channel_ids"), Str(m, "cli_version"), Str(m, "environment"), Str(m, "source_url"), Str(m, "source_revision"), Str(m, "source_id"), until, a.ID)
	} else {
		_, e = tx.Exec(ctx, "UPDATE knowledge SET title=$2,body=$3,module=$4,visibility=$5,channel_ids=$6,cli_version=$7,environment=$8,source_url=$9,source_revision=$10,source_id=$11,valid_until=$12,status='draft',permission_checked_at=now(),version=version+1,updated_at=now() WHERE id=$1", id, Str(m, "title"), body, Str(m, "module"), Str(m, "visibility"), Strings(m, "channel_ids"), Str(m, "cli_version"), Str(m, "environment"), Str(m, "source_url"), Str(m, "source_revision"), Str(m, "source_id"), until)
	}
	if e != nil {
		return nil, e
	}
	if e = s.invalidateKnowledge(ctx, tx, id); e != nil {
		return nil, e
	}
	k, e := s.Get(ctx, tx, a, "knowledge", id)
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO knowledge_versions(knowledge_id,version,snapshot) VALUES($1,$2,$3)", id, Num(k, "version"), JSON(k)); e != nil {
		return nil, e
	}
	if e = audit(ctx, tx, a, "save", "knowledge", id, ""); e != nil {
		return nil, e
	}
	return k, nil
}
func (s *Service) invalidateKnowledge(ctx context.Context, q DB, id string) error {
	_, e := q.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE status IN ('pending','approved') AND ((subject_type='knowledge' AND subject_id=$1::uuid) OR (subject_type='reply' AND subject_id IN (SELECT id FROM replies WHERE $1::text=ANY(evidence_ids))))", id)
	return e
}
func (s *Service) KnowledgeAction(ctx context.Context, tx pgx.Tx, a Actor, id, action string, b Data) (Data, error) {
	if !a.Has("knowledge", "developer") {
		return nil, Forbidden
	}
	if _, e := s.Get(ctx, tx, a, "knowledge", id); e != nil {
		return nil, e
	}
	k, e := locked(ctx, tx, "knowledge", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(k, b); e != nil {
		return nil, e
	}
	if action == "withdraw" {
		if _, e = tx.Exec(ctx, "UPDATE knowledge SET status='withdrawn',version=version+1,updated_at=now() WHERE id=$1", id); e != nil {
			return nil, e
		}
		if e = s.invalidateKnowledge(ctx, tx, id); e != nil {
			return nil, e
		}
		if e = audit(ctx, tx, a, "withdraw", "knowledge", id, Str(b, "reason")); e != nil {
			return nil, e
		}
		return s.Get(ctx, tx, a, "knowledge", id)
	}
	if Str(k, "status") != "draft" {
		return nil, Fail(409, "invalid_state", "只有草稿可以提交审核")
	}
	ap, e := s.createApproval(ctx, tx, a, "knowledge_publish", "knowledge", id, "发布知识："+Str(k, "title"), Data{"knowledge_id": id, "knowledge_version": Num(k, "version"), "content_hash": Hash(JSON(k)), "visibility": Str(k, "visibility"), "body": Str(k, "body"), "title": Str(k, "title")})
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, "UPDATE knowledge SET status='submitted',updated_at=now() WHERE id=$1", id)
	return ap, e
}
func (s *Service) KnowledgeVersions(ctx context.Context, a Actor, id string) (Data, error) {
	if _, e := s.Get(ctx, s.DB, a, "knowledge", id); e != nil {
		return nil, e
	}
	v, e := many(ctx, s.DB, "SELECT snapshot FROM knowledge_versions WHERE knowledge_id=$1 ORDER BY version DESC LIMIT 100", id)
	if e != nil {
		return nil, e
	}
	visible := []Data{}
	for _, snapshot := range v {
		if s.canKnowledge(a, snapshot) {
			visible = append(visible, snapshot)
		}
	}
	return Data{"items": visible, "next_cursor": "", "total": len(visible)}, nil
}

func pretty(v any) string                  { return string(JSON(v)) }
func keyVersion(id string, v int64) string { return fmt.Sprintf("%s:%d", id, v) }
