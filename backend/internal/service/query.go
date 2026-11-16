package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var tables = map[string]string{"cases": "cases", "knowledge": "knowledge", "principals": "principals", "approvals": "approvals", "development-tasks": "dev_tasks", "audit-events": "audit_events", "external-operations": "external_operations", "replies": "replies", "artifacts": "artifacts", "jobs": "jobs"}
var configKinds = map[string]bool{"admission-rules": true, "connectors": true, "channels": true, "repositories": true, "policies": true, "owner-routes": true, "identity-bindings": true, "knowledge-sources": true, "runtime-machines": true}

func param(args *[]any, v any) string {
	*args = append(*args, v)
	return "$" + strconv.Itoa(len(*args))
}
func caseWhere(alias string, a Actor, args *[]any) string {
	if a.Has("admin") {
		return "true"
	}
	id := param(args, a.ID)
	ch := param(args, append([]string{}, a.ChannelIDs...))
	return fmt.Sprintf("(%s.created_by=%s OR %s.assignee_id=%s OR %s.channel_id=ANY(%s::text[]) OR ('*'=ANY(%s::text[]) AND %s.channel_id<>''))", alias, id, alias, id, alias, ch, ch, alias)
}
func repoWhere(alias string, a Actor, args *[]any) string {
	if a.Has("admin") {
		return "true"
	}
	p := param(args, a.RepositoryIDs)
	return fmt.Sprintf("(%s.repository_id::text=ANY(%s::text[]) OR '*'=ANY(%s::text[]))", alias, p, p)
}
func knowledgeWhere(alias string, a Actor, args *[]any) string {
	if a.Has("admin") {
		return "true"
	}
	id := param(args, a.ID)
	ch := param(args, a.ChannelIDs)
	return fmt.Sprintf("(%s.visibility IN ('public','internal') OR %s.author_id=%s OR %s.channel_ids && %s::text[] OR '*'=ANY(%s::text[]))", alias, alias, id, alias, ch, ch)
}

