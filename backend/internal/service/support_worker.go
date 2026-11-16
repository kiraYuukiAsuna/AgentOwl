package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

func (s *Service) ProcessSupportJob(ctx context.Context, kind string, p Data) error {
	switch kind {
	case "route_task_source":
		return s.routeTaskSource(ctx, Str(p, "event_id"))
	case "evaluate_task_inputs":
		return s.evaluateTaskInputs(ctx, Str(p, "task_id"))
	case "apply_task_inputs":
		return s.applyTaskInputs(ctx, p)
	case "evaluate_admission":
		return s.evaluateAdmission(ctx, Str(p, "case_id"), Str(p, "request_key"))
	case "ingest_event":
		return s.consumeSupportEvent(ctx, Str(p, "event_id"))
	case "classify":
		return s.classifySupport(ctx, p)
	case "answer":
		return s.answerSupport(ctx, p)
	case "verify_connector":
		return s.verifySupportConnector(ctx, p)
	case "sync_channel":
		return s.syncSupportChannel(ctx, p)
	case "poll_github":
		return s.pollGitHubChannel(ctx, p)
	case "sync_source":
		return s.syncSupportSource(ctx, p)
	case "notify":
		return s.queueSupportNotification(ctx, p)
	default:
		return Fail(422, "unknown_job", "未知任务类型")
	}
}
func boundedText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func supportImageContext(images []integration.InputImage) string {
	if len(images) == 0 {
		return ""
	}
	parts := make([]string, 0, len(images))
	for _, image := range images {
		name := image.Name
		if strings.TrimSpace(name) == "" {
			name = "image"
		}
		parts = append(parts, name+" ("+image.MediaType+", "+strconv.Itoa(len(image.Data))+" bytes)")
	}
	return "\n\nAttached images available to the model: " + strings.Join(parts, "; ")
}
func sourceClock(s string) time.Time {
	s = strings.Split(s, ":deleted:")[0]
	if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
		return t
	}
	n, e := strconv.ParseInt(s, 10, 64)
	if e != nil {
		return time.Time{}
	}
	if n > 1e17 {
		return time.Unix(0, n)
	}
	if n > 1e14 {
		return time.UnixMicro(n)
	}
	if n > 1e11 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}
func sourceThread(e integration.Event) string {
	if e.Repository != "" {
		return e.ChannelID
	}
	return fallback(e.ThreadID, fallback(e.RootID, fallback(e.ParentID, e.MessageID)))
}

