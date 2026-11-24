package service

import (
	"context"
	"strings"
	"time"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

func sourceActorType(ev integration.Event) string {
	if ev.Bot {
		return "bot"
	}
	return "user"
}

func sourceOccurredAt(ev integration.Event) time.Time {
	if !ev.OccurredAt.IsZero() {
		return ev.OccurredAt.UTC()
	}
	return time.Now().UTC()
}

func sourceRevision(ev integration.Event, occurred time.Time) string {
	if ev.SourceRevision != "" {
		return ev.SourceRevision
	}
	return occurred.Format(time.RFC3339Nano)
}

func (s *Service) archiveSourceEvent(ctx context.Context, tx pgx.Tx, connectorID string, ch Data, ev integration.Event, caseID, thread string) (Data, error) {
	content := boundedText(integration.Redact(ev.Text), 64000)
	if ev.Deleted {
		content = ""
	}
	occurred := sourceOccurredAt(ev)
	revision := sourceRevision(ev, occurred)
	old, err := one(ctx, tx, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND external_id=$3 FOR UPDATE", connectorID, Str(ch, "id"), ev.MessageID)
	if err != nil && err != NotFound {
		return nil, err
	}
	if err == nil && Str(old, "source_revision") == revision && Bool(old, "deleted") == ev.Deleted && Str(old, "content") == content {
		return old, nil
	}
	id := Str(old, "id")
	if id == "" {
		id = ID()
	}
	actorKind := sourceActorType(ev)
	if ev.ActorID != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO source_actors(id,connector_id,subject_id,kind,updated_at) VALUES($1,$2,$3,$4,now()) ON CONFLICT(connector_id,subject_id) DO UPDATE SET kind=COALESCE(NULLIF(EXCLUDED.kind,''),source_actors.kind),updated_at=now()`, ID(), connectorID, ev.ActorID, actorKind); err != nil {
			return nil, err
		}
	}
	rawDigest := Hash(JSON(ev))
	contentDigest := Hash([]byte(content))
	mentions := append([]string{}, ev.MentionedUserIDs...)
	caseRef := nullIfBlank(caseID)
	if Str(old, "id") == "" {
		_, err = tx.Exec(ctx, `INSERT INTO source_messages(id,connector_id,channel_id,external_id,case_id,actor_id,content,thread_id,root_id,parent_id,platform_thread_id,actor_type,sender_name,message_type,mentioned_subjects,bot,occurred_at,source_url,source_revision,deleted,created_at,raw_digest,content_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'',$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`, id, connectorID, Str(ch, "id"), ev.MessageID, caseRef, ev.ActorID, content, thread, ev.RootID, ev.ParentID, ev.ThreadID, actorKind, ev.MessageType, mentions, ev.Bot, occurred, ev.SourceURL, revision, ev.Deleted, occurred, rawDigest, contentDigest)
	} else {
		_, err = tx.Exec(ctx, `UPDATE source_messages SET case_id=COALESCE($2::uuid,case_id),actor_id=$3,content=$4,thread_id=$5,root_id=$6,parent_id=$7,platform_thread_id=$8,actor_type=$9,message_type=$10,mentioned_subjects=$11,bot=$12,occurred_at=$13,source_url=$14,source_revision=$15,deleted=$16,raw_digest=$17,content_digest=$18,version=version+1,updated_at=now() WHERE id=$1`, id, caseRef, ev.ActorID, content, thread, ev.RootID, ev.ParentID, ev.ThreadID, actorKind, ev.MessageType, mentions, ev.Bot, occurred, ev.SourceURL, revision, ev.Deleted, rawDigest, contentDigest)
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO source_message_versions(message_id,version,content,source_revision,deleted,root_id,parent_id,thread_id,platform_thread_id,actor_id,actor_type,sender_name,message_type,mentioned_subjects,bot,occurred_at,source_url,content_digest) SELECT id,version,content,source_revision,deleted,root_id,parent_id,thread_id,platform_thread_id,actor_id,actor_type,sender_name,message_type,mentioned_subjects,bot,occurred_at,source_url,content_digest FROM source_messages WHERE id=$1 ON CONFLICT DO NOTHING`, id); err != nil {
		return nil, err
	}
	for _, a := range ev.Attachments {
		if a.Key == "" {
			continue
		}
		if _, err = tx.Exec(ctx, `INSERT INTO source_message_files(message_id,file_key,connector_id,channel_id,kind,name,updated_at) VALUES($1,$2,$3,$4,$5,$6,now()) ON CONFLICT(message_id,file_key) DO UPDATE SET kind=EXCLUDED.kind,name=EXCLUDED.name,updated_at=now()`, id, a.Key, connectorID, Str(ch, "id"), a.Type, a.Name); err != nil {
			return nil, err
		}
	}
	return one(ctx, tx, "SELECT to_jsonb(m) FROM source_messages m WHERE id=$1", id)
}

func (s *Service) recordSourceSyncStatus(ctx context.Context, q DB, connectorID, channelID, threadKey string, syncedUntil time.Time, lastError string) error {
	var until any
	backfilled := false
	if !syncedUntil.IsZero() {
		until = syncedUntil.UTC()
		backfilled = true
	}
	_, err := q.Exec(ctx, `INSERT INTO source_sync_status(connector_id,channel_id,thread_key,backfilled,synced_until,last_attempt_at,last_error,updated_at) VALUES($1,$2,$3,$4,$5,now(),$6,now()) ON CONFLICT(connector_id,channel_id,thread_key) DO UPDATE SET backfilled=source_sync_status.backfilled OR EXCLUDED.backfilled,synced_until=COALESCE(EXCLUDED.synced_until,source_sync_status.synced_until),last_attempt_at=now(),last_error=EXCLUDED.last_error,updated_at=now()`, connectorID, channelID, threadKey, backfilled, until, lastError)
	return err
}

func (s *Service) archiveExternalOperation(ctx context.Context, tx pgx.Tx, op Data, result Data) error {
	if Str(op, "kind") == "read_mark" || Str(result, "remote_id") == "" {
		return nil
	}
	ch, co, err := s.operationChannel(ctx, tx, op)
	if err != nil {
		return err
	}
	if Str(ch, "id") == "" {
		return nil
	}
	req := Object(op, "request")
	var c Data
	caseID := Str(req, "case_id")
	if caseID != "" {
		c, err = one(ctx, tx, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", caseID)
		if err != nil {
			return err
		}
	}
	body := integration.Redact(Str(req, "body"))
	if Str(co, "provider") != "github" && !Bool(ch, "private") {
		body = replyBodyWithMention(req, body)
	}
	if body == "" {
		return nil
	}
	remoteID := Str(result, "remote_id")
	if Str(co, "provider") == "github" && remoteID != "" && !strings.HasPrefix(remoteID, "comment:") {
		remoteID = "comment:" + remoteID
	}
	parentID, platformThread := "", ""
	if Str(op, "kind") == "reply_send" {
		parentID = Str(Object(c, "environment"), "last_message_id")
		platformThread = Str(Object(c, "environment"), "platform_thread_id")
	}
	actor := fallback(Str(co, "bot_open_id"), fallback(Str(co, "sender_id"), "agentowl"))
	thread := sourceThread(integration.Event{Repository: Str(co, "repository"), ChannelID: Str(ch, "external_id"), MessageID: remoteID, RootID: parentID, ParentID: parentID, ThreadID: platformThread})
	ev := integration.Event{ID: "operation:" + Str(op, "id"), Kind: "agentowl.operation", AppID: Str(co, "app_id"), TenantID: Str(co, "tenant_id"), ChannelID: Str(ch, "external_id"), MessageID: remoteID, ParentID: parentID, ThreadID: platformThread, ActorID: actor, SourceRevision: time.Now().UTC().Format(time.RFC3339Nano), OccurredAt: time.Now().UTC(), Text: body, MessageType: "text", SourceURL: Str(Object(result, "response"), "url"), Repository: Str(co, "repository"), Number: int(Num(result, "issue_number")), Bot: true}
	_, err = s.archiveSourceEvent(ctx, tx, Str(co, "id"), ch, ev, caseID, thread)
	return err
}
