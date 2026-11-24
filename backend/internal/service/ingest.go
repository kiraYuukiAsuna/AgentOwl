package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

type cachedSupportLark struct {
	fingerprint string
	client      *integration.LarkClient
}

var supportLarkClients = struct {
	sync.Mutex
	entries map[string]cachedSupportLark
	order   []string
}{entries: map[string]cachedSupportLark{}}

func connectorDisabled(c Data) bool {
	if enabled, ok := c["enabled"].(bool); ok && !enabled {
		return true
	}
	return contains([]string{"disabled", "revoked"}, Str(c, "status"))
}
func (s *Service) supportLark(ctx context.Context, q DB, id string) (*integration.LarkClient, Data, error) {
	c, secret, e := s.config(ctx, q, id, "connectors")
	if e != nil {
		return nil, nil, e
	}
	if !contains([]string{"lark", "feishu"}, Str(c, "provider")) {
		return nil, nil, Fail(422, "invalid_connector", "不是飞书/Lark 连接器")
	}
	base := Str(c, "base_url")
	if base == "" && Str(c, "provider") == "lark" {
		base = "https://open.larksuite.com"
	}
	// Reuse the adapter's synchronized tenant token cache across HTTP requests and
	// jobs. Credential/config changes invalidate it; old clients are bounded.
	cacheKey := fmt.Sprintf("%p:%s", s, id)
	fingerprint := Hash(JSON(Data{"base": base, "app_id": Str(c, "app_id"), "version": Num(c, "version"), "secret": secret}))
	supportLarkClients.Lock()
	defer supportLarkClients.Unlock()
	if cached, ok := supportLarkClients.entries[cacheKey]; ok && cached.fingerprint == fingerprint {
		return cached.client, c, nil
	}
	cli, e := integration.NewLarkClient(integration.LarkConfig{BaseURL: base, AppID: Str(c, "app_id"), TenantKey: Str(c, "tenant_id"), AppSecret: Str(secret, "app_secret"), VerificationToken: Str(secret, "verification_token"), EncryptKey: Str(secret, "encrypt_key")})
	if e == nil {
		if _, ok := supportLarkClients.entries[cacheKey]; !ok {
			if len(supportLarkClients.order) >= 64 {
				delete(supportLarkClients.entries, supportLarkClients.order[0])
				supportLarkClients.order = supportLarkClients.order[1:]
			}
			supportLarkClients.order = append(supportLarkClients.order, cacheKey)
		}
		supportLarkClients.entries[cacheKey] = cachedSupportLark{fingerprint, cli}
	}
	return cli, c, e
}

func privateChannelID(connectorID, actorID, externalID string) string {
	subject := actorID
	if subject == "" {
		subject = externalID
	}
	return "private:" + connectorID + ":" + subject
}

func privateChannel(connectorID, externalID, actorID, replyMode string) Data {
	return Data{"id": privateChannelID(connectorID, actorID, externalID), "name": "私聊", "connector_id": connectorID, "external_id": externalID, "kind": "private", "enabled": true, "visibility": "internal", "reply_mode": fallback(replyMode, "auto"), "private": true, "private_actor_id": actorID}
}

func privateChannelFromCase(c Data) Data {
	env := Object(c, "environment")
	if !Bool(env, "p2p_chat") || Str(env, "private_connector_id") == "" || Str(env, "external_channel_id") == "" {
		return nil
	}
	return privateChannel(Str(env, "private_connector_id"), Str(env, "external_channel_id"), Str(env, "private_actor_id"), Str(c, "reply_mode"))
}

func (s *Service) caseReplyChannel(ctx context.Context, q DB, c Data) (Data, Data, error) {
	if channelID := Str(c, "channel_id"); channelID != "" {
		ch, _, e := s.config(ctx, q, channelID, "channels")
		if e != nil {
			return nil, nil, e
		}
		co, _, e := s.config(ctx, q, Str(ch, "connector_id"), "connectors")
		return ch, co, e
	}
	ch := privateChannelFromCase(c)
	if len(ch) == 0 {
		return nil, nil, Fail(422, "no_reply_channel", "此问题没有可发送渠道")
	}
	co, _, e := s.config(ctx, q, Str(ch, "connector_id"), "connectors")
	return ch, co, e
}

