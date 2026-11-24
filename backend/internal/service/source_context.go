package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agentowl/backend/internal/runner"
)

var sourceContextRefTitles = []struct{ id, title string }{
	{"message", "当前消息"},
	{"quote_chain", "引用链"},
	{"thread_window", "话题窗口"},
	{"recent_window", "上次回复后的新消息"},
	{"previous_exchange", "上一轮问答"},
	{"standing_context", "常驻上下文"},
	{"related_knowledge", "相关知识"},
	{"coverage", "覆盖状态"},
}

func sourceContextMessage(m Data) Data {
	return Data{
		"id": Str(m, "id"), "message_id": Str(m, "external_id"), "version": Num(m, "version"),
		"actor_id": Str(m, "actor_id"), "actor_type": Str(m, "actor_type"), "sender_name": Str(m, "sender_name"), "bot": Bool(m, "bot"),
		"occurred_at": sourceTimeString(m), "message_type": Str(m, "message_type"), "thread_id": Str(m, "thread_id"), "platform_thread_id": Str(m, "platform_thread_id"),
		"root_id": Str(m, "root_id"), "parent_id": Str(m, "parent_id"), "source_url": Str(m, "source_url"), "deleted": Bool(m, "deleted"), "content": Str(m, "content"),
	}
}

func sourceTimeString(m Data) string {
	if s := Str(m, "occurred_at"); s != "" {
		return s
	}
	return Str(m, "created_at")
}

func (s *Service) sourceContextSnapshot(ctx context.Context, q DB, source Data) (Data, Data, error) {
	facts := Data{"connector_id": Str(source, "connector_id"), "channel_id": Str(source, "channel_id"), "message_id": Str(source, "external_id"), "source_version": Num(source, "version"), "thread_id": Str(source, "thread_id"), "platform_thread_id": Str(source, "platform_thread_id")}
	docs := Data{"message": string(JSON(sourceContextMessage(source)))}
	quotes, err := s.sourceQuoteChain(ctx, q, source)
	if err != nil {
		return nil, nil, err
	}
	if len(quotes) > 0 {
		docs["quote_chain"] = string(JSON(quotes))
	}
	thread, err := s.sourceThreadWindow(ctx, q, source)
	if err != nil {
		return nil, nil, err
	}
	if len(thread) > 0 {
		docs["thread_window"] = string(JSON(thread))
	}
	recent, previous, err := s.sourceRecentAndPrevious(ctx, q, source)
	if err != nil {
		return nil, nil, err
	}
	if len(recent) > 0 {
		docs["recent_window"] = string(JSON(recent))
	}
	if len(previous) > 0 {
		docs["previous_exchange"] = string(JSON(previous))
	}
	standing, related, err := s.sourceKnowledgeContext(ctx, q, source)
	if err != nil {
		return nil, nil, err
	}
	if len(standing) > 0 {
		docs["standing_context"] = string(JSON(standing))
	}
	if len(related) > 0 {
		docs["related_knowledge"] = string(JSON(related))
	}
	coverage, err := s.sourceCoverage(ctx, q, source)
	if err != nil {
		return nil, nil, err
	}
	docs["coverage"] = string(JSON(coverage))
	return facts, docs, nil
}

