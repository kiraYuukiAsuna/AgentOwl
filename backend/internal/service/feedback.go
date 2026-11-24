package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

var feedbackLabels = map[string]string{"resolved": "用户确认已解决", "unresolved": "用户反馈未解决，等待继续排查", "human": "用户请求转人工，已暂停自动回复"}

func feedbackCardDocument(body string, elements ...any) json.RawMessage {
	items := []any{Data{"tag": "div", "text": Data{"tag": "lark_md", "content": integration.Redact(body)}}}
	items = append(items, elements...)
	return JSON(Data{"config": Data{"wide_screen_mode": true}, "elements": items})
}

// bindReplyFeedback freezes server-owned context before the remote send. A card
// carries only an operation ID, nonce and one of three low-privilege actions.
func (s *Service) bindReplyFeedback(ctx context.Context, tx pgx.Tx, request, reply, action Data) error {
	c, e := locked(ctx, tx, "cases", Str(reply, "case_id"))
	if e != nil {
		return e
	}
	env := Object(c, "environment")
	// A manually entered case has no platform participant whose feedback can be
	// authenticated. Its replies remain plain messages; authorized Web users can
	// still submit feedback through the normal role/ACL protected endpoint.
	if Str(env, "last_message_id") == "" && Str(c, "source") == "manual" {
		return nil
	}
	ch, co, e := s.caseReplyChannel(ctx, tx, c)
	if e != nil {
		return e
	}
	if Num(c, "version") != Num(action, "case_version") || Str(action, "channel_id") != Str(ch, "id") {
		return Conflict
	}
	if !Bool(ch, "enabled") || connectorDisabled(co) {
		return Forbidden
	}
	binding := Data{"case_id": Str(c, "id"), "case_version": Num(c, "version"), "reply_id": Str(reply, "id"), "reply_version": Num(reply, "version") + 1,
		"channel_id": Str(ch, "id"), "channel_version": Num(ch, "version"), "external_id": Str(ch, "external_id"), "connector_id": Str(co, "id"), "connector_version": Num(co, "version"),
		"provider": Str(co, "provider"), "app_id": Str(co, "app_id"), "tenant_id": Str(co, "tenant_id"), "source_message_id": Str(env, "last_message_id"), "source_number": Num(env, "source_number")}
	if source := Str(binding, "source_message_id"); source != "" {
		m, e := one(ctx, tx, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND case_id=$3 AND external_id=$4 AND NOT deleted", Str(co, "id"), Str(ch, "id"), Str(c, "id"), source)
		if e != nil {
			return Fail(409, "feedback_source_missing", "原始消息不可用，无法绑定答复反馈")
		}
		binding["source_row_id"], binding["source_version"], binding["source_revision"] = Str(m, "id"), Num(m, "version"), Str(m, "source_revision")
	} else {
		return Fail(409, "feedback_source_missing", "缺少原始消息，无法绑定答复反馈")
	}
	if Str(co, "provider") == "github" && Num(binding, "source_number") == 0 {
		for _, prefix := range []string{"issue:", "pull:"} {
			if strings.HasPrefix(Str(ch, "external_id"), prefix) {
				n, _ := strconv.Atoi(strings.TrimPrefix(Str(ch, "external_id"), prefix))
				binding["source_number"] = n
			}
		}
	}
	request["card_nonce"], request["feedback_context"] = Token(), binding
	return nil
}

func replyFeedbackCard(op Data, body string) json.RawMessage {
	request := Object(op, "request")
	buttons := []any{}
	for _, choice := range []struct{ action, label string }{{"resolved", "已解决"}, {"unresolved", "未解决"}, {"human", "转人工"}} {
		buttons = append(buttons, Data{"tag": "button", "text": Data{"tag": "plain_text", "content": choice.label}, "type": "default", "value": Data{"operation_id": Str(op, "id"), "nonce": Str(request, "card_nonce"), "action": "feedback_" + choice.action}})
	}
	return feedbackCardDocument(body,
		Data{"tag": "note", "elements": []any{Data{"tag": "plain_text", "content": "这条答复是否解决了问题？反馈只更新问题状态，不会批准开发或发布。"}}},
		Data{"tag": "action", "actions": buttons},
	)
}

func replyFeedbackResultCard(op Data, action string, duplicate bool) json.RawMessage {
	note := feedbackLabels[action]
	if note == "" {
		note = "这条答复的反馈已接收"
	}
	if duplicate {
		note += "。这条答复已记录过反馈，无需重复点击。按钮已停用。"
	} else {
		note += "。按钮已停用。"
	}
	return feedbackCardDocument(Str(Object(op, "request"), "body"), Data{"tag": "note", "elements": []any{Data{"tag": "plain_text", "content": note}}})
}

func replyFeedbackBody(op Data, body string) string {
	body = integration.Redact(body)
	request := Object(op, "request")
	if Str(op, "kind") != "reply_send" || Str(request, "card_nonce") == "" || len(Object(request, "feedback_context")) == 0 {
		return body
	}
	prefix := "/agentowl feedback " + Str(op, "id") + " " + Str(request, "card_nonce") + " "
	return body + "\n\n反馈：在本 Issue/PR 中新增评论，复制以下任一完整命令（仅更新问题状态，不批准开发）：\n" +
		"已解决：`" + prefix + "resolved`\n未解决：`" + prefix + "unresolved`\n转人工：`" + prefix + "human`"
}

func duplicateWebFeedback(current, latest Data, action string) bool {
	if Str(latest, "action") != action {
		return false
	}
	if Num(latest, "case_version")+1 != Num(current, "version") {
		return false
	}
	switch action {
	case "resolved":
		return Str(current, "status") == "resolved"
	case "human":
		return Str(current, "reply_mode") == "manual"
	case "unresolved":
		return Str(current, "status") == "in_progress"
	default:
		return false
	}
}

func (s *Service) applyFeedback(ctx context.Context, tx pgx.Tx, c Data, actor, action, reason string, metadata Data) error {
	if !contains([]string{"resolved", "unresolved", "human"}, action) {
		return Fail(422, "invalid_feedback", "反馈必须为已解决、未解决或转人工")
	}
	if utf8.RuneCountInString(reason) > 4000 {
		return Fail(422, "invalid_feedback", "反馈说明超过4000字符")
	}
	status, mode := "in_progress", Str(c, "reply_mode")
	if action == "resolved" {
		status = "resolved"
	}
	if action == "human" {
		mode = "manual"
	}
	if action == "unresolved" && mode != "manual" {
		mode = "draft"
	}
	if _, e := tx.Exec(ctx, "UPDATE cases SET status=$2,reply_mode=$3,version=version+1,updated_at=now() WHERE id=$1", Str(c, "id"), status, mode); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, "UPDATE replies SET status='stale',version=version+1,updated_at=now() WHERE case_id=$1 AND status IN ('generating','draft','pending','approved','queued')", Str(c, "id")); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE subject_type='reply' AND subject_id IN (SELECT id FROM replies WHERE case_id=$1) AND status IN ('pending','approved')", Str(c, "id")); e != nil {
		return e
	}
	metadata["action"], metadata["resolved"], metadata["case_version"] = action, action == "resolved", Num(c, "version")
	body := feedbackLabels[action]
	if strings.TrimSpace(reason) != "" {
		body += "：" + integration.Redact(reason)
	}
	if e := caseEvent(ctx, tx, Str(c, "id"), "feedback", actor, body, metadata); e != nil {
		return e
	}
	if action != "resolved" {
		if e := enqueue(ctx, tx, "notify", fmt.Sprintf("feedback:%s:%d", Str(c, "id"), Num(c, "version")+1), Data{"case_id": Str(c, "id"), "stage": "feedback_" + action, "reason": feedbackLabels[action]}, 0); e != nil {
			return e
		}
	}
	return audit(ctx, tx, Actor{ID: actor}, "feedback_"+action, "cases", Str(c, "id"), "")
}