func (s *Service) consumeSupportEvent(ctx context.Context, id string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		in, e := locked(ctx, tx, "ingest_events", id)
		if e != nil {
			return e
		}
		if Str(in, "status") == "processed" {
			return nil
		}
		p := Object(in, "payload")
		ev := integration.Event{}
		if e = json.Unmarshal(JSON(p["event"]), &ev); e != nil {
			return e
		}
		ch := Object(p, "channel")
		if len(ch) == 0 {
			ch, _, e = s.config(ctx, tx, Str(p, "channel_id"), "channels")
			if e != nil {
				return e
			}
		}
		co, _, e := s.config(ctx, tx, Str(in, "connector_id"), "connectors")
		if e != nil {
			return e
		}
		finish := func() error {
			_, e := tx.Exec(ctx, "UPDATE ingest_events SET status='processed' WHERE id=$1", id)
			if e != nil {
				return e
			}
			return s.finishTaskSource(ctx, tx, id)
		}
		if !Bool(ch, "enabled") || connectorDisabled(co) || Str(ch, "connector_id") != Str(in, "connector_id") {
			return finish()
		}
		// Bot echoes never initiate a support cycle. Deleted bot messages may still
		// remove previously stored source data.
		if ev.Bot && !ev.Deleted {
			var allowed bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM configs WHERE kind='policies' AND data->>'kind'='create_task' AND data->>'mode'<>'deny' AND data->>'channel_id'=$1 AND data->>'enabled'='true' AND data->>'allow_bot'='true')`, Str(ch, "id")).Scan(&allowed); e != nil {
				return e
			}
			if !allowed || ev.ActorID == Str(co, "bot_open_id") || ev.ActorID == Str(co, "sender_id") {
				return finish()
			}
		}
		if !ev.Deleted && Str(co, "provider") == "github" {
			if strings.HasPrefix(ev.MessageID, "comment:") && Str(co, "sender_id") != "" && Str(co, "sender_id") == ev.ActorID {
				return finish()
			}
			if strings.HasPrefix(ev.MessageID, "comment:") {
				var own bool
				if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM external_operations WHERE remote_id=$1 AND kind IN ('notify','reply_send') AND target->>'channel_id'=$2)", strings.TrimPrefix(ev.MessageID, "comment:"), Str(ch, "id")).Scan(&own); e != nil {
					return e
				}
				if own {
					return finish()
				}
			}
		}
		thread := sourceThread(ev)
		historical := strings.HasPrefix(ev.ID, "history:")
		lockThread := thread
		if ev.ChatType == "p2p" && ev.ActorID != "" {
			lockThread = "p2p:" + ev.ActorID
		}
		if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", Str(in, "connector_id")+":"+Str(ch, "id")+":"+lockThread); e != nil {
			return e
		}
		old, e := one(ctx, tx, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND external_id=$3 FOR UPDATE", Str(in, "connector_id"), Str(ch, "id"), ev.MessageID)
		if e != nil && e != NotFound {
			return e
		}
		exists := e == nil
		if exists {
			if Bool(old, "deleted") && !ev.Deleted {
				return finish()
			}
			a, b := sourceClock(Str(old, "source_revision")), sourceClock(ev.SourceRevision)
			if !a.IsZero() && !b.IsZero() && b.Before(a) {
				return finish()
			}
			if Str(old, "source_revision") == ev.SourceRevision && Bool(old, "deleted") == ev.Deleted && Str(old, "content") == ev.Text {
				return finish()
			}
		}
		cid := Str(old, "case_id")
		if cid == "" {
			m, err := one(ctx, tx, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND case_id IS NOT NULL AND (thread_id=$3 OR external_id=$4 OR external_id=$5) ORDER BY created_at LIMIT 1", Str(in, "connector_id"), Str(ch, "id"), thread, ev.RootID, ev.ParentID)
			if err != nil && err != NotFound {
				return err
			}
			cid = Str(m, "case_id")
		}
		if cid == "" && ev.ChatType == "p2p" && ev.ActorID != "" && !ev.Deleted {
			m, err := one(ctx, tx, `SELECT to_jsonb(m) FROM source_messages m JOIN cases c ON c.id=m.case_id
				WHERE m.connector_id=$1 AND m.channel_id=$2 AND m.actor_id=$3 AND m.case_id IS NOT NULL AND NOT m.deleted
				AND c.status NOT IN ('resolved','closed')
				ORDER BY m.created_at DESC,m.external_id DESC LIMIT 1`, Str(in, "connector_id"), Str(ch, "id"), ev.ActorID)
			if err != nil && err != NotFound {
				return err
			}
			cid = Str(m, "case_id")
		}
		if cid == "" && !ev.Deleted {
			cid = ID()
			title := boundedText(strings.TrimSpace(strings.Split(ev.Text, "\n")[0]), 250)
			if title == "" {
				title = "待识别的用户消息"
			}
			replyMode := fallback(Str(ch, "reply_mode"), "draft")
			if historical && replyMode != "manual" {
				replyMode = "draft"
			}
			caseChannelID := Str(ch, "id")
			if Bool(ch, "private") {
				caseChannelID = ""
			}
			_, e = tx.Exec(ctx, "INSERT INTO cases(id,canonical_case_id,title,description,channel_id,reply_mode,source,created_by,environment) VALUES($1,$1,$2,$3,$4,$5,$6,'system:ingest',$7)", cid, title, boundedText(ev.Text, 64000), caseChannelID, replyMode, Str(co, "provider"), JSON(Data{"source_thread": thread, "historical": historical, "p2p_chat": ev.ChatType == "p2p"}))
			if e != nil {
				return e
			}
		}
		mid := Str(old, "id")
		content := boundedText(integration.Redact(ev.Text), 64000)
		if ev.Deleted {
			content = ""
		}
		var caseID any
		if cid != "" {
			caseID = cid
		}
		if exists {
			_, e = tx.Exec(ctx, "UPDATE source_messages SET content=$2,source_revision=$3,deleted=$4,version=version+1,updated_at=now() WHERE id=$1", mid, content, ev.SourceRevision, ev.Deleted)
		} else {
			mid = ID()
			occurred := ev.OccurredAt
			if occurred.IsZero() {
				occurred = time.Now()
			}
			_, e = tx.Exec(ctx, "INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,actor_id,content,thread_id,source_url,source_revision,deleted,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)", mid, Str(in, "connector_id"), Str(ch, "id"), ev.MessageID, caseID, ev.ActorID, content, thread, ev.SourceURL, ev.SourceRevision, ev.Deleted, occurred)
		}
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO source_message_versions(message_id,version,content,source_revision,deleted) SELECT id,version,content,source_revision,deleted FROM source_messages WHERE id=$1", mid); e != nil {
			return e
		}
		if cid != "" {
			c, e := locked(ctx, tx, "cases", cid)
			if e != nil {
				return e
			}
			env := Object(c, "environment")
			replyMode := Str(c, "reply_mode")
			if Bool(env, "historical") && !historical && replyMode == "draft" {
				replyMode = fallback(Str(ch, "reply_mode"), "draft")
			}
			if !historical {
				env["historical"] = false
			}
			env["external_channel_id"] = ev.ChannelID
			env["source_number"] = ev.Number
			env["source_thread"] = thread
			env["platform_thread_id"] = ev.ThreadID
			env["last_message_id"] = ev.MessageID
			if len(ev.Attachments) > 0 {
				env["attachments"] = ev.Attachments
				env["attachment_message_id"] = ev.MessageID
				env["attachment_source_revision"] = ev.SourceRevision
				env["attachment_connector_id"] = Str(co, "id")
				env["attachment_app_id"] = ev.AppID
				env["attachment_tenant_id"] = ev.TenantID
			}
			env["p2p_chat"] = ev.ChatType == "p2p"
			env["source_bot"] = ev.Bot
			if Bool(ch, "private") {
				env["private_connector_id"] = Str(ch, "connector_id")
				env["private_actor_id"] = ev.ActorID
			}
			mentioned := Str(co, "bot_open_id") != "" && contains(ev.MentionedUserIDs, Str(co, "bot_open_id"))
			if ev.ParentID != "" {
				var own bool
				if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM external_operations WHERE remote_id=$1 AND status='succeeded' AND target->>'channel_id'=$2)", ev.ParentID, Str(ch, "id")).Scan(&own); e != nil {
					return e
				}
				mentioned = mentioned || own
			}
			env["mentioned_bot"] = mentioned
			msgs, e := many(ctx, tx, "SELECT to_jsonb(m) FROM (SELECT content,external_id,created_at FROM source_messages WHERE case_id=$1 AND NOT deleted ORDER BY created_at DESC,external_id DESC LIMIT 30) m ORDER BY created_at,external_id", cid)
			if e != nil {
				return e
			}
			parts := []string{}
			for _, m := range msgs {
				parts = append(parts, Str(m, "content"))
			}
			description := boundedText(strings.Join(parts, "\n\n"), 64000)
			// Titles and summaries are derived source content too. A recalled first
			// message must not survive in the next model prompt through those fields.
			title := "已撤回的用户问题"
			env["last_message_id"] = ""
			if len(msgs) > 0 {
				title = boundedText(strings.TrimSpace(strings.Split(Str(msgs[0], "content"), "\n")[0]), 250)
				if title == "" {
					title = "待识别的用户消息"
				}
				env["last_message_id"] = Str(msgs[len(msgs)-1], "external_id")
			}
			if refs, attachment, err := s.caseAttachmentContext(ctx, tx, Str(ch, "id"), cid, co, id); err != nil {
				return err
			} else if len(refs) > 0 {
				env["attachments"] = refs
				env["attachment_message_id"] = Str(attachment, "message_id")
				env["attachment_source_revision"] = Str(attachment, "source_revision")
				env["attachment_connector_id"] = Str(attachment, "connector_id")
				env["attachment_app_id"] = Str(attachment, "app_id")
				env["attachment_tenant_id"] = Str(attachment, "tenant_id")
			} else {
				delete(env, "attachments")
				delete(env, "attachment_message_id")
				delete(env, "attachment_source_revision")
				delete(env, "attachment_connector_id")
				delete(env, "attachment_app_id")
				delete(env, "attachment_tenant_id")
			}
			_, e = tx.Exec(ctx, "UPDATE cases SET description=$2,environment=$3,status=CASE WHEN status IN ('closed','resolved') AND NOT $4 THEN 'triage' ELSE status END,title=$5,summary='',reply_mode=$6,version=version+1,updated_at=now() WHERE id=$1", cid, description, JSON(env), ev.Deleted, title, replyMode)
			if e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "UPDATE replies SET status='stale',version=version+1,updated_at=now() WHERE case_id=$1 AND status IN ('generating','draft','pending','approved')", cid); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE subject_type='reply' AND subject_id IN (SELECT id FROM replies WHERE case_id=$1) AND status IN ('pending','approved')", cid); e != nil {
				return e
			}
			if e = caseEvent(ctx, tx, cid, "source_updated", ev.ActorID, content, Data{"message_id": mid, "deleted": ev.Deleted, "source_revision": ev.SourceRevision}); e != nil {
				return e
			}
			if !historical && !ev.Deleted && replyMode == "manual" && strings.TrimSpace(content) != "" {
				if e = enqueue(ctx, tx, "notify", "notify-manual:"+cid+":"+strconv.FormatInt(Num(c, "version")+1, 10), Data{"case_id": cid, "stage": "manual_update", "reason": "人工接管的问题收到新消息：" + content}, 0); e != nil {
					return e
				}
			}
			if !ev.Deleted && replyMode != "manual" && strings.TrimSpace(description) != "" {
				if e = enqueue(ctx, tx, "classify", "classify:"+cid+":"+strconv.FormatInt(Num(c, "version")+1, 10), Data{"case_id": cid, "version": Num(c, "version") + 1, "historical": historical}, 0); e != nil {
					return e
				}
			}
		}
		return finish()
	})
}

func (s *Service) classifySupport(ctx context.Context, p Data) error {
	c, e := one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(p, "case_id"))
	if e != nil {
		return e
	}
	if Num(p, "version") > 0 && Num(p, "version") != Num(c, "version") {
		return nil
	}
	if Str(c, "reply_mode") == "manual" {
		return nil
	}
	if active, e := s.supportCaseActive(ctx, c); e != nil {
		return e
	} else if !active {
		return nil
	}
	var result integration.Classification
	var usage integration.Usage
	failure := ""
	model, modelErr := s.CurrentModel(ctx)
	if modelErr != nil {
		failure = "模型配置暂不可用，已转交人工判断"
	} else if model == nil {
		failure = "尚未配置模型，已转交人工判断"
	} else {
		images, _, err := s.supportCaseImages(ctx, c)
		if err != nil {
			return err
		}
		input := boundedText(Str(c, "title")+"\n"+Str(c, "description"), 24000)
		facts, e := caseRequestFacts(ctx, s.DB, c)
		if e != nil {
			return e
		}
		input = "<agentowl_context>" + string(JSON(facts)) + "</agentowl_context>\n" + input
		input += supportImageContext(images)
		reservation, err := s.ReserveBudget(ctx, "classify:"+Str(c, "id"), int64(len(input)+len(images)*serviceVisionBudgetPerImage+8192))
		if err != nil {
			if retry, retryErr := s.shouldRetryJobError(ctx, s.DB, err); retryErr != nil {
				return retryErr
			} else if retry {
				return err
			}
			failure = "模型预算不可用，已转交人工判断"
		} else {
			result, usage, err = model.ClassifyInput(ctx, integration.ClassificationInput{Text: input, Images: images})
			if err != nil {
				_ = s.SettleBudget(ctx, reservation, 0)
				if retry, retryErr := s.shouldRetryJobError(ctx, s.DB, err); retryErr != nil {
					return retryErr
				} else if retry {
					return err
				}
				failure = classificationFailure(err)
			} else if e = s.SettleBudget(ctx, reservation, usage.TotalTokens); e != nil {
				return e
			}
		}
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		current, e := locked(ctx, tx, "cases", Str(c, "id"))
		if e != nil {
			return e
		}
		if Num(current, "version") != Num(c, "version") || Str(current, "reply_mode") == "manual" {
			return nil
		}
		if failure != "" {
			if Bool(p, "historical") {
				return caseEvent(ctx, tx, Str(c, "id"), "needs_human", "system", failure, Data{"historical": true})
			}
			return s.supportHuman(ctx, tx, current, failure, "")
		}
		module := boundedText(result.Module, 80)
		result.Reason = integration.Redact(result.Reason)
		for i := range result.MissingInfo {
			result.MissingInfo[i] = integration.Redact(result.MissingInfo[i])
		}
		if module == "" {
			module = "general"
		}
		status := Str(current, "status")
		if status == "triage" {
			status = "open"
		}
		if result.SupportRelevance == "out_of_scope" {
			status = "closed"
		}
		_, e = tx.Exec(ctx, "UPDATE cases SET support_relevance=$2,category=$3,module=$4,summary=$5,status=$6,version=version+1,updated_at=now() WHERE id=$1", Str(c, "id"), result.SupportRelevance, result.Type, module, boundedText(integration.Redact(result.Reason), 2000), status)
		if e != nil {
			return e
		}
		if e = caseEvent(ctx, tx, Str(c, "id"), "classified", "system:model", result.Reason, Data{"usage": usage, "classification": result}); e != nil {
			return e
		}
		if result.SupportRelevance == "out_of_scope" {
			return nil
		}
		current["version"] = Num(current, "version") + 1
		current["module"] = module
		if result.SupportRelevance != "in_scope" || result.NeedsHuman || result.Type == "security" {
			if Bool(p, "historical") {
				return caseEvent(ctx, tx, Str(c, "id"), "needs_human", "system", result.Reason, Data{"historical": true})
			}
			return s.supportHuman(ctx, tx, current, "需要开发者进一步判断："+result.Reason, "")
		}
		rid := ID()
		_, e = tx.Exec(ctx, "INSERT INTO replies(id,case_id,status,author_id,case_version) VALUES($1,$2,'generating','system:model',$3)", rid, Str(c, "id"), Num(current, "version"))
		if e != nil {
			return e
		}
		return enqueue(ctx, tx, "answer", "answer:"+rid, Data{"reply_id": rid, "automatic": !Bool(p, "historical"), "historical": Bool(p, "historical")}, 0)
	})
}

const serviceVisionBudgetPerImage = 4096

func classificationFailure(err error) string {
	detail := safeError(err)
	if errors.Is(err, integration.ErrInvalidModelOutput) {
		detail = "模型返回的数据格式不符合要求"
	}
	return "分类服务暂不可用，已转交人工判断：" + detail
}

func (s *Service) supportHuman(ctx context.Context, tx pgx.Tx, c Data, reason, replyID string) error {
	if e := caseEvent(ctx, tx, Str(c, "id"), "needs_human", "system", reason, Data{}); e != nil {
		return e
	}
	key := "notify:" + Str(c, "id") + ":" + strconv.FormatInt(Num(c, "version"), 10)
	payload := Data{"case_id": Str(c, "id"), "reason": integration.Redact(reason)}
	if replyID != "" {
		reply, err := one(ctx, tx, "SELECT to_jsonb(r) FROM replies r WHERE id=$1 AND case_id=$2", replyID, Str(c, "id"))
		if err != nil {
			return err
		}
		payload["reply_id"], payload["reply_version"], payload["case_version"] = replyID, Num(reply, "version"), Num(c, "version")
		key += ":reply:" + replyID + ":" + strconv.FormatInt(Num(reply, "version"), 10)
	}
	return enqueue(ctx, tx, "notify", key, payload, 0)
}

func (s *Service) answerSupport(ctx context.Context, p Data) error {
	r, e := one(ctx, s.DB, "SELECT to_jsonb(r) FROM replies r WHERE id=$1", Str(p, "reply_id"))
	if e != nil {
		return e
	}
	if Str(r, "status") != "generating" {
		return nil
	}
	c, e := one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", Str(r, "case_id"))
	if e != nil {
		return e
	}
	if Num(r, "case_version") != Num(c, "version") || Str(c, "reply_mode") == "manual" {
		return s.staleSupportReply(ctx, Str(r, "id"))
	}
	if active, e := s.supportCaseActive(ctx, c); e != nil {
		return e
	} else if !active {
		return s.staleSupportReply(ctx, Str(r, "id"))
	}
	ids := Strings(r, "evidence_ids")
	applicabilityReason := ""
	if len(ids) == 0 {
		external := true
		if ch, _, err := s.config(ctx, s.DB, Str(c, "channel_id"), "channels"); err == nil {
			external = Str(ch, "visibility") != "internal"
		} else if Str(c, "channel_id") == "" {
			external = false
		} else {
			return err
		}
		// Retrieval is restricted before any text reaches the model. A repository or
		// document source must have a recent successful permission check.
		candidates, err := many(ctx, s.DB, `SELECT to_jsonb(k) FROM knowledge k WHERE status='published' AND (valid_until IS NULL OR valid_until>now())
 AND (visibility='public' OR (NOT $1 AND (visibility='internal' OR (visibility='channel' AND $2=ANY(channel_ids)))))
 AND (source_id='' OR (permission_checked_at>now()-interval '24 hours' AND EXISTS(
 SELECT 1 FROM configs s WHERE s.id::text=k.source_id AND s.kind='knowledge-sources'
 AND s.data->>'status' IN ('synced','manual') AND COALESCE(s.data->>'enabled','true')='true'
 AND (NOT $1 OR s.data->>'visibility'='public')
 AND (COALESCE(s.data->'channel_ids','[]'::jsonb)='[]'::jsonb OR (s.data->'channel_ids') ? $2)
 AND (COALESCE(s.data->>'visibility','internal')<>'channel' OR (s.data->'channel_ids') ? $2)
 AND (s.data->>'kind'='manual' OR (s.data->>'kind'='lark' AND (s.data->'dependency_versions') ? (s.data->>'connector_id')) OR (s.data->>'kind'='repository' AND (s.data->'dependency_versions') ? (s.data->>'repository_id')))
 AND NOT EXISTS(SELECT 1 FROM jsonb_each_text(COALESCE(s.data->'dependency_versions','{}'::jsonb)) d LEFT JOIN configs dep ON dep.id::text=d.key
 WHERE dep.id IS NULL OR dep.version::text<>d.value OR (dep.kind='repositories' AND COALESCE(dep.data->>'enabled','false')<>'true') OR (dep.kind='connectors' AND (COALESCE(dep.data->>'enabled','true')<>'true' OR dep.data->>'status' IN ('disabled','revoked')))))))
 ORDER BY similarity(title||' '||left(body,4000),$3) DESC,updated_at DESC LIMIT 100`, external, Str(c, "channel_id"), boundedText(Str(c, "title")+" "+Str(c, "description"), 4000))
		if err != nil {
			return err
		}
		for _, k := range candidates {
			if err := knowledgeApplicability(k, c); err != nil {
				var detail *Error
				if errors.As(err, &detail) && (applicabilityReason == "" || strings.HasPrefix(detail.Message, "请补充")) {
					applicabilityReason = detail.Message
				}
				continue
			}
			ids = append(ids, Str(k, "id"))
			if len(ids) == 8 {
				break
			}
		}
		if len(ids) > 0 {
			applicabilityReason = ""
		}
	}
	evidence, e := s.evidence(ctx, s.DB, c, ids)
	if e != nil {
		var detail *Error
		if errors.As(e, &detail) && detail.Code == "knowledge_applicability" {
			applicabilityReason = detail.Message
			evidence = nil
		} else {
			return s.staleSupportReply(ctx, Str(r, "id"))
		}
	}
	images, imageMeta, err := s.supportCaseImages(ctx, c)
	if err != nil {
		return err
	}
	facts, err := caseRequestFacts(ctx, s.DB, c)
	if err != nil {
		return err
	}
	input := integration.AnswerInput{Question: boundedText(Str(c, "title"), 500), Context: string(JSON(Data{"request_facts": facts, "user_context": boundedText(Str(c, "description"), 24000), "confirmed_conditions": caseApplicability(c).values})) + supportImageContext(images), Images: images}
	total := len(input.Question) + len(input.Context)
	for _, k := range evidence {
		body := boundedText(Str(k, "body"), 12000)
		if total+len(body) > 64000 {
			break
		}
		input.Evidence = append(input.Evidence, integration.Evidence{ID: Str(k, "id"), Text: Str(k, "title") + "\n" + body})
		total += len(body)
	}
	var answer integration.Answer
	var usage integration.Usage
	reason := ""
	model, modelErr := s.CurrentModel(ctx)
	if applicabilityReason != "" {
		reason = applicabilityReason
	} else if modelErr != nil {
		reason = "模型配置暂不可用，已转交人工"
	} else if model == nil {
		reason = "模型尚未配置，无法自动生成回复"
	} else if len(input.Evidence) == 0 && len(images) == 0 {
		reason = "没有符合当前渠道权限且有效的知识证据或图片附件，已转交人工"
	} else {
		budget, err := s.ReserveBudget(ctx, "answer:"+Str(r, "id"), int64(total+len(images)*serviceVisionBudgetPerImage+8192))
		if err != nil {
			if retry, retryErr := s.shouldRetryJobError(ctx, s.DB, err); retryErr != nil {
				return retryErr
			} else if retry {
				return err
			}
			reason = "模型预算不可用，已转交人工"
		} else {
			answer, usage, err = model.Answer(ctx, input)
			if err != nil {
				_ = s.SettleBudget(ctx, budget, 0)
				if retry, retryErr := s.shouldRetryJobError(ctx, s.DB, err); retryErr != nil {
					return retryErr
				} else if retry {
					return err
				}
				reason = "模型未返回符合证据约束的答案，已转交人工"
			} else if e = s.SettleBudget(ctx, budget, usage.TotalTokens); e != nil {
				return e
			}
		}
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		current, e := locked(ctx, tx, "cases", Str(c, "id"))
		if e != nil {
			return e
		}
		reply, e := locked(ctx, tx, "replies", Str(r, "id"))
		if e != nil {
			return e
		}
		if Str(reply, "status") != "generating" {
			return nil
		}
		if Num(current, "version") != Num(c, "version") || Str(current, "reply_mode") == "manual" {
			_, e = tx.Exec(ctx, "UPDATE replies SET status='stale',version=version+1 WHERE id=$1", Str(r, "id"))
			return e
		}
		if reason != "" {
			if _, e = tx.Exec(ctx, "UPDATE replies SET status='failed',updated_at=now() WHERE id=$1", Str(r, "id")); e != nil {
				return e
			}
			if Bool(p, "historical") {
				return caseEvent(ctx, tx, Str(c, "id"), "needs_human", "system", reason, Data{"historical": true})
			}
			return s.supportHuman(ctx, tx, current, reason, "")
		}
		knowledgeIDs := []string{}
		imageRefs := []Data{}
		imagesByID := map[string]Data{}
		for _, meta := range imageMeta {
			if id := Str(meta, "id"); id != "" {
				imagesByID[id] = meta
			}
		}
		for _, id := range answer.CitationIDs {
			if image, ok := imagesByID[id]; ok {
				imageRefs = append(imageRefs, image)
				continue
			}
			knowledgeIDs = append(knowledgeIDs, id)
		}
		used, e := s.evidence(ctx, tx, current, knowledgeIDs)
		if e != nil {
			return e
		}
		body := boundedText(integration.Redact(answer.Text), 16000)
		// Citation links are rendered by trusted code; model-generated URLs are not
		// treated as evidence. Public knowledge must itself have been reviewed.
		if len(used) > 0 {
			body += "\n\n参考："
			for i, k := range used {
				if i > 0 {
					body += "；"
				}
				body += Str(k, "title")
				if u := Str(k, "source_url"); safeReferenceURL(u) {
					body += " " + u
				}
			}
		}
		if len(imageRefs) > 0 {
			body += "\n\n图片依据："
			for i, image := range imageRefs {
				if i > 0 {
					body += "；"
				}
				body += fallback(Str(image, "name"), "image")
			}
		}
		_, e = tx.Exec(ctx, "UPDATE replies SET body=$2,status='draft',evidence_ids=$3,evidence=$4,image_evidence=$5,version=version+1,updated_at=now() WHERE id=$1", Str(r, "id"), body, knowledgeIDs, JSON(used), JSON(imageRefs))
		if e != nil {
			return e
		}
		if e = caseEvent(ctx, tx, Str(c, "id"), "answer_generated", "system:model", "回复草稿已生成", Data{"reply_id": Str(r, "id"), "usage": usage, "disposition": answer.Disposition, "image_count": len(imageRefs)}); e != nil {
			return e
		}
		if answer.Disposition != "answer" {
			if Bool(p, "historical") {
				return caseEvent(ctx, tx, Str(c, "id"), "needs_human", "system", body, Data{"historical": true})
			}
			return s.supportHuman(ctx, tx, current, "需要补充信息或人工解答："+body, Str(r, "id"))
		}
		mode := Str(current, "reply_mode")
		auto := Bool(p, "automatic") && (mode == "auto" || mode == "mentions" && Bool(Object(current, "environment"), "mentioned_bot"))
		if !auto {
			if Bool(p, "historical") {
				return nil
			}
			return s.supportHuman(ctx, tx, current, "回复草稿待确认："+body, Str(r, "id"))
		}
		ch, co, e := s.caseReplyChannel(ctx, tx, current)
		if e != nil {
			return e
		}
		if !Bool(ch, "enabled") {
			return nil
		}
		if connectorDisabled(co) {
			return nil
		}
		action := Data{"reply_id": Str(r, "id"), "reply_version": Num(reply, "version") + 1, "case_id": Str(c, "id"), "case_version": Num(c, "version"), "channel_id": Str(ch, "id"), "channel_version": Num(ch, "version"), "connector_id": Str(co, "id"), "connector_version": Num(co, "version"), "body": body, "evidence": used, "image_evidence": imageMeta, "manual": false, "answer_verified": true, "max_tokens": usage.TotalTokens, "timeout_seconds": 90, "file_count": 0}
		if Bool(ch, "private") {
			action["private_channel"], action["external_id"], action["private_actor_id"] = true, Str(ch, "external_id"), Str(ch, "private_actor_id")
		}
		ap, e := s.createApproval(ctx, tx, Actor{ID: "system:model"}, "reply_send", "reply", Str(r, "id"), "发送回复："+Str(c, "title"), action)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, "UPDATE replies SET status='pending',approval_id=$2,case_version=$3 WHERE id=$1", Str(r, "id"), Str(ap, "id"), Num(c, "version"))
		return e
	})
}
func safeReferenceURL(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func (s *Service) staleSupportReply(ctx context.Context, id string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, "UPDATE replies SET status='stale',version=version+1 WHERE id=$1 AND status='generating'", id)
		return e
	})
}

func (s *Service) verifySupportConnector(ctx context.Context, p Data) error {
	id := Str(p, "connector_id")
	c, _, e := s.config(ctx, s.DB, id, "connectors")
	if e != nil {
		return e
	}
	if Num(p, "version") > 0 && Num(p, "version") != Num(c, "version") {
		return nil
	}
	if connectorDisabled(c) {
		return nil
	}
	result, snapshots, e := s.probeConnectorCapabilities(ctx, c)
	if e != nil {
		return e
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		raw, e := locked(ctx, tx, "configs", id)
		if e != nil {
			return e
		}
		if Num(raw, "version") != Num(c, "version") {
			return nil
		}
		for channelID, version := range snapshots {
			channel, _, err := s.config(ctx, tx, channelID, "channels")
			if err != nil || Num(channel, "version") != version {
				return Conflict
			}
		}
		// Live receipts/sends may finish while these read-only probes run. Keep
		// their newer scoped observations rather than overwrite them with a snapshot.
		currentCapabilities := capabilityMatrix(configPublic(raw))
		for _, key := range []string{"receive_mentions", "receive_messages", "attachment_read", "send", "card_callback"} {
			if Str(Object(currentCapabilities, key), "status") == "observed" {
				Object(result, "capabilities")[key] = currentCapabilities[key]
			}
		}
		_, e = tx.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb,updated_at=now() WHERE id=$1", id, JSON(result))
		return e
	})
}

func (s *Service) syncSupportChannel(ctx context.Context, p Data) error {
	id := Str(p, "channel_id")
	ch, _, e := s.config(ctx, s.DB, id, "channels")
	if e != nil {
		return e
	}
	if !Bool(ch, "enabled") || Num(p, "version") > 0 && Num(p, "version") != Num(ch, "version") {
		return nil
	}
	co, _, e := s.config(ctx, s.DB, Str(ch, "connector_id"), "connectors")
	if e != nil {
		return e
	}
	if connectorDisabled(co) {
		return nil
	}
	if Num(p, "page_count") >= 200 {
		return Fail(422, "history_limit", "历史同步达到分页上限，请缩小同步范围")
	}
	if Str(p, "sync_id") == "" {
		p["sync_id"] = ID()
		p["end_time"] = time.Now().UTC().Format(time.RFC3339Nano)
		if last := Str(ch, "last_synced_at"); last != "" {
			if t, err := time.Parse(time.RFC3339Nano, last); err == nil {
				p["start_time"] = t.Add(-5 * time.Minute).Format(time.RFC3339Nano)
			}
		}
	}
	events := []integration.Event{}
	next := Data{}
	children := []Data{}
	adapter, _, e := s.sourceAdapter(ctx, s.DB, co)
	if e != nil {
		return e
	}
	repository := fallback(Str(co, "repository"), Str(co, "owner")+"/"+Str(co, "repo"))
	if Str(co, "repository_id") != "" {
		repo, _, err := s.config(ctx, s.DB, Str(co, "repository_id"), "repositories")
		if err != nil {
			return err
		}
		repository = Str(repo, "owner") + "/" + Str(repo, "repo")
	}
	thread := Str(p, "thread_id")
	if Num(p, "issue_number") > 0 {
		thread = fmt.Sprintf("issue:%d", Num(p, "issue_number"))
	}
	cursor := Str(p, "cursor")
	if cursor == "" && Num(p, "page") > 0 {
		cursor = fmt.Sprint(Num(p, "page"))
	}
	start, _ := time.Parse(time.RFC3339Nano, Str(p, "start_time"))
	end, _ := time.Parse(time.RFC3339Nano, Str(p, "end_time"))
	page, err := adapter.ReadThread(ctx, integration.SourceRead{ChannelID: Str(ch, "external_id"), ThreadID: thread, Cursor: cursor, Repository: repository, Since: start, Until: end})
	if err != nil {
		return err
	}
	for _, ev := range page.Events {
		ev.AppID = Str(co, "app_id")
		ev.TenantID = Str(co, "tenant_id")
		events = append(events, ev)
	}
	for _, ref := range page.Threads {
		child := copyData(p)
		child["thread_id"] = ref
		child["cursor"] = ""
		child["page_count"] = 0
		delete(child, "page")
		children = append(children, child)
	}
	if page.NextCursor != "" {
		next = copyData(p)
		next["cursor"] = page.NextCursor
		delete(next, "page")
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		raw, e := locked(ctx, tx, "configs", Str(ch, "id"))
		if e != nil {
			return e
		}
		if Num(raw, "version") != Num(ch, "version") || !Bool(Object(raw, "data"), "enabled") {
			return nil
		}
		currentConnector, _, e := s.config(ctx, tx, Str(co, "id"), "connectors")
		if e != nil {
			return e
		}
		if Num(currentConnector, "version") != Num(co, "version") || connectorDisabled(currentConnector) {
			return Conflict
		}
		for _, ev := range events {
			if _, _, e = s.persistEvent(ctx, tx, Str(co, "id"), ch, ev); e != nil {
				return e
			}
		}
		for _, child := range children {
			key := fmt.Sprintf("sync:%s:%s:%s:%d", Str(p, "sync_id"), Str(ch, "id"), Str(child, "thread_id"), Num(child, "issue_number"))
			if e = enqueue(ctx, tx, "sync_channel", key, child, 0); e != nil {
				return e
			}
		}
		if len(next) > 0 {
			next["page_count"] = Num(p, "page_count") + 1
			return enqueue(ctx, tx, "sync_channel", "sync-page:"+Hash(JSON(next)), next, 0)
		}
		// Root pagination completion is a cursor watermark, not proof that every
		// thread/comment child has completed. Child jobs retain their own failures.
		patch := Data{"last_page_synced_at": time.Now().UTC(), "last_sync_error": ""}
		if !Bool(p, "periodic") && Str(p, "thread_id") == "" && Num(p, "issue_number") == 0 {
			patch["last_synced_at"] = Str(p, "end_time")
		}
		_, e = tx.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb,updated_at=now() WHERE id=$1", Str(ch, "id"), JSON(patch))
		return e
	})
}
func copyData(m Data) Data {
	out := Data{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

type sourceDocument struct{ key, title, body, revision, url string }

func (s *Service) syncSupportSource(ctx context.Context, p Data) error {
	id := Str(p, "source_id")
	src, _, e := s.config(ctx, s.DB, id, "knowledge-sources")
	if e != nil {
		return e
	}
	if Str(src, "status") == "revoked" || Num(p, "version") > 0 && Num(p, "version") != Num(src, "version") {
		return nil
	}
	if enabled, ok := src["enabled"].(bool); ok && !enabled {
		return nil
	}
	docs := []sourceDocument{}
	dependencies := Data{}
	var failure error
	switch Str(src, "kind") {
	case "manual":
	case "repository":
		cli, repo, err := s.supportGitHub(ctx, s.DB, Str(src, "repository_id"), "repositories")
		failure = err
		if err == nil {
			dependencies[Str(repo, "id")] = Num(repo, "version")
			if cid := Str(repo, "connector_id"); cid != "" {
				co, _, e := s.config(ctx, s.DB, cid, "connectors")
				if e != nil {
					failure = e
				} else if connectorDisabled(co) {
					failure = Fail(409, "connector_disabled", "知识连接器已停用")
				} else {
					dependencies[cid] = Num(co, "version")
				}
			}
		}
		if err == nil && !Bool(repo, "enabled") {
			failure = Fail(409, "repository_disabled", "仓库未启用")
		}
		paths := Strings(src, "paths")
		if len(paths) == 0 && Str(src, "path") != "" {
			paths = []string{Str(src, "path")}
		}
		if failure == nil && len(paths) == 0 {
			failure = Fail(422, "source_paths_required", "请配置需要同步的 Markdown 路径")
		}
		for _, v := range paths {
			if path.Clean(v) != v || strings.HasPrefix(v, "/") || v == ".." || strings.HasPrefix(v, "../") {
				failure = Fail(422, "invalid_path", "知识路径无效")
			}
		}
		if failure == nil {
			ref, err := cli.GetRef(ctx, "heads/"+fallback(Str(src, "ref"), fallback(Str(repo, "base_branch"), "main")))
			failure = err
			if err == nil {
				commit, err := cli.GetCommit(ctx, ref.Object.SHA)
				failure = err
				if err == nil {
					tree, err := cli.GetTree(ctx, commit.Tree.SHA, true)
					failure = err
					if err == nil {
						total := 0
						for _, entry := range tree.Tree {
							if entry.Type != "blob" || !contains([]string{"100644", "100755"}, entry.Mode) || !pathAllowed(entry.Path, paths) || !contains([]string{".md", ".markdown"}, strings.ToLower(path.Ext(entry.Path))) {
								continue
							}
							if len(docs) >= 100 || entry.Size > 200000 || total+int(entry.Size) > 2<<20 {
								failure = Fail(422, "source_limit", "知识来源超出文件数或大小限制")
								break
							}
							body, err := cli.GetBlob(ctx, entry.SHA)
							if err != nil {
								failure = err
								break
							}
							if !utf8.Valid(body) {
								failure = Fail(422, "source_encoding", "知识文件必须为 UTF-8")
								break
							}
							total += len(body)
							docs = append(docs, sourceDocument{key: entry.Path, title: entry.Path, body: integration.Redact(string(body)), revision: ref.Object.SHA + ":" + entry.SHA, url: "https://github.com/" + Str(repo, "owner") + "/" + Str(repo, "repo") + "/blob/" + ref.Object.SHA + "/" + escapedPath(entry.Path)})
						}
					}
				}
			}
		}
	case "lark":
		cli, c, err := s.supportLark(ctx, s.DB, Str(src, "connector_id"))
		failure = err
		if err == nil {
			dependencies[Str(c, "id")] = Num(c, "version")
		}
		if err == nil && connectorDisabled(c) {
			failure = Fail(409, "connector_disabled", "连接器已停用")
		}
		if failure == nil {
			documentID, err := cli.ResolveDocumentID(ctx, Str(src, "document_id"), Str(src, "url"))
			failure = err
			if failure == nil {
				doc, err := cli.ReadDocument(ctx, documentID)
				failure = err
				if err == nil {
					if len(doc.Content) > 200000 {
						failure = Fail(422, "source_limit", "文档正文超出限制")
					} else {
						docs = append(docs, sourceDocument{key: documentID, title: doc.Title, body: doc.Content, revision: strconv.FormatInt(doc.RevisionID, 10), url: Str(src, "url")})
					}
				}
			}
		}

	default:
		failure = Fail(422, "invalid_source", "知识来源类型无效")
	}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		raw, e := locked(ctx, tx, "configs", id)
		if e != nil {
			return e
		}
		if Num(raw, "version") != Num(src, "version") || Str(Object(raw, "data"), "status") == "revoked" {
			return nil
		}
		dependencyIDs := []string{}
		for dependency := range dependencies {
			dependencyIDs = append(dependencyIDs, dependency)
		}
		sort.Strings(dependencyIDs)
		for _, dependency := range dependencyIDs {
			current, e := locked(ctx, tx, "configs", dependency)
			if e != nil {
				return e
			}
			if Num(current, "version") != Num(dependencies, dependency) {
				return Conflict
			}
		}
		if failure != nil {
			if _, e = tx.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb,updated_at=now() WHERE id=$1", id, JSON(Data{"status": "failed", "last_error": safeError(failure), "last_attempt_at": time.Now().UTC()})); e != nil {
				return e
			}
			return s.withdrawSource(ctx, tx, id)
		}
		if Str(src, "kind") != "manual" {
			existing, e := many(ctx, tx, "SELECT to_jsonb(k) FROM knowledge k WHERE source_id=$1 FOR UPDATE", id)
			if e != nil {
				return e
			}
			seen := map[string]bool{}
			for _, doc := range docs {
				doc.title = integration.Redact(doc.title)
				if strings.TrimSpace(doc.body) == "" {
					continue
				}
				var old Data
				for _, k := range existing {
					if Str(k, "source_url") == doc.url || Str(k, "title") == doc.title {
						old = k
						break
					}
				}
				visibility := fallback(Str(src, "visibility"), "internal")
				if !contains([]string{"public", "internal", "channel"}, visibility) {
					return Fail(422, "source_visibility", "知识来源可见范围无效")
				}
				kid := Str(old, "id")
				if kid == "" {
					kid = ID()
					_, e = tx.Exec(ctx, "INSERT INTO knowledge(id,title,body,module,visibility,channel_ids,source_url,source_revision,source_id,author_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'system:sync')", kid, doc.title, doc.body, fallback(Str(src, "module"), "general"), visibility, Strings(src, "channel_ids"), doc.url, doc.revision, id)
				} else if Str(old, "status") != "withdrawn" && Str(old, "source_revision") == doc.revision && Str(old, "body") == doc.body && Str(old, "visibility") == visibility && Hash(JSON(Strings(old, "channel_ids"))) == Hash(JSON(Strings(src, "channel_ids"))) {
					_, e = tx.Exec(ctx, "UPDATE knowledge SET permission_checked_at=now() WHERE id=$1", kid)
				} else {
					_, e = tx.Exec(ctx, "UPDATE knowledge SET title=$2,body=$3,source_url=$4,source_revision=$5,visibility=$6,channel_ids=$7,status='draft',permission_checked_at=now(),version=version+1,updated_at=now() WHERE id=$1", kid, doc.title, doc.body, doc.url, doc.revision, visibility, Strings(src, "channel_ids"))
					if e == nil {
						e = s.invalidateKnowledge(ctx, tx, kid)
					}
				}
				if e != nil {
					return e
				}
				seen[kid] = true
				if _, e = tx.Exec(ctx, "INSERT INTO knowledge_versions(knowledge_id,version,snapshot) SELECT id,version,to_jsonb(k) FROM knowledge k WHERE id=$1 ON CONFLICT DO NOTHING", kid); e != nil {
					return e
				}
			}
			for _, k := range existing {
				if seen[Str(k, "id")] {
					continue
				}
				if _, e = tx.Exec(ctx, "UPDATE knowledge SET status='withdrawn',version=version+1,updated_at=now() WHERE id=$1", Str(k, "id")); e != nil {
					return e
				}
				if e = s.invalidateKnowledge(ctx, tx, Str(k, "id")); e != nil {
					return e
				}
			}
		} else {
			if _, e = tx.Exec(ctx, "UPDATE knowledge SET permission_checked_at=now() WHERE source_id=$1", id); e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, "UPDATE configs SET data=data||$2::jsonb,updated_at=now() WHERE id=$1", id, JSON(Data{"status": "synced", "last_synced_at": time.Now().UTC(), "last_attempt_at": time.Now().UTC(), "last_error": "", "document_count": len(docs), "dependency_versions": dependencies}))
		return e
	})
	if err != nil {
		return err
	}
	return failure
}
func escapedPath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
func (s *Service) withdrawSource(ctx context.Context, tx pgx.Tx, id string) error {
	rows, e := many(ctx, tx, "SELECT to_jsonb(k) FROM knowledge k WHERE source_id=$1", id)
	if e != nil {
		return e
	}
	for _, k := range rows {
		if _, e = tx.Exec(ctx, "UPDATE knowledge SET status='withdrawn',version=version+1,updated_at=now() WHERE id=$1 AND status<>'withdrawn'", Str(k, "id")); e != nil {
			return e
		}
		if e = s.invalidateKnowledge(ctx, tx, Str(k, "id")); e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) queueSupportNotification(ctx context.Context, p Data) error {
	if Str(p, "approval_id") != "" {
		return s.queueApprovalNotification(ctx, p)
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		c, e := locked(ctx, tx, "cases", Str(p, "case_id"))
		if e != nil {
			return e
		}
		if contains([]string{"closed", "resolved"}, Str(c, "status")) || Str(p, "reply_id") != "" && Num(p, "case_version") != Num(c, "version") {
			return nil
		}
		stage := fallback(Str(p, "stage"), "initial")
		if stage == "claim_timeout" && contains([]string{"in_progress", "waiting_fix", "waiting_release"}, Str(c, "status")) {
			return nil
		}
		routes, e := many(ctx, tx, "SELECT to_jsonb(c) FROM configs c WHERE kind='owner-routes' AND data->>'enabled'='true' AND data->>'module' IN ($1,'*') ORDER BY (data->>'module'=$1) DESC,id", Str(c, "module"))
		if e != nil {
			return e
		}
		if len(routes) == 0 {
			return caseEvent(ctx, tx, Str(c, "id"), "notification_unrouted", "system", "未配置此模块的 Oncall 路由，请在控制台认领", Data{})
		}
		route := configPublic(routes[0])
		owner := Str(route, "primary_id")
		if stage != "initial" && stage != "manual_update" && Str(route, "backup_id") != "" {
			owner = Str(route, "backup_id")
		}
		principal, e := one(ctx, tx, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1 AND active", owner)
		if e != nil {
			return caseEvent(ctx, tx, Str(c, "id"), "notification_unrouted", "system", "Oncall 路由中的处理人未启用", Data{})
		}
		if !actorFrom(principal).Has("oncall", "developer") {
			return caseEvent(ctx, tx, Str(c, "id"), "notification_unrouted", "system", "Oncall 路由处理人没有处理问题的角色", Data{})
		}
		if Str(c, "assignee_id") == "" && Str(p, "approval_id") == "" {
			if _, e = tx.Exec(ctx, "UPDATE cases SET assignee_id=$2,version=version+1,updated_at=now() WHERE id=$1", Str(c, "id"), owner); e != nil {
				return e
			}
			c["version"] = Num(c, "version") + 1
			c["assignee_id"] = owner
		}
		channelID := Str(route, "channel_id")
		ch, _, e := s.config(ctx, tx, channelID, "channels")
		if e != nil || !Bool(ch, "enabled") || Str(ch, "visibility") != "internal" {
			return caseEvent(ctx, tx, Str(c, "id"), "notification_unrouted", "system", "Oncall 通知需要启用的内部渠道", Data{})
		}
		co, _, e := s.config(ctx, tx, Str(ch, "connector_id"), "connectors")
		if e != nil {
			return e
		}
		if connectorDisabled(co) {
			return caseEvent(ctx, tx, Str(c, "id"), "notification_unrouted", "system", "Oncall 通知连接器已停用", Data{})
		}
		body := boundedText("AgentOwl · "+Str(c, "title")+"\n负责人："+Str(principal, "name")+"\n"+fallback(Str(p, "reason"), "问题尚未完成，请关注处理进度"), 2500)
		switch stage {
		case "claim_timeout", "resolve_timeout":
			body = "处理超时提醒\n" + body
		case "manual_update":
			body = "人工接管问题收到新消息\n" + body
		}
		actions := Data{"claim": Data{"case_id": Str(c, "id"), "expected_version": Num(c, "version")}, "takeover": Data{"case_id": Str(c, "id"), "expected_version": Num(c, "version")}}

		replyID := Str(p, "reply_id")
		if replyID != "" {
			reply, err := one(ctx, tx, "SELECT to_jsonb(r) FROM replies r WHERE id=$1 AND case_id=$2", replyID, Str(c, "id"))
			if err != nil {
				return err
			}
			if Str(reply, "status") != "draft" || Num(reply, "version") != Num(p, "reply_version") {
				return nil
			}
			actions["send_reply"] = Data{"reply_id": replyID, "expected_version": Num(reply, "version"), "case_version": Num(c, "version")}
		}
		opID := ID()
		request := Data{"case_id": Str(c, "id"), "case_version": Num(c, "version"), "body": integration.Redact(body), "card_nonce": Token(), "card_actions": actions}
		if replyID != "" {
			request["case_url"] = strings.TrimRight(s.Config.PublicURL, "/") + "/cases/" + Str(c, "id")
		}
		target := Data{"channel_id": channelID, "channel_version": Num(ch, "version"), "connector_id": Str(co, "id"), "connector_version": Num(co, "version"), "external_id": Str(ch, "external_id")}
		key := fmt.Sprintf("notify:%s:%d:%s:%s", Str(c, "id"), Num(c, "version"), stage, Str(p, "approval_id"))
		if replyID != "" {
			key += ":reply:" + replyID + ":" + strconv.FormatInt(Num(p, "reply_version"), 10)
		}
		var inserted string
		e = tx.QueryRow(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request) VALUES($1,'notify',$2,$3,$4,$5) ON CONFLICT(dedupe_key) DO NOTHING RETURNING id", opID, Str(c, "id"), key, JSON(target), JSON(request)).Scan(&inserted)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if e = enqueue(ctx, tx, "dispatch_external", "dispatch:"+opID, Data{"operation_id": opID}, 0); e != nil {
			return e
		}
		if stage == "initial" && Str(p, "approval_id") == "" {
			for _, timeout := range []struct{ stage, key string }{{"claim_timeout", "claim_minutes"}, {"resolve_timeout", "resolve_minutes"}} {
				delay := time.Duration(Num(route, timeout.key)) * time.Minute
				if delay <= 0 {
					continue
				}
				if e = enqueue(ctx, tx, "notify", "notify-timeout:"+Str(c, "id")+":"+timeout.stage, Data{"case_id": Str(c, "id"), "stage": timeout.stage}, delay); e != nil {
					return e
				}
			}
		}
		return caseEvent(ctx, tx, Str(c, "id"), "notification_queued", "system", "已加入 Oncall 通知队列", Data{"operation_id": opID, "stage": stage, "owner_id": owner})
	})
}

// DispatchMessage performs one remote attempt and never changes operation state.
// The caller owns approval validation, leasing, compare-and-swap and reconcile.
func (s *Service) DispatchMessage(ctx context.Context, op Data) (Data, error) {
	kind := Str(op, "kind")
	if kind != "reply_send" && kind != "notify" && kind != "read_mark" {
		return nil, integration.ErrUnsupportedOperation
	}
	allowed, err := s.approvalNotificationAllowed(ctx, s.DB, op)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, Fail(409, "notification_cancelled", "审批通知已关闭或不再适用")
	}
	request, target := Object(op, "request"), Object(op, "target")
	ch, co, e := s.operationChannel(ctx, s.DB, op)
	if e != nil {
		return nil, e
	}
	channelID := Str(ch, "id")
	if channelID == "" {
		return nil, Fail(409, "missing_channel", "消息缺少 chat_id 渠道信息")
	}
	if !Bool(ch, "enabled") {
		return nil, Fail(409, "channel_disabled", "渠道已停用")
	}
	if n := Num(target, "channel_version"); n > 0 && n != Num(ch, "version") {
		return nil, Conflict
	}
	if n := Num(request, "channel_version"); n > 0 && n != Num(ch, "version") {
		return nil, Conflict
	}
	if external := Str(target, "external_id"); external != "" && external != Str(ch, "external_id") {
		return nil, Conflict
	}
	if cid := Str(target, "connector_id"); cid != "" && cid != Str(co, "id") {
		return nil, Conflict
	}
	if connectorDisabled(co) {
		return nil, Fail(409, "connector_disabled", "连接器已停用")
	}
	if n := Num(target, "connector_version"); n > 0 && n != Num(co, "version") {
		return nil, Conflict
	}
	if n := Num(request, "connector_version"); n > 0 && n != Num(co, "version") {
		return nil, Conflict
	}
	body := ""
	if kind != "read_mark" {
		body = integration.Redact(Str(request, "body"))
		if strings.TrimSpace(body) == "" || len(body) > 24000 {
			return nil, Fail(422, "invalid_message", "消息正文为空或过长")
		}
	}
	if !validID(Str(op, "id")) {
		return nil, Fail(422, "invalid_operation", "外部操作标识无效")
	}
	var c Data
	if cid := Str(request, "case_id"); cid != "" {
		c, e = one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", cid)
		if e != nil {
			return nil, e
		}
		if kind == "reply_send" && Str(c, "channel_id") != channelID && !(Bool(ch, "private") && Str(c, "channel_id") == "") {
			return nil, Forbidden
		}
		if kind == "notify" && Num(request, "case_version") > 0 && Num(request, "case_version") != Num(c, "version") {
			return nil, Conflict
		}
	}
	if kind == "notify" && Str(ch, "visibility") != "internal" {
		return nil, Forbidden
	}
	adapter, current, e := s.sourceAdapter(ctx, s.DB, co)
	if e != nil {
		return nil, e
	}
	if Num(current, "version") != Num(co, "version") {
		return nil, Conflict
	}
	write := sourceWrite(op, ch, co, c)
	if Str(co, "provider") == "github" && write.Issue <= 0 {
		return nil, Fail(422, "missing_issue", "未配置目标 GitHub Issue")
	}
	if kind == "reply_send" && Str(co, "provider") != "github" && write.ReplyTo == "" && Str(c, "source") != "manual" {
		return nil, Fail(409, "missing_source", "无法定位原始消息")
	}
	if Str(op, "approval_id") != "" {
		if e = s.validateExternal(ctx, s.DB, op); e != nil {
			return nil, e
		}
	}
	sent, e := adapter.Send(ctx, write)
	if e != nil {
		return nil, e
	}
	if kind != "read_mark" && !Bool(ch, "private") {
		_ = s.observeCapabilityVersion(ctx, Str(co, "id"), channelID, "send", Num(co, "version"), Num(ch, "version"))
	}
	response := decodeData(sent)
	response["provider"] = Str(co, "provider")
	response["message_id"] = sent.RemoteID
	if kind == "read_mark" {
		response["reaction_id"] = sent.RemoteID
	}
	return Data{"remote_id": sent.RemoteID, "response": response}, nil
}

func notificationCard(operationID string, request Data) json.RawMessage {
	buttons := []any{}
	labels := map[string]string{"claim": "认领", "takeover": "人工接管", "resume": "恢复辅助", "approve": "批准", "reject": "拒绝", "revoke": "撤销批准", "confirm_development": "确认开发", "send_reply": "批准并发送"}
	actions := Object(request, "card_actions")
	for _, name := range []string{"claim", "takeover", "resume", "approve", "reject", "revoke", "confirm_development", "send_reply"} {
		if len(Object(actions, name)) == 0 {
			continue
		}
		buttons = append(buttons, Data{"tag": "button", "text": Data{"tag": "plain_text", "content": labels[name]}, "type": "default", "value": Data{"operation_id": operationID, "nonce": Str(request, "card_nonce"), "action": name}})
	}
	if link := Str(request, "case_url"); link != "" {
		buttons = append(buttons, Data{"tag": "button", "text": Data{"tag": "plain_text", "content": "查看草稿"}, "type": "default", "url": link})
	} else if link := Str(request, "approval_url"); link != "" {
		buttons = append(buttons, Data{"tag": "button", "text": Data{"tag": "plain_text", "content": "查看详情"}, "type": "default", "url": link})
	}
	elements := []any{Data{"tag": "div", "text": Data{"tag": "plain_text", "content": Str(request, "body")}}}
	if len(buttons) > 0 {
		elements = append(elements, Data{"tag": "action", "actions": buttons})
	}
	return JSON(Data{"config": Data{"wide_screen_mode": true}, "elements": elements})
}

func notificationResultCard(op Data, action string, duplicate bool) json.RawMessage {
	labels := map[string]string{"claim": "已认领", "takeover": "已接管", "resume": "已恢复辅助", "approve": "已批准", "reject": "已拒绝", "revoke": "已撤销批准", "confirm_development": "已确认开发", "send_reply": "已批准回复并加入发送队列"}
	note := labels[action]
	if note == "" {
		note = "操作已记录"
	}
	if duplicate {
		note = "此操作已接收，无需重复点击。"
	} else {
		note += "。按钮已停用。"
	}
	request := Object(op, "request")
	if action == "claim" && Str(request, "reply_approval_id") == "" && len(Object(Object(request, "card_actions"), "send_reply")) > 0 {
		return notificationCard(Str(op, "id"), Data{"body": Str(request, "body") + "\n已认领", "card_nonce": Str(request, "card_nonce"), "case_url": Str(request, "case_url"), "card_actions": Data{"send_reply": Object(Object(request, "card_actions"), "send_reply")}})
	}
	return notificationCard(Str(op, "id"), Data{"body": Str(request, "body") + "\n" + note, "approval_url": fallback(Str(request, "case_url"), Str(request, "approval_url"))})
}

func (s *Service) supportCaseActive(ctx context.Context, c Data) (bool, error) {
	if Str(c, "channel_id") == "" {
		return true, nil
	}
	ch, _, e := s.config(ctx, s.DB, Str(c, "channel_id"), "channels")
	if e != nil {
		return false, e
	}
	if !Bool(ch, "enabled") {
		return false, nil
	}
	co, _, e := s.config(ctx, s.DB, Str(ch, "connector_id"), "connectors")
	if e != nil {
		return false, e
	}
	return !connectorDisabled(co), nil
}

// Knowledge cannot widen its source audience. A successful source sync records
// the exact credential/repository configurations whose access was verified.
func (s *Service) sourceAudience(ctx context.Context, q DB, src Data, channelID string, external bool) error {
	if enabled, ok := src["enabled"].(bool); ok && !enabled {
		return Fail(409, "source_disabled", "知识来源已停用")
	}
	visibility := fallback(Str(src, "visibility"), "internal")
	if !contains([]string{"public", "internal", "channel"}, visibility) {
		return Forbidden
	}
	if external && visibility != "public" {
		return Forbidden
	}
	ids := Strings(src, "channel_ids")
	if visibility == "channel" && !contains(ids, channelID) || len(ids) > 0 && !contains(ids, channelID) {
		return Forbidden
	}
	dependencies := Object(src, "dependency_versions")
	check := func(id, kind string) (Data, error) {
		m, _, e := s.config(ctx, q, id, kind)
		if e != nil {
			return nil, Fail(409, "stale_source", "知识来源的连接配置不可访问")
		}
		if Num(dependencies, id) != Num(m, "version") {
			return nil, Fail(409, "stale_source", "知识连接配置已改变，需要重新同步")
		}
		if kind == "repositories" && !Bool(m, "enabled") || kind == "connectors" && connectorDisabled(m) {
			return nil, Fail(409, "source_disabled", "知识来源的连接配置已停用")
		}
		return m, nil
	}
	switch Str(src, "kind") {
	case "manual":
		return nil
	case "lark":
		_, e := check(Str(src, "connector_id"), "connectors")
		return e
	case "repository":
		repo, e := check(Str(src, "repository_id"), "repositories")
		if e != nil {
			return e
		}
		if cid := Str(repo, "connector_id"); cid != "" {
			_, e = check(cid, "connectors")
		}
		return e
	default:
		return Fail(409, "stale_source", "未知知识来源类型")
	}
}

// MaintainSupport is safe to call from multiple workers. Hourly source buckets
// and per-case SLA keys are durable dedupe keys, so a restart does not spam users.
func (s *Service) MaintainSupport(ctx context.Context) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		sources, e := many(ctx, tx, "SELECT to_jsonb(c) FROM configs c WHERE kind='knowledge-sources' AND COALESCE(data->>'status','pending')<>'revoked' AND COALESCE(data->>'enabled','true')='true' ORDER BY COALESCE(data->>'last_attempt_at',data->>'last_synced_at',''),id LIMIT 1000")
		if e != nil {
			return e
		}
		bucket := time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
		for _, raw := range sources {
			source := configPublic(raw)
			last, err := time.Parse(time.RFC3339Nano, Str(source, "last_synced_at"))
			if err == nil && time.Since(last) < time.Hour {
				continue
			}
			id := Str(source, "id")
			if e = enqueue(ctx, tx, "sync_source", "source-periodic:"+id+":"+bucket, Data{"source_id": id, "version": Num(source, "version")}, 0); e != nil {
				return e
			}
		}
		cases, e := many(ctx, tx, "SELECT to_jsonb(c) FROM cases c WHERE status NOT IN ('resolved','closed') AND support_relevance<>'out_of_scope' AND COALESCE(environment->>'historical','false')<>'true' ORDER BY created_at LIMIT 1000")
		if e != nil {
			return e
		}
		routes, e := many(ctx, tx, "SELECT to_jsonb(c) FROM configs c WHERE kind='owner-routes' AND data->>'enabled'='true'")
		if e != nil {
			return e
		}
		for _, c := range cases {
			var route Data
			for _, raw := range routes {
				r := configPublic(raw)
				if Str(r, "module") == Str(c, "module") {
					route = r
					break
				}
				if Str(r, "module") == "*" {
					route = r
				}
			}
			if route == nil {
				continue
			}
			created, err := time.Parse(time.RFC3339Nano, Str(c, "created_at"))
			if err != nil {
				continue
			}
			age := time.Since(created)
			for _, deadline := range []struct{ stage, key string }{{"claim_timeout", "claim_minutes"}, {"resolve_timeout", "resolve_minutes"}} {
				if deadline.stage == "claim_timeout" && contains([]string{"in_progress", "waiting_fix", "waiting_release"}, Str(c, "status")) {
					continue
				}
				minutes := Num(route, deadline.key)
				if minutes <= 0 || age < time.Duration(minutes)*time.Minute {
					continue
				}
				if e = enqueue(ctx, tx, "notify", "notify-timeout:"+Str(c, "id")+":"+deadline.stage, Data{"case_id": Str(c, "id"), "stage": deadline.stage}, 0); e != nil {
					return e
				}
			}
		}
		return nil
	})
}

// ReconcileMessage only searches for positive evidence. A missing match is not
// permission to replay a non-idempotent write, even after a complete scan.
func (s *Service) ReconcileMessage(ctx context.Context, op Data) (Data, bool, error) {
	req, target := Object(op, "request"), Object(op, "target")
	channelID := fallback(Str(target, "channel_id"), Str(req, "channel_id"))
	ch, _, e := s.config(ctx, s.DB, channelID, "channels")
	if e != nil {
		return nil, false, e
	}
	if !Bool(ch, "enabled") {
		return nil, false, Fail(409, "channel_disabled", "渠道已停用")
	}
	co, _, e := s.config(ctx, s.DB, Str(ch, "connector_id"), "connectors")
	if e != nil {
		return nil, false, e
	}
	if connectorDisabled(co) {
		return nil, false, Fail(409, "connector_disabled", "连接器已停用")
	}
	if n := Num(target, "channel_version"); n > 0 && n != Num(ch, "version") {
		return nil, false, Conflict
	}
	if x := Str(target, "external_id"); x != "" && x != Str(ch, "external_id") {
		return nil, false, Conflict
	}
	adapter, _, e := s.sourceAdapter(ctx, s.DB, co)
	if e != nil {
		return nil, false, e
	}
	c := Data{}
	if id := Str(req, "case_id"); id != "" {
		c, e = one(ctx, s.DB, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", id)
		if e != nil {
			return nil, false, e
		}
	}
	write := sourceWrite(op, ch, co, c)
	if n := int(Num(Object(op, "response"), "issue_number")); n > 0 {
		write.Issue = n
	}
	result, found, e := adapter.Reconcile(ctx, write, integration.SourceResult{RemoteID: Str(op, "remote_id")})
	if e != nil {
		return nil, false, e
	}
	response := decodeData(result)
	response["reconciled"] = true
	return Data{"remote_id": result.RemoteID, "response": response}, found, nil
}