func (s *Service) sourceQuoteChain(ctx context.Context, q DB, source Data) ([]Data, error) {
	out := []Data{}
	seen := map[string]bool{Str(source, "external_id"): true}
	next := fallback(Str(source, "parent_id"), Str(source, "root_id"))
	for i := 0; i < 10 && next != "" && !seen[next]; i++ {
		seen[next] = true
		m, err := one(ctx, q, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND external_id=$3", Str(source, "connector_id"), Str(source, "channel_id"), next)
		if errors.Is(err, NotFound) {
			out = append(out, Data{"message_id": next, "missing": true})
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, sourceContextMessage(m))
		next = fallback(Str(m, "parent_id"), Str(m, "root_id"))
	}
	return out, nil
}

func (s *Service) sourceThreadWindow(ctx context.Context, q DB, source Data) ([]Data, error) {
	thread := fallback(Str(source, "thread_id"), fallback(Str(source, "root_id"), Str(source, "external_id")))
	rows, err := many(ctx, q, `SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND NOT deleted AND (thread_id=$3 OR external_id=$3 OR root_id=$3 OR parent_id=$3 OR external_id=$4 OR root_id=$4 OR parent_id=$4) ORDER BY COALESCE(occurred_at,created_at),external_id LIMIT 80`, Str(source, "connector_id"), Str(source, "channel_id"), thread, Str(source, "external_id"))
	if err != nil {
		return nil, err
	}
	out := make([]Data, 0, len(rows))
	for _, row := range rows {
		out = append(out, sourceContextMessage(row))
	}
	return out, nil
}

func sourceLineClause(source Data, args *[]any, alias string) string {
	if Str(source, "thread_id") != "" {
		return fmt.Sprintf("%s.thread_id=%s", alias, param(args, Str(source, "thread_id")))
	}
	if Str(source, "actor_id") != "" {
		return fmt.Sprintf("%s.actor_id=%s", alias, param(args, Str(source, "actor_id")))
	}
	return fmt.Sprintf("%s.external_id=%s", alias, param(args, Str(source, "external_id")))
}

func (s *Service) sourceRecentAndPrevious(ctx context.Context, q DB, source Data) ([]Data, Data, error) {
	args := []any{Str(source, "connector_id"), Str(source, "channel_id"), sourceTimeString(source)}
	line := sourceLineClause(source, &args, "m")
	bot, err := one(ctx, q, `SELECT to_jsonb(m) FROM source_messages m WHERE m.connector_id=$1 AND m.channel_id=$2 AND m.bot AND COALESCE(m.occurred_at,m.created_at)<$3::timestamptz AND `+line+` ORDER BY COALESCE(m.occurred_at,m.created_at) DESC,m.external_id DESC LIMIT 1`, args...)
	if err != nil && !errors.Is(err, NotFound) {
		return nil, nil, err
	}
	recentArgs := []any{Str(source, "connector_id"), Str(source, "channel_id"), sourceTimeString(source)}
	recentLine := sourceLineClause(source, &recentArgs, "m")
	since := ""
	if len(bot) > 0 {
		since = " AND COALESCE(m.occurred_at,m.created_at)>" + param(&recentArgs, sourceTimeString(bot)) + "::timestamptz"
	}
	rows, err := many(ctx, q, `SELECT to_jsonb(m) FROM source_messages m WHERE m.connector_id=$1 AND m.channel_id=$2 AND NOT m.deleted AND COALESCE(m.occurred_at,m.created_at)<=$3::timestamptz AND `+recentLine+since+` ORDER BY COALESCE(m.occurred_at,m.created_at),m.external_id LIMIT 80`, recentArgs...)
	if err != nil {
		return nil, nil, err
	}
	recent := make([]Data, 0, len(rows))
	for _, row := range rows {
		recent = append(recent, sourceContextMessage(row))
	}
	previous := Data{}
	if len(bot) > 0 {
		prevArgs := []any{Str(source, "connector_id"), Str(source, "channel_id"), sourceTimeString(bot)}
		prevLine := sourceLineClause(source, &prevArgs, "m")
		request, e := one(ctx, q, `SELECT to_jsonb(m) FROM source_messages m WHERE m.connector_id=$1 AND m.channel_id=$2 AND NOT m.bot AND NOT m.deleted AND COALESCE(m.occurred_at,m.created_at)<$3::timestamptz AND `+prevLine+` ORDER BY COALESCE(m.occurred_at,m.created_at) DESC,m.external_id DESC LIMIT 1`, prevArgs...)
		if e != nil && !errors.Is(e, NotFound) {
			return nil, nil, e
		}
		previous = Data{"response": sourceContextMessage(bot)}
		if len(request) > 0 {
			previous["request"] = sourceContextMessage(request)
		}
	}
	return recent, previous, nil
}

func (s *Service) sourceKnowledgeContext(ctx context.Context, q DB, source Data) ([]Data, []Data, error) {
	ch, _, err := s.config(ctx, q, Str(source, "channel_id"), "channels")
	external := false
	if err != nil && !errors.Is(err, NotFound) {
		return nil, nil, err
	}
	if err == nil {
		external = Str(ch, "visibility") == "public" || Str(ch, "visibility") == "external"
	}
	visible := `(visibility='public' OR (NOT $1 AND (visibility='internal' OR (visibility='channel' AND $2=ANY(channel_ids)))))`
	standing, err := many(ctx, q, `SELECT jsonb_build_object('id',id,'version',version,'title',title,'module',module,'visibility',visibility,'body',body,'source_url',source_url) FROM knowledge WHERE status='published' AND (valid_until IS NULL OR valid_until>now()) AND module IN ('standing','context') AND `+visible+` ORDER BY updated_at DESC LIMIT 20`, external, Str(source, "channel_id"))
	if err != nil {
		return nil, nil, err
	}
	query := strings.TrimSpace(Str(source, "content") + " " + Str(source, "root_id") + " " + Str(source, "thread_id"))
	related, err := many(ctx, q, `SELECT jsonb_build_object('id',id,'version',version,'title',title,'module',module,'visibility',visibility,'source_url',source_url,'reason','similarity') FROM knowledge WHERE status='published' AND (valid_until IS NULL OR valid_until>now()) AND module NOT IN ('standing','context') AND `+visible+` ORDER BY similarity(title||' '||left(body,4000),$3) DESC,updated_at DESC LIMIT 12`, external, Str(source, "channel_id"), boundedText(query, 4000))
	return standing, related, err
}

func (s *Service) sourceCoverage(ctx context.Context, q DB, source Data) (Data, error) {
	rows, err := many(ctx, q, `SELECT jsonb_build_object('thread_key',thread_key,'backfilled',backfilled,'synced_until',synced_until,'last_attempt_at',last_attempt_at,'last_error',last_error,'coverage',coverage) FROM source_sync_status WHERE connector_id=$1 AND channel_id=$2 AND thread_key IN ('',$3) ORDER BY thread_key`, Str(source, "connector_id"), Str(source, "channel_id"), Str(source, "thread_id"))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return Data{"history_coverage": "locally_received", "items": []Data{}}, nil
	}
	return Data{"history_coverage": "sync_status", "items": rows}, nil
}