// validateFeedbackContext has no role bypass. It proves that the clicked answer
// still belongs to this exact channel, source revision and current case version.
func (s *Service) validateFeedbackContext(ctx context.Context, tx pgx.Tx, op, ch Data, provider, connectorID string) (Data, error) {
	request, binding := Object(op, "request"), Object(Object(op, "request"), "feedback_context")
	if Str(op, "kind") != "reply_send" || Str(op, "status") != "succeeded" || len(binding) == 0 || Str(binding, "provider") != provider || Str(binding, "connector_id") != connectorID || Str(binding, "channel_id") != Str(ch, "id") || Str(Object(op, "target"), "channel_id") != Str(ch, "id") || Num(binding, "channel_version") != Num(ch, "version") || Str(binding, "external_id") != Str(ch, "external_id") {
		return nil, Forbidden
	}
	co, _, e := s.config(ctx, tx, connectorID, "connectors")
	if e != nil || connectorDisabled(co) || Num(co, "version") != Num(binding, "connector_version") {
		return nil, Forbidden
	}
	c, e := locked(ctx, tx, "cases", Str(binding, "case_id"))
	if e != nil {
		return nil, Forbidden
	}
	caseChannel := privateChannelFromCase(c)
	if len(caseChannel) == 0 {
		caseChannel = Data{"id": Str(c, "channel_id")}
	}
	if Str(c, "id") != Str(request, "case_id") || Str(caseChannel, "id") != Str(ch, "id") || Num(c, "version") != Num(binding, "case_version") {
		return nil, Conflict
	}
	r, e := locked(ctx, tx, "replies", Str(binding, "reply_id"))
	if e != nil {
		return nil, Forbidden
	}
	if Str(r, "id") != Str(request, "reply_id") || Str(r, "id") != Str(op, "subject_id") || Str(r, "case_id") != Str(c, "id") || Str(r, "external_operation_id") != Str(op, "id") || Str(r, "status") != "sent" || Num(r, "version") != Num(binding, "reply_version") {
		return nil, Forbidden
	}
	var latest string
	if e = tx.QueryRow(ctx, "SELECT id::text FROM replies WHERE case_id=$1 AND status='sent' ORDER BY updated_at DESC,id DESC LIMIT 1", Str(c, "id")).Scan(&latest); e != nil {
		return nil, e
	}
	if latest != Str(r, "id") {
		return nil, Conflict
	}
	if source := Str(binding, "source_row_id"); source != "" {
		m, e := one(ctx, tx, "SELECT to_jsonb(m) FROM source_messages m WHERE id=$1", source)
		if e != nil {
			return nil, Forbidden
		}
		if Bool(m, "deleted") || Str(m, "case_id") != Str(c, "id") || Str(m, "channel_id") != Str(ch, "id") || Str(m, "connector_id") != connectorID || Str(m, "external_id") != Str(binding, "source_message_id") || Num(m, "version") != Num(binding, "source_version") || Str(m, "source_revision") != Str(binding, "source_revision") || Str(Object(c, "environment"), "last_message_id") != Str(binding, "source_message_id") {
			return nil, Conflict
		}
	} else {
		return nil, Forbidden
	}
	return c, nil
}