func taskSourceWhere(taskID string, a Actor, args *[]any) string {
	if a.Has("admin") {
		return "true"
	}
	// Generated reports and code can repeat any previously supplied source.
	return "NOT EXISTS(SELECT 1 FROM task_inputs si JOIN source_messages sm ON sm.id=si.source_message_id JOIN cases sc ON sc.id=sm.case_id WHERE si.task_id=" + taskID + " AND NOT COALESCE(" + caseWhere("sc", a, args) + ",false))"
}
func visible(resource string, a Actor, args *[]any) string {
	if a.Has("admin") {
		return "true"
	}
	switch resource {
	case "cases":
		return caseWhere("t", a, args)
	case "knowledge":
		return knowledgeWhere("t", a, args)
	case "development-tasks":
		return "EXISTS(SELECT 1 FROM cases c WHERE c.id=t.case_id AND " + caseWhere("c", a, args) + ") AND " + repoWhere("t", a, args) + " AND " + taskSourceWhere("t.id", a, args)
	case "replies":
		return "EXISTS(SELECT 1 FROM cases c WHERE c.id=t.case_id AND " + caseWhere("c", a, args) + ")"
	case "approvals":
		return "((t.subject_type='admission' AND EXISTS(SELECT 1 FROM admission_decisions d JOIN cases c ON c.id=d.case_id WHERE d.id=t.subject_id AND " + caseWhere("c", a, args) + " AND (d.repository_id IS NULL OR " + repoWhere("d", a, args) + "))) OR (t.subject_type='reply' AND EXISTS(SELECT 1 FROM replies r JOIN cases c ON c.id=r.case_id WHERE r.id=t.subject_id AND " + caseWhere("c", a, args) + ")) OR (t.subject_type='knowledge' AND EXISTS(SELECT 1 FROM knowledge k WHERE k.id=t.subject_id AND " + knowledgeWhere("k", a, args) + ")) OR (t.subject_type='step' AND EXISTS(SELECT 1 FROM task_steps st JOIN dev_tasks d ON d.id=st.task_id JOIN cases c ON c.id=d.case_id WHERE st.id=t.subject_id AND " + caseWhere("c", a, args) + " AND " + repoWhere("d", a, args) + " AND " + taskSourceWhere("d.id", a, args) + ")))"
	case "principals", "owner-routes":
		return "true"
	case "channels":
		p := param(args, a.ChannelIDs)
		return "(t.id::text=ANY(" + p + "::text[]) OR '*'=ANY(" + p + "::text[]))"
	case "repositories":
		p := param(args, a.RepositoryIDs)
		return "(t.id::text=ANY(" + p + "::text[]) OR '*'=ANY(" + p + "::text[]))"
	case "knowledge-sources":
		if a.Has("knowledge") {
			return "true"
		}
	case "artifacts":
		return "((t.task_id='' AND t.case_id<>'' AND EXISTS(SELECT 1 FROM cases c WHERE c.id::text=t.case_id AND " + caseWhere("c", a, args) + ")) OR (t.task_id<>'' AND EXISTS(SELECT 1 FROM dev_tasks d JOIN cases c ON c.id=d.case_id WHERE d.id::text=t.task_id AND " + caseWhere("c", a, args) + " AND " + repoWhere("d", a, args) + " AND " + taskSourceWhere("d.id", a, args) + ")))"
	}
	return "false"
}
func selector(resource string) string {
	if resource == "principals" {
		return "to_jsonb(t)-'password_hash'"
	}
	return "to_jsonb(t)"
}
func (s *Service) Get(ctx context.Context, q DB, a Actor, resource, id string) (Data, error) {
	if resource == "admission-rules" {
		if !a.Has("admin") {
			return nil, Forbidden
		}
		return nil, Fail(410, "policy_moved", "请在审批策略中配置创建开发任务")
	}
	if !validID(id) {
		return nil, NotFound
	}
	table := tables[resource]
	args := []any{id}
	where := "t.id=$1"
	if configKinds[resource] {
		table = "configs"
		where += " AND t.kind=" + param(&args, resource)
	}
	if table == "" {
		return nil, NotFound
	}
	where += " AND " + visible(resource, a, &args)
	m, e := one(ctx, q, "SELECT "+selector(resource)+" FROM "+table+" t WHERE "+where, args...)
	if e != nil {
		return nil, e
	}
	if configKinds[resource] {
		m = configPublic(m)
	}
	return m, nil
}
func (s *Service) List(ctx context.Context, a Actor, resource string, f url.Values) (Data, error) {
	if resource == "admission-rules" {
		if !a.Has("admin") {
			return nil, Forbidden
		}
		return nil, Fail(410, "policy_moved", "请在审批策略中配置创建开发任务")
	}
	table := tables[resource]
	args := []any{}
	where := "true"
	if configKinds[resource] {
		table = "configs"
		where = "t.kind=" + param(&args, resource)
	}
	if table == "" {
		return nil, NotFound
	}
	where += " AND " + visible(resource, a, &args)
	for _, k := range []string{"status", "module", "channel_id", "assignee_id", "kind", "subject_id"} {
		v := f.Get(k)
		if v == "" {
			continue
		}
		p := param(&args, v)
		where += " AND to_jsonb(t)->>" + "'" + k + "'=" + p
		if configKinds[resource] {
			where = strings.TrimSuffix(where, " AND to_jsonb(t)->>'"+k+"'="+p) + " AND t.data->>'" + k + "'=" + p
		}
	}
	if resource == "jobs" && f.Get("ignored") != "" {
		switch f.Get("ignored") {
		case "true":
			where += " AND t.ignored_at IS NOT NULL"
		case "false":
			where += " AND t.ignored_at IS NULL"
		default:
			return nil, Fail(422, "validation", "忽略筛选条件无效")
		}
	}
	if q := f.Get("q"); q != "" {
		p := param(&args, "%"+q+"%")
		var search string
		if configKinds[resource] {
			search = "t.name"
		} else if resource == "cases" || resource == "knowledge" {
			search = "t.title"
		} else if resource == "principals" {
			search = "t.name||' '||t.email"
		} else {
			search = "to_jsonb(t)->>'summary'"
		}
		where += " AND " + search + " ILIKE " + p
	}
	var total int
	if e := s.DB.QueryRow(ctx, "SELECT count(*) FROM "+table+" t WHERE "+where, args...).Scan(&total); e != nil {
		return nil, e
	}
	if cur := f.Get("cursor"); cur != "" {
		b, e := base64.RawURLEncoding.DecodeString(cur)
		if e != nil {
			return nil, Fail(422, "invalid_cursor", "分页游标无效")
		}
		var c []string
		if json.Unmarshal(b, &c) != nil || len(c) != 2 || !validID(c[1]) {
			return nil, Fail(422, "invalid_cursor", "分页游标无效")
		}
		p1 := param(&args, c[0])
		p2 := param(&args, c[1])
		where += " AND (t.created_at,t.id)<(" + p1 + "::timestamptz," + p2 + "::uuid)"
	}
	limit, _ := strconv.Atoi(f.Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 30
	}
	p := param(&args, limit+1)
	items, e := many(ctx, s.DB, "SELECT "+selector(resource)+" FROM "+table+" t WHERE "+where+" ORDER BY t.created_at DESC,t.id DESC LIMIT "+p, args...)
	if e != nil {
		return nil, e
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = base64.RawURLEncoding.EncodeToString(JSON([]string{Str(last, "created_at"), Str(last, "id")}))
	}
	if configKinds[resource] {
		for i := range items {
			items[i] = configPublic(items[i])
		}
	}
	return Data{"items": items, "next_cursor": next, "total": total}, nil
}
func (s *Service) Detail(ctx context.Context, a Actor, resource, id string) (Data, error) {
	m, e := s.Get(ctx, s.DB, a, resource, id)
	if e != nil {
		return nil, e
	}
	switch resource {
	case "cases":
		messages, err := many(ctx, s.DB, "SELECT to_jsonb(m) FROM source_messages m WHERE case_id=$1 ORDER BY created_at LIMIT 200", id)
		e = err
		if e != nil {
			return nil, e
		}
		identities := map[string]Data{}
		for _, message := range messages {
			key := Str(message, "connector_id") + ":" + Str(message, "actor_id")
			identity, ok := identities[key]
			if !ok {
				identity, e = sourceIdentity(ctx, s.DB, message)
				if e != nil {
					return nil, e
				}
				identities[key] = identity
			}
			message["actor_name"] = Str(identity, "name")
		}
		m["messages"] = messages
		m["replies"], e = many(ctx, s.DB, "SELECT to_jsonb(r) FROM replies r WHERE case_id=$1 ORDER BY created_at,id LIMIT 100", id)
		if e != nil {
			return nil, e
		}
		m["events"], e = many(ctx, s.DB, "SELECT to_jsonb(ev) FROM case_events ev WHERE case_id=$1 ORDER BY created_at DESC LIMIT 100", id)
		var tid string
		_ = s.DB.QueryRow(ctx, "SELECT id FROM dev_tasks WHERE canonical_case_id=$1 AND status NOT IN ('cancelled','failed','completed') ORDER BY created_at DESC LIMIT 1", Str(m, "canonical_case_id")).Scan(&tid)
		m["development_task_id"] = tid
	case "development-tasks":
		m["steps"], e = many(ctx, s.DB, "SELECT to_jsonb(st) FROM task_steps st WHERE task_id=$1 ORDER BY revision,created_at", id)
		if e != nil {
			return nil, e
		}
		m["runs"], e = many(ctx, s.DB, "SELECT to_jsonb(r)-'spec' FROM run_attempts r WHERE task_id=$1 ORDER BY created_at DESC LIMIT 100", id)
	}
	return m, e
}
func (s *Service) Events(ctx context.Context, a Actor, resource, id, cursor string) (Data, error) {
	table, col := "case_events", "case_id"
	if resource == "run-attempts" {
		r, e := one(ctx, s.DB, "SELECT to_jsonb(r) FROM run_attempts r WHERE id::text=$1", id)
		if e != nil {
			return nil, e
		}
		if _, e = s.Get(ctx, s.DB, a, "development-tasks", Str(r, "task_id")); e != nil {
			return nil, e
		}
		table, col = "run_events", "run_id"
	} else if _, e := s.Get(ctx, s.DB, a, "cases", id); e != nil {
		return nil, e
	}
	args := []any{id}
	where := col + "::text=$1"
	if cursor != "" {
		if !validID(cursor) {
			return nil, Fail(422, "invalid_cursor", "游标无效")
		}
		args = append(args, cursor)
		where += " AND (created_at,id)>(SELECT created_at,id FROM " + table + " WHERE id=$2)"
	}
	items, e := many(ctx, s.DB, "SELECT to_jsonb(e) FROM "+table+" e WHERE "+where+" ORDER BY created_at,id LIMIT 100", args...)
	next := ""
	if len(items) > 0 {
		next = Str(items[len(items)-1], "id")
	}
	return Data{"items": items, "next_cursor": next, "total": len(items)}, e
}
func (s *Service) Dashboard(ctx context.Context, a Actor) (Data, error) {
	d := Data{}
	for _, x := range []struct{ key, resource, status string }{{"open_cases", "cases", ""}, {"pending_approvals", "approvals", "pending"}, {"running_tasks", "development-tasks", "running"}, {"published_knowledge", "knowledge", "published"}} {
		f := url.Values{"limit": {"5"}}
		if x.status != "" {
			f.Set("status", x.status)
		}
		v, e := s.List(ctx, a, x.resource, f)
		if e != nil {
			return nil, e
		}
		d[x.key] = v["total"]
		if x.resource == "cases" {
			d["recent_cases"] = v["items"]
		}
	}
	v, e := s.List(ctx, a, "development-tasks", url.Values{"limit": {"5"}})
	if e != nil {
		return nil, e
	}
	d["recent_tasks"] = v["items"]
	d["activity"] = []Data{}
	args := []any{}
	scope := caseWhere("c", a, &args)
	var open, unassigned int
	if e := s.DB.QueryRow(ctx, "SELECT count(*) FILTER (WHERE status NOT IN ('resolved','closed')),count(*) FILTER (WHERE assignee_id='' AND status NOT IN ('resolved','closed')) FROM cases c WHERE "+scope, args...).Scan(&open, &unassigned); e != nil {
		return nil, e
	}
	d["open_cases"] = open
	d["unassigned_cases"] = unassigned
	d["failed_jobs"] = 0
	d["unknown_operations"] = 0
	if a.Has("admin") {
		var n int
		_ = s.DB.QueryRow(ctx, "SELECT count(*) FROM cases WHERE assignee_id='' AND status NOT IN ('resolved','closed')").Scan(&n)
		d["unassigned_cases"] = n
		_ = s.DB.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE status='failed' AND ignored_at IS NULL").Scan(&n)
		d["failed_jobs"] = n
		_ = s.DB.QueryRow(ctx, "SELECT count(*) FROM external_operations WHERE status='outcome_unknown'").Scan(&n)
		d["unknown_operations"] = n
		d["activity"], e = many(ctx, s.DB, "SELECT to_jsonb(ev) FROM case_events ev ORDER BY created_at DESC LIMIT 10")
	}
	return d, e
}