func (s *Service) operationChannel(ctx context.Context, q DB, op Data) (Data, Data, error) {
	target := Object(op, "target")
	if Bool(target, "private_channel") {
		ch := privateChannel(Str(target, "connector_id"), Str(target, "external_id"), Str(target, "private_actor_id"), "auto")
		co, _, e := s.config(ctx, q, Str(ch, "connector_id"), "connectors")
		return ch, co, e
	}
	ch, _, e := s.config(ctx, q, Str(target, "channel_id"), "channels")
	if e != nil {
		return nil, nil, e
	}
	co, _, e := s.config(ctx, q, Str(ch, "connector_id"), "connectors")
	return ch, co, e
}
func (s *Service) supportGitHub(ctx context.Context, q DB, id, kind string) (*integration.GitHubClient, Data, error) {
	c, secret, e := s.config(ctx, q, id, kind)
	if e != nil {
		return nil, nil, e
	}
	owner, repo := Str(c, "owner"), Str(c, "repo")
	if kind == "connectors" && Str(c, "provider") != "github" {
		return nil, nil, Fail(422, "invalid_connector", "不是 GitHub 连接器")
	}
	if rid := Str(c, "repository_id"); rid != "" && kind == "connectors" {
		r, rs, e := s.config(ctx, q, rid, "repositories")
		if e != nil {
			return nil, nil, e
		}
		if !Bool(r, "enabled") {
			return nil, nil, Fail(409, "repository_disabled", "仓库未启用")
		}
		owner, repo = Str(r, "owner"), Str(r, "repo")
		if Str(secret, "token") == "" {
			secret["token"] = Str(rs, "token")
		}
	}
	if kind == "repositories" && Str(secret, "token") == "" && Str(c, "connector_id") != "" {
		_, cs, e := s.config(ctx, q, Str(c, "connector_id"), "connectors")
		if e != nil {
			return nil, nil, e
		}
		secret["token"] = Str(cs, "token")
	}
	cli, e := integration.NewGitHubClient(integration.GitHubConfig{BaseURL: Str(c, "base_url"), Token: Str(secret, "token"), WebhookSecret: Str(secret, "webhook_secret"), Owner: owner, Repo: repo})
	return cli, c, e
}

// IngestWebhook acknowledges only after normalized, redacted input and its job
// commit together. Platform retries therefore cannot create a second job.
func (s *Service) IngestWebhook(ctx context.Context, provider, connectorID string, headers http.Header, body []byte) (Data, error) {
	if !contains([]string{"feishu", "lark", "github"}, provider) || len(body) > 2<<20 {
		return nil, Fail(400, "invalid_webhook", "事件无效")
	}
	c, secrets, e := s.config(ctx, s.DB, connectorID, "connectors")
	if e != nil {
		return nil, e
	}
	if Str(c, "provider") != provider {
		return nil, Forbidden
	}
	adapter, _, e := s.sourceAdapter(ctx, s.DB, c)
	if e != nil {
		return nil, e
	}
	in, e := adapter.Parse(headers, body)
	if e != nil {
		return nil, Fail(401, "invalid_webhook", "事件验证失败")
	}
	if in.Challenge != "" {
		return Data{"challenge": in.Challenge}, nil
	}
	if in.Ignored {
		return Data{"ignored": true}, nil
	}
	return s.ingestVerifiedEvent(ctx, provider, connectorID, c, secrets, in.Event, in.Card)
}

func (s *Service) IngestCardAction(ctx context.Context, provider, connectorID string, headers http.Header, body []byte) (Data, error) {
	if !contains([]string{"feishu", "lark"}, provider) || len(body) > 2<<20 {
		return nil, Fail(400, "invalid_webhook", "事件无效")
	}
	c, secrets, e := s.config(ctx, s.DB, connectorID, "connectors")
	if e != nil {
		return nil, e
	}
	if Str(c, "provider") != provider {
		return nil, Forbidden
	}
	cli, _, e := s.supportLark(ctx, s.DB, connectorID)
	if e != nil {
		return nil, e
	}
	in, e := cli.ParseCardAction(headers, body)
	if e != nil {
		return nil, Fail(401, "invalid_webhook", "事件验证失败")
	}
	if in.Challenge != "" {
		return Data{"challenge": in.Challenge}, nil
	}
	if in.CardAction == nil {
		return nil, Fail(400, "invalid_webhook", "缺少卡片操作")
	}
	return s.ingestVerifiedEvent(ctx, provider, connectorID, c, secrets, nil, in.CardAction)
}