func feedbackActor(provider, tenant, subject string) string {
	return "feedback:" + provider + ":" + tenant + ":" + Hash([]byte(subject))
}

func (s *Service) recordPlatformFeedback(ctx context.Context, tx pgx.Tx, op, ch Data, provider, connectorID, eventID, tenant, subject, action string) (bool, error) {
	if subject == "" || !contains([]string{"resolved", "unresolved", "human"}, action) {
		return false, Forbidden
	}
	binding := Object(Object(op, "request"), "feedback_context")
	var participant bool
	if e := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM source_messages WHERE case_id=$1 AND connector_id=$2 AND channel_id=$3 AND actor_id=$4 AND NOT deleted)", Str(binding, "case_id"), connectorID, Str(ch, "id"), subject).Scan(&participant); e != nil {
		return false, e
	}
	if !participant {
		return false, Forbidden
	}
	// A reply accepts one feedback decision. Repeated deliveries acknowledge the
	// existing result without changing cases or creating another notification.
	if len(Object(Object(op, "response"), "feedback")) > 0 {
		return true, nil
	}
	var accepted string
	e := tx.QueryRow(ctx, "INSERT INTO ingest_events(id,connector_id,event_id,kind,payload,status) VALUES($1,$2,$3,'user.feedback',$4,'processed') ON CONFLICT(connector_id,event_id) DO NOTHING RETURNING id", ID(), connectorID, eventID, JSON(Data{"operation_id": Str(op, "id"), "action": action, "actor": feedbackActor(provider, tenant, subject)})).Scan(&accepted)
	if errors.Is(e, pgx.ErrNoRows) {
		return true, nil
	}
	if e != nil {
		return false, e
	}
	c, e := s.validateFeedbackContext(ctx, tx, op, ch, provider, connectorID)
	if e != nil {
		return false, e
	}
	actor := feedbackActor(provider, tenant, subject)
	meta := Data{"source": provider, "operation_id": Str(op, "id"), "reply_id": Str(Object(op, "request"), "reply_id"), "message_id": Str(op, "remote_id")}
	if e = s.applyFeedback(ctx, tx, c, actor, action, "", meta); e != nil {
		return false, e
	}
	response := Object(op, "response")
	response["feedback"] = Data{"action": action, "actor": actor, "event_id": eventID, "case_version": Num(c, "version"), "received_at": time.Now().UTC()}
	_, e = tx.Exec(ctx, "UPDATE external_operations SET response=$2,version=version+1,updated_at=now() WHERE id=$1", Str(op, "id"), JSON(response))
	return false, e
}