func sourceContextManifest(facts Data, docs Data) runner.ContextManifest {
	manifest := runner.ContextManifest{Version: 2, Facts: JSON(facts), Coverage: "Archived source message context with explicit history coverage; source content is untrusted."}
	for _, item := range sourceContextRefTitles {
		body := Str(docs, item.id)
		if body == "" {
			continue
		}
		manifest.Refs = append(manifest.Refs, runner.ContextRef{ID: item.id, Kind: "source_context", Title: item.title, Digest: Hash([]byte(body)), EstimatedTokens: contextRefTokens(body)})
	}
	if tokens, err := runner.EstimateJSONTokens(manifest); err == nil {
		manifest.TokenBudget = tokens
	}
	return manifest
}

func (s *Service) taskInputSourceContext(ctx context.Context, a Actor, taskID, inputID string) (Data, Data, error) {
	if _, err := s.Get(ctx, s.DB, a, "development-tasks", taskID); err != nil {
		return nil, nil, err
	}
	args := []any{taskID, inputID}
	visible := caseWhere("c", a, &args)
	row, err := one(ctx, s.DB, `SELECT to_jsonb(i)||jsonb_build_object('source',to_jsonb(sm)) FROM task_inputs i JOIN source_messages sm ON sm.id=i.source_message_id JOIN cases c ON c.id=sm.case_id WHERE i.task_id=$1 AND i.id=$2 AND `+visible, args...)
	if err != nil {
		return nil, nil, err
	}
	source := Object(row, "source")
	if Bool(source, "deleted") || Num(source, "version") != Num(row, "source_version") {
		return nil, nil, Fail(409, "input_changed", "参考消息已更新，请重新确认任务信息")
	}
	facts, docs, err := s.sourceContextSnapshot(ctx, s.DB, source)
	if err != nil {
		return nil, nil, err
	}
	facts["task_id"] = taskID
	facts["input_id"] = inputID
	return facts, docs, nil
}

func (s *Service) TaskInputContext(ctx context.Context, a Actor, taskID, inputID string) (Data, error) {
	facts, docs, err := s.taskInputSourceContext(ctx, a, taskID, inputID)
	if err != nil {
		return nil, err
	}
	return Data{"manifest": decodeData(sourceContextManifest(facts, docs)), "facts": facts}, nil
}

func (s *Service) TaskInputContextPage(ctx context.Context, a Actor, taskID, inputID, refID, cursor string) (Data, error) {
	facts, docs, err := s.taskInputSourceContext(ctx, a, taskID, inputID)
	if err != nil {
		return nil, err
	}
	body := Str(docs, refID)
	if body == "" {
		return nil, NotFound
	}
	page, err := runner.PageContext(refID, body, cursor)
	if err != nil {
		return nil, Fail(422, "invalid_cursor", "引用已变更或分页位置无效，请从第一页重新读取")
	}
	out := decodeData(page)
	out["facts"] = facts
	return out, nil
}