// The caller must authenticate the delivery before invoking this shared path.
func (s *Service) ingestVerifiedEvent(ctx context.Context, provider, connectorID string, c, secrets Data, event *integration.Event, card *integration.CardAction) (Data, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if e = lockExecutionSettings(ctx, tx); e != nil {
		return nil, e
	}
	// Serialize configuration withdrawal with acceptance. Recheck after signature
	// verification because config may have changed while parsing the request.
	raw, e := locked(ctx, tx, "configs", connectorID)
	if e != nil {
		return nil, e
	}
	current := configPublic(raw)
	if Num(current, "version") != Num(c, "version") {
		return nil, Conflict
	}
	if connectorDisabled(current) {
		return Data{"ignored": true}, nil
	}
	if card != nil {
		return s.ingestCard(ctx, tx, provider, connectorID, *card)
	}
	if event == nil {
		return nil, Fail(400, "invalid_webhook", "缺少事件")
	}
	knownSecrets := []string{}
	for _, value := range secrets {
		if secret, ok := value.(string); ok && secret != "" {
			knownSecrets = append(knownSecrets, secret)
		}
	}
	event.Text = integration.Redact(event.Text, knownSecrets...)
	if provider == "github" && strings.HasPrefix(event.MessageID, "comment:") && Str(c, "sender_id") != "" && Str(c, "sender_id") == event.ActorID {
		event.Bot = true
	}
	channel, e := s.eventChannel(ctx, tx, connectorID, *event)
	if errors.Is(e, NotFound) {
		return Data{"ignored": true}, nil
	}
	if e != nil {
		return nil, e
	}
	if provider == "github" {
		result, handled, err := s.ingestGitHubFeedback(ctx, tx, connectorID, channel, *event)
		if handled {
			if err == nil {
				err = tx.Commit(ctx)
			}
			return result, err
		}
	}
	id, duplicate, e := s.persistEvent(ctx, tx, connectorID, channel, *event)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return Data{"accepted": true, "id": id, "duplicate": duplicate}, nil
}

func (s *Service) eventChannel(ctx context.Context, q DB, connectorID string, event integration.Event) (Data, error) {
	external := event.ChannelID
	if external == "" && event.Deleted {
		m, e := one(ctx, q, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND external_id=$2", connectorID, event.MessageID)
		if e != nil {
			return nil, e
		}
		if strings.HasPrefix(Str(m, "channel_id"), "private:") {
			c, e := one(ctx, q, "SELECT to_jsonb(c) FROM cases c WHERE id=(SELECT case_id FROM source_messages WHERE id=$1)", Str(m, "id"))
			if e != nil {
				return nil, e
			}
			ch := privateChannelFromCase(c)
			if len(ch) == 0 {
				return nil, NotFound
			}
			return ch, nil
		}
		c, _, e := s.config(ctx, q, Str(m, "channel_id"), "channels")
		if e != nil {
			return nil, e
		}
		if !Bool(c, "enabled") {
			return nil, NotFound
		}
		return c, nil
	}
	if event.ChatType == "p2p" && external != "" {
		return privateChannel(connectorID, external, event.ActorID, "auto"), nil
	}
	rows, e := many(ctx, q, "SELECT to_jsonb(c) FROM configs c WHERE kind='channels' AND data->>'connector_id'=$1 AND data->>'enabled'='true' AND (data->>'external_id'=$2 OR ($3<>'' AND data->>'external_id'=$3)) ORDER BY (data->>'external_id'=$2) DESC,id", connectorID, external, event.Repository)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, NotFound
	}
	best := configPublic(rows[0])
	if len(rows) > 1 && Str(configPublic(rows[1]), "external_id") == Str(best, "external_id") {
		return nil, Fail(409, "ambiguous_channel", "相同来源配置了多个接入渠道")
	}
	return best, nil
}