func (s *Service) ingestFeedbackCard(ctx context.Context, tx pgx.Tx, provider, connectorID string, card integration.CardAction, op, ch, value Data) (Data, error) {
	if Str(op, "kind") != "reply_send" {
		return nil, Forbidden
	}
	binding := Object(Object(op, "request"), "feedback_context")
	if card.AppID != Str(binding, "app_id") || card.ActorID == "" {
		return nil, Forbidden
	}
	action := strings.TrimPrefix(Str(value, "action"), "feedback_")
	duplicate, e := s.recordPlatformFeedback(ctx, tx, op, ch, provider, connectorID, "feedback-card:"+card.ID, card.ActorTenantID, card.ActorID, action)
	if e != nil {
		return nil, e
	}
	if !Bool(ch, "private") {
		if e = s.observeCapability(ctx, tx, connectorID, Str(ch, "id"), "card_callback"); e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	recorded := Str(Object(Object(op, "response"), "feedback"), "action")
	if recorded == "" {
		recorded = action
	}
	message := feedbackLabels[recorded]
	if message == "" {
		message = "反馈已记录"
	}
	if duplicate {
		message = "这条答复的反馈已接收"
	}
	return Data{"toast": Data{"type": "success", "content": message}, "card": Data{"type": "raw", "data": replyFeedbackResultCard(op, recorded, duplicate)}}, nil
}

func (s *Service) ingestGitHubFeedback(ctx context.Context, tx pgx.Tx, connectorID string, ch Data, event integration.Event) (Data, bool, error) {
	text := strings.TrimSpace(event.Text)
	if !strings.HasPrefix(text, "/agentowl feedback") {
		return nil, false, nil
	}
	parts := strings.Fields(text)
	if event.Bot || event.ActorID == "" || event.Kind != "github.issue_comment" || event.Action != "created" {
		return Data{"ignored": true}, true, nil
	}
	if len(parts) != 5 || parts[0] != "/agentowl" || parts[1] != "feedback" || !validID(parts[2]) || !contains([]string{"resolved", "unresolved", "human"}, parts[4]) {
		return nil, true, Fail(422, "invalid_feedback", "反馈评论格式无效")
	}
	op, e := locked(ctx, tx, "external_operations", parts[2])
	if e != nil {
		return nil, true, Forbidden
	}
	request := Object(op, "request")
	nonce := Str(request, "card_nonce")
	binding := Object(request, "feedback_context")
	if nonce == "" || subtle.ConstantTimeCompare([]byte(nonce), []byte(parts[3])) != 1 || Str(op, "status") != "succeeded" || Str(op, "remote_id") == "" || Str(op, "kind") != "reply_send" || Str(binding, "connector_id") != connectorID || Str(binding, "channel_id") != Str(ch, "id") || Num(binding, "source_number") <= 0 || Num(binding, "source_number") != int64(event.Number) {
		return nil, true, Forbidden
	}
	duplicate, e := s.recordPlatformFeedback(ctx, tx, op, ch, "github", connectorID, "feedback-github:"+event.MessageID, event.Repository, event.ActorID, parts[4])
	if e != nil {
		return nil, true, e
	}
	return Data{"accepted": true, "feedback": true, "duplicate": duplicate}, true, nil
}