func (s *Service) persistEvent(ctx context.Context, tx pgx.Tx, connectorID string, channel Data, event integration.Event) (string, bool, error) {
	event.Text = integration.Redact(event.Text)
	event.SourceURL = integration.Redact(event.SourceURL)
	for i := range event.Attachments {
		event.Attachments[i].Name = integration.Redact(event.Attachments[i].Name)
	}
	if event.Repository != "" && strings.HasPrefix(event.MessageID, "comment:") {
		const prefix = "<!-- agentowl-operation:"
		if at := strings.LastIndex(event.Text, prefix); at >= 0 && strings.HasSuffix(event.Text, " -->") {
			operationID := strings.TrimSuffix(event.Text[at+len(prefix):], " -->")
			if validID(operationID) {
				op, e := one(ctx, tx, "SELECT to_jsonb(o) FROM external_operations o WHERE id=$1 AND kind IN ('reply_send','notify') AND target->>'channel_id'=$2", operationID, Str(channel, "id"))
				if e != nil && e != NotFound {
					return "", false, e
				}
				if e == nil {
					marker, _ := integration.OperationMarker(operationID)
					if event.Text == replyFeedbackBody(op, Str(Object(op, "request"), "body"))+"\n\n"+marker {
						event.Bot = true
					}
				}
			}
		}
	}
	id := ID()
	payload := Data{"channel_id": Str(channel, "id"), "event": event}
	if Bool(channel, "private") {
		payload["channel"] = channel
	}
	var inserted string
	e := tx.QueryRow(ctx, "INSERT INTO ingest_events(id,connector_id,event_id,kind,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT(connector_id,event_id) DO NOTHING RETURNING id", id, connectorID, event.ID, event.Kind, JSON(payload)).Scan(&inserted)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", true, nil
	}
	if e != nil {
		return "", false, e
	}
	// Stop an already-generated response as soon as a newer live message is
	// accepted, rather than waiting for the asynchronous ingest worker. Historical
	// backfills do not invalidate a reply merely because their delivery ID differs.
	if !event.Bot && !strings.HasPrefix(event.ID, "history:") {
		cases, err := many(ctx, tx, `SELECT to_jsonb(c) FROM cases c WHERE EXISTS(SELECT 1 FROM source_messages m WHERE m.case_id=c.id AND m.connector_id=$2 AND m.channel_id=$1 AND (m.external_id=$3 OR m.external_id=$4 OR m.external_id=$5 OR m.thread_id=$6)) AND NOT EXISTS(SELECT 1 FROM source_messages m WHERE m.connector_id=$2 AND m.channel_id=$1 AND m.external_id=$3 AND m.source_revision=$7 AND m.deleted=$8 AND m.content=$9) ORDER BY c.id FOR UPDATE`, Str(channel, "id"), connectorID, event.MessageID, event.RootID, event.ParentID, sourceThread(event), event.SourceRevision, event.Deleted, boundedText(event.Text, 64000))
		if err != nil {
			return "", false, err
		}
		for _, c := range cases {
			if _, err = tx.Exec(ctx, "UPDATE cases SET version=version+1,updated_at=now() WHERE id=$1", Str(c, "id")); err != nil {
				return "", false, err
			}
			if _, err = tx.Exec(ctx, "UPDATE replies SET status='stale',version=version+1,updated_at=now() WHERE case_id=$1 AND status IN ('generating','draft','pending','approved','queued')", Str(c, "id")); err != nil {
				return "", false, err
			}
			if _, err = tx.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE subject_type='reply' AND subject_id IN(SELECT id FROM replies WHERE case_id=$1) AND status IN ('pending','approved')", Str(c, "id")); err != nil {
				return "", false, err
			}
		}
	}
	if e = s.registerTaskSource(ctx, tx, id, connectorID, channel, event); e != nil {
		return "", false, e
	}
	e = enqueue(ctx, tx, "ingest_event", "ingest:"+id, Data{"event_id": id}, 0)
	if e == nil && !strings.HasPrefix(event.ID, "history:") && !event.Bot && !event.Deleted {
		co, _, err := s.config(ctx, tx, connectorID, "connectors")
		if err != nil {
			return "", false, err
		}
		if !Bool(channel, "private") {
			capability := ""
			if event.Repository != "" || len(event.MentionedUserIDs) == 0 && event.ParentID == "" {
				capability = "receive_messages"
			}
			if Str(co, "bot_open_id") != "" && contains(event.MentionedUserIDs, Str(co, "bot_open_id")) {
				capability = "receive_mentions"
			}
			if capability != "" {
				e = s.observeCapability(ctx, tx, connectorID, Str(channel, "id"), capability)
			}
		}
		if e == nil && (Str(co, "provider") == "feishu" || Str(co, "provider") == "lark") {
			markID := ID()
			request := Data{"message_id": event.MessageID, "emoji_type": "Typing", "connector_id": connectorID, "connector_version": Num(co, "version")}
			target := Data{"channel_id": Str(channel, "id"), "connector_id": connectorID, "connector_version": Num(co, "version"), "external_id": Str(channel, "external_id")}
			if Bool(channel, "private") {
				target["private_channel"], target["private_actor_id"] = true, Str(channel, "private_actor_id")
			} else {
				target["channel_version"] = Num(channel, "version")
			}
			if _, err = tx.Exec(ctx, "INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request) VALUES($1,'read_mark',$2,$3,$4,$5) ON CONFLICT(dedupe_key) DO NOTHING", markID, id, "readmark:"+connectorID+":"+event.MessageID, JSON(target), JSON(request)); err != nil {
				return "", false, err
			}
			if e = enqueue(ctx, tx, "dispatch_external", "dispatch:"+markID, Data{"operation_id": markID}, 0); e != nil {
				return "", false, e
			}
		}
	}
	return id, false, e
}

// Cards are tied to an operation created by this service, its remote message,
// an unpredictable nonce, and a server-stored action. Card values never supply
// roles, repository scope, approval evidence or development instructions.
func (s *Service) ingestCard(ctx context.Context, tx pgx.Tx, provider, connectorID string, card integration.CardAction) (Data, error) {
	value := Data{}
	if json.Unmarshal(card.Value, &value) != nil || len(value) != 3 {
		return nil, Forbidden
	}
	op, e := locked(ctx, tx, "external_operations", Str(value, "operation_id"))
	if e != nil {
		return nil, Forbidden
	}
	req := Object(op, "request")
	nonce := Str(req, "card_nonce")
	if nonce == "" || subtle.ConstantTimeCompare([]byte(nonce), []byte(Str(value, "nonce"))) != 1 || Str(op, "remote_id") != card.MessageID || Str(op, "status") != "succeeded" {
		return nil, Forbidden
	}
	ch, _, e := s.operationChannel(ctx, tx, op)
	if e != nil || !Bool(ch, "enabled") || Str(ch, "connector_id") != connectorID || Str(ch, "external_id") != card.ChannelID {
		return nil, Forbidden
	}
	if strings.HasPrefix(Str(value, "action"), "feedback_") {
		return s.ingestFeedbackCard(ctx, tx, provider, connectorID, card, op, ch, value)
	}
	a, _, e := resolvePlatformIdentity(ctx, tx, provider, card.AppID, card.ActorID)
	if e != nil {
		if !errors.Is(e, NotFound) {
			return nil, e
		}
		return nil, Forbidden
	}
	name := Str(value, "action")
	actions := Object(req, "card_actions")
	action := Object(actions, name)
	if len(action) == 0 {
		return nil, Forbidden
	}
	var eid string
	e = tx.QueryRow(ctx, "INSERT INTO ingest_events(id,connector_id,event_id,kind,payload,status) VALUES($1,$2,$3,'card.action.trigger',$4,'processed') ON CONFLICT(connector_id,event_id) DO NOTHING RETURNING id", ID(), connectorID, card.ID, JSON(Data{"operation_id": Str(op, "id"), "action": name, "actor_id": a.ID})).Scan(&eid)
	if errors.Is(e, pgx.ErrNoRows) {
		return Data{"toast": Data{"type": "info", "content": "此操作已接收"}, "card": Data{"type": "raw", "data": notificationResultCard(op, Str(value, "action"), true)}}, nil
	}
	if e != nil {
		return nil, e
	}
	switch name {
	case "approve", "reject", "revoke":
		action["decision"] = name
		_, e = s.Decide(ctx, tx, a, Str(action, "approval_id"), action)
	case "claim", "takeover", "resume":
		action["action"] = name
		var current Data
		current, e = s.CaseAction(ctx, tx, a, Str(action, "case_id"), action)
		if e == nil && name == "claim" && len(Object(actions, "send_reply")) > 0 {
			// Only advance the case version for this checked claim; the draft version stays frozen.
			Object(actions, "send_reply")["case_version"] = Num(current, "version")
			_, e = tx.Exec(ctx, "UPDATE external_operations SET request=$2 WHERE id=$1", Str(op, "id"), JSON(req))
		}
	case "send_reply":
		var approval Data
		approval, e = s.SubmitReply(ctx, tx, a, Str(action, "reply_id"), action)
		if e == nil {
			_, e = s.Decide(ctx, tx, a, Str(approval, "id"), Data{"expected_version": Num(approval, "version"), "decision": "approve"})
		}
		if e == nil {
			req["reply_approval_id"] = Str(approval, "id")
			_, e = tx.Exec(ctx, "UPDATE external_operations SET request=$2 WHERE id=$1", Str(op, "id"), JSON(req))
		}
	case "ask_requester":
		body := fallback(Str(action, "body"), "请补充更多信息，以便我们继续排查。")
		var reply Data
		reply, e = s.CreateReply(ctx, tx, a, Str(action, "case_id"), Data{"body": body, "expected_version": Num(action, "case_version")})
		if e == nil {
			var approval Data
			approval, e = s.SubmitReply(ctx, tx, a, Str(reply, "id"), Data{"expected_version": Num(reply, "version"), "case_version": Num(action, "case_version")})
			if e == nil {
				_, e = s.Decide(ctx, tx, a, Str(approval, "id"), Data{"expected_version": Num(approval, "version"), "decision": "approve"})
			}
			if e == nil {
				req["ask_requester_reply_id"] = Str(reply, "id")
				req["ask_requester_approval_id"] = Str(approval, "id")
				_, e = tx.Exec(ctx, "UPDATE external_operations SET request=$2 WHERE id=$1", Str(op, "id"), JSON(req))
			}
		}
	case "confirm_development":
		_, e = s.ConfirmDevelopment(ctx, tx, a, Str(action, "case_id"), action)
	default:
		return nil, Forbidden
	}
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
	return Data{"toast": Data{"type": "success", "content": "操作已记录"}, "card": Data{"type": "raw", "data": notificationResultCard(op, name, false)}}, nil
}

func (s *Service) ScheduleResource(ctx context.Context, tx pgx.Tx, a Actor, resource, id string, b Data) (Data, error) {
	if !contains([]string{"connectors", "channels", "knowledge-sources", "external-operations"}, resource) {
		return nil, NotFound
	}
	if resource == "knowledge-sources" {
		if !a.Has("knowledge", "developer") {
			return nil, Forbidden
		}
	} else if resource == "channels" {
		if !a.Has("admin", "oncall") {
			return nil, Forbidden
		}
	} else if !a.Has("admin") {
		return nil, Forbidden
	}
	m, e := s.Get(ctx, tx, a, resource, id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(m, b); e != nil {
		return nil, e
	}
	kind, key := "", ""
	payload := Data{"actor_id": a.ID, "version": Num(m, "version")}
	switch resource {
	case "connectors":
		kind, key = "verify_connector", "connector_id"
	case "channels":
		kind, key = "sync_channel", "channel_id"
	case "knowledge-sources":
		kind, key = "sync_source", "source_id"
	case "external-operations":
		kind, key = "reconcile_external", "operation_id"
		if !contains([]string{"outcome_unknown", "dispatching", "failed"}, Str(m, "status")) {
			return nil, Fail(409, "invalid_state", "当前操作无需核对")
		}
	}
	payload[key] = id
	jobKey := kind + ":" + id + ":" + ID()
	if e = enqueue(ctx, tx, kind, jobKey, payload, 0); e != nil {
		return nil, e
	}
	if e = audit(ctx, tx, a, "schedule_"+strings.TrimPrefix(kind, "sync_"), resource, id, ""); e != nil {
		return nil, e
	}
	return Data{"accepted": true, "resource_id": id, "job_key": jobKey, "scheduled_at": time.Now().UTC()}, nil
}
