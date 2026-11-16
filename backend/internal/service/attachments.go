package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

const (
	MaxAttachmentBytes  = 4 << 20
	MaxVisionImageBytes = 2 << 20
	MaxVisionImages     = 3
)

type AttachmentContent struct {
	Bytes               []byte
	MediaType, Filename string
	Version             int64
}
type attachmentSnapshot struct {
	c, ch, co, message Data
	ref                integration.Attachment
	refsHash           string
}

// ReadCaseAttachment never accepts a caller-supplied resource key or URL. Both
// ACL and source/config versions are rechecked after the bounded remote read.
// Bytes are original evidence, not parsed, redacted, persisted or sent to a model.
func (s *Service) ReadCaseAttachment(ctx context.Context, a Actor, id string, index int, expectedVersion int64) (AttachmentContent, error) {
	if index < 0 || expectedVersion < 1 {
		return AttachmentContent{}, Fail(422, "attachment_version_required", "缺少附件编号或问题版本，请刷新页面后重试")
	}
	before, e := s.attachmentSnapshot(ctx, a, id, index, expectedVersion)
	if e != nil {
		return AttachmentContent{}, e
	}
	cli, clientConfig, e := s.supportLark(ctx, s.DB, Str(before.co, "id"))
	if e != nil {
		return AttachmentContent{}, e
	}
	if Num(clientConfig, "version") != Num(before.co, "version") {
		return AttachmentContent{}, Conflict
	}
	data, e := (integration.LarkSource{Client: cli}).ReadAttachment(ctx, Str(before.message, "external_id"), before.ref)
	if e != nil {
		if errors.Is(e, integration.ErrResponseTooLarge) {
			return AttachmentContent{}, Fail(413, "attachment_too_large", "附件超过 4 MiB 下载上限，请到原始消息中查看")
		}
		var upstream *integration.APIError
		if errors.As(e, &upstream) {
			return AttachmentContent{}, e
		}
		return AttachmentContent{}, Fail(502, "attachment_unavailable", "附件读取失败，请确认原始消息仍在，并检查应用的读取权限")
	}
	if len(data) == 0 || len(data) > MaxAttachmentBytes {
		return AttachmentContent{}, Fail(413, "attachment_too_large", "附件为空或超过 4 MiB 下载上限，请到原始消息中查看")
	}
	media, name, e := attachmentType(before.ref, data)
	if e != nil {
		return AttachmentContent{}, e
	}
	after, e := s.attachmentSnapshot(ctx, a, id, index, expectedVersion)
	if e != nil {
		return AttachmentContent{}, e
	}
	if before.refsHash != after.refsHash || Str(before.message, "id") != Str(after.message, "id") || Num(before.message, "version") != Num(after.message, "version") || Num(before.ch, "version") != Num(after.ch, "version") || Num(before.co, "version") != Num(after.co, "version") {
		return AttachmentContent{}, Conflict
	}
	// Observations are informational only; they never authorize a later request.
	_ = s.transaction(ctx, func(tx pgx.Tx) error {
		return s.observeCapability(ctx, tx, Str(after.co, "id"), Str(after.ch, "id"), "attachment_read")
	})
	return AttachmentContent{Bytes: data, MediaType: media, Filename: name, Version: expectedVersion}, nil
}

func (s *Service) caseAttachmentContext(ctx context.Context, q DB, channelID, caseID string, co Data, consumingEvent ...string) ([]integration.Attachment, Data, error) {
	if channelID == "" || caseID == "" || Str(co, "app_id") == "" || Str(co, "tenant_id") == "" {
		return nil, nil, nil
	}
	eventID := ""
	if len(consumingEvent) > 0 {
		eventID = consumingEvent[0]
	}
	messages, e := many(ctx, q, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND case_id=$3 AND NOT deleted ORDER BY created_at DESC,external_id DESC LIMIT 30", Str(co, "id"), channelID, caseID)
	if e != nil {
		return nil, nil, e
	}
	for _, message := range messages {
		if Str(message, "external_id") == "" || Str(message, "source_revision") == "" {
			continue
		}
		var pending bool
		if e = q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM ingest_events WHERE connector_id=$1 AND payload->>'channel_id'=$2 AND payload->'event'->>'message_id'=$3 AND status<>'processed' AND id::text<>$4 AND payload->'event'->>'bot'='false')", Str(co, "id"), channelID, Str(message, "external_id"), eventID).Scan(&pending); e != nil {
			return nil, nil, e
		}
		if pending {
			continue
		}
		events, e := many(ctx, q, "SELECT payload FROM ingest_events WHERE connector_id=$1 AND payload->>'channel_id'=$2 AND payload->'event'->>'message_id'=$3 AND payload->'event'->>'source_revision'=$4 ORDER BY created_at DESC LIMIT 10", Str(co, "id"), channelID, Str(message, "external_id"), Str(message, "source_revision"))
		if e != nil {
			return nil, nil, e
		}
		for _, payload := range events {
			event := integration.Event{}
			if json.Unmarshal(JSON(payload["event"]), &event) != nil || event.Deleted || event.AppID != Str(co, "app_id") || event.TenantID != Str(co, "tenant_id") || len(event.Attachments) == 0 {
				continue
			}
			return event.Attachments, Data{"message_id": Str(message, "external_id"), "source_revision": Str(message, "source_revision"), "connector_id": Str(co, "id"), "app_id": Str(co, "app_id"), "tenant_id": Str(co, "tenant_id")}, nil
		}
	}
	return nil, nil, nil
}

func (s *Service) supportCaseImages(ctx context.Context, c Data) ([]integration.InputImage, []Data, error) {
	channelID := Str(c, "channel_id")
	if channelID == "" {
		return nil, nil, nil
	}
	ch, _, e := s.config(ctx, s.DB, channelID, "channels")
	if e != nil {
		return nil, nil, e
	}
	if !Bool(ch, "enabled") {
		return nil, nil, nil
	}
	co, _, e := s.config(ctx, s.DB, Str(ch, "connector_id"), "connectors")
	if e != nil {
		return nil, nil, e
	}
	if connectorDisabled(co) || !contains([]string{"lark", "feishu"}, Str(co, "provider")) {
		return nil, nil, nil
	}
	refs, attachment, e := s.caseAttachmentContext(ctx, s.DB, channelID, Str(c, "id"), co)
	if e != nil || len(refs) == 0 {
		return nil, nil, e
	}
	cli, current, e := s.supportLark(ctx, s.DB, Str(co, "id"))
	if e != nil {
		return nil, nil, e
	}
	if Num(current, "version") != Num(co, "version") {
		return nil, nil, nil
	}
	images := []integration.InputImage{}
	meta := []Data{}
	total := 0
	for _, ref := range refs {
		if ref.Type != "image" || ref.Key == "" {
			continue
		}
		data, err := (integration.LarkSource{Client: cli}).ReadAttachment(ctx, Str(attachment, "message_id"), ref)
		if err != nil || len(data) == 0 || len(data) > MaxVisionImageBytes {
			continue
		}
		media, name, err := attachmentType(ref, data)
		if err != nil || !strings.HasPrefix(media, "image/") {
			continue
		}
		if total+len(data) > MaxVisionImageBytes*MaxVisionImages {
			break
		}
		images = append(images, integration.InputImage{ID: ref.Key, Name: name, MediaType: media, Data: data})
		meta = append(meta, Data{"id": ref.Key, "name": name, "media_type": media, "size": len(data), "message_id": Str(attachment, "message_id"), "source_revision": Str(attachment, "source_revision")})
		total += len(data)
		if len(images) == MaxVisionImages {
			break
		}
	}
	return images, meta, nil
}
func (s *Service) attachmentSnapshot(ctx context.Context, a Actor, id string, index int, expectedVersion int64) (attachmentSnapshot, error) {
	var out attachmentSnapshot
	p, e := one(ctx, s.DB, "SELECT to_jsonb(p) FROM principals p WHERE id::text=$1 AND active", a.ID)
	if e != nil {
		return out, Forbidden
	}
	a = actorFrom(p)
	c, e := s.Get(ctx, s.DB, a, "cases", id)
	if e != nil {
		return out, e
	}
	if Num(c, "version") != expectedVersion {
		return out, Conflict
	}
	channelID := Str(c, "channel_id")
	if !a.Channel(channelID) {
		return out, Forbidden
	}
	ch, _, e := s.config(ctx, s.DB, channelID, "channels")
	if e != nil {
		return out, e
	}
	if !Bool(ch, "enabled") {
		return out, Fail(409, "channel_disabled", "附件来源渠道已停用")
	}
	co, _, e := s.config(ctx, s.DB, Str(ch, "connector_id"), "connectors")
	if e != nil {
		return out, e
	}
	if connectorDisabled(co) || !contains([]string{"lark", "feishu"}, Str(co, "provider")) {
		return out, Fail(409, "attachment_connector", "此渠道没有可用的飞书 / Lark 接入配置，无法下载附件")
	}
	env := Object(c, "environment")
	refs := []integration.Attachment{}
	if e = json.Unmarshal(JSON(env["attachments"]), &refs); e != nil || index >= len(refs) {
		return out, NotFound
	}
	ref := refs[index]
	if ref.Key == "" || !contains([]string{"image", "file"}, ref.Type) {
		return out, Fail(415, "attachment_type", "不支持此附件类型，请到原始消息中查看")
	}
	if Str(co, "app_id") == "" || Str(co, "tenant_id") == "" || Str(env, "attachment_message_id") == "" || Str(env, "attachment_connector_id") != Str(co, "id") || Str(env, "attachment_app_id") != Str(co, "app_id") || Str(env, "attachment_tenant_id") != Str(co, "tenant_id") {
		return out, Fail(409, "attachment_source_changed", "附件的来源信息已变更，请刷新页面或重新同步消息")
	}
	message, e := one(ctx, s.DB, "SELECT to_jsonb(m) FROM source_messages m WHERE connector_id=$1 AND channel_id=$2 AND external_id=$3 AND case_id=$4 AND NOT deleted", Str(co, "id"), channelID, Str(env, "attachment_message_id"), id)
	if e != nil {
		return out, Fail(409, "attachment_source_changed", "原始消息已撤回或不再属于此问题")
	}
	if Str(message, "source_revision") == "" || Str(message, "source_revision") != Str(env, "attachment_source_revision") {
		return out, Conflict
	}
	var pending bool
	if e = s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM ingest_events WHERE connector_id=$1 AND payload->>'channel_id'=$2 AND payload->'event'->>'message_id'=$3 AND status<>'processed' AND payload->'event'->>'bot'='false')", Str(co, "id"), channelID, Str(message, "external_id")).Scan(&pending); e != nil {
		return out, e
	}
	if pending {
		return out, Fail(409, "attachment_source_pending", "原始消息正在更新或撤回，请刷新后重试")
	}
	// Case.environment is editable for factual context. Verify the selected key
	// against the signed/queried immutable event, not just that editable metadata.
	events, e := many(ctx, s.DB, "SELECT payload FROM ingest_events WHERE connector_id=$1 AND payload->>'channel_id'=$2 AND payload->'event'->>'message_id'=$3 AND payload->'event'->>'source_revision'=$4 ORDER BY created_at DESC LIMIT 10", Str(co, "id"), channelID, Str(message, "external_id"), Str(message, "source_revision"))
	if e != nil {
		return out, e
	}
	verified := false
	for _, payload := range events {
		event := integration.Event{}
		if json.Unmarshal(JSON(payload["event"]), &event) != nil || event.Deleted || event.AppID != Str(co, "app_id") || event.TenantID != Str(co, "tenant_id") {
			continue
		}
		for _, original := range event.Attachments {
			if original.Key == ref.Key && original.Type == ref.Type && original.Name == ref.Name {
				verified = true
				break
			}
		}
	}
	if !verified {
		return out, Fail(409, "attachment_reference_changed", "此附件与原始消息中的附件不一致，请刷新页面后重试")
	}
	return attachmentSnapshot{c: c, ch: ch, co: co, message: message, ref: ref, refsHash: Hash(JSON(env["attachments"]))}, nil
}
func attachmentType(ref integration.Attachment, data []byte) (string, string, error) {
	media := http.DetectContentType(data)
	name := path.Base(strings.ReplaceAll(ref.Name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		name = "attachment"
	}
	name = boundedText(name, 180)
	extension := strings.ToLower(path.Ext(name))
	suffix := ""
	switch media {
	case "image/png":
		suffix = ".png"
	case "image/jpeg":
		suffix = ".jpg"
	case "image/gif":
		suffix = ".gif"
	case "image/webp":
		suffix = ".webp"
	case "application/pdf":
		suffix = ".pdf"
	default:
		if ref.Type == "image" || !strings.HasPrefix(media, "text/plain") || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) || !contains([]string{"", ".txt", ".log", ".json", ".md", ".csv", ".yaml", ".yml"}, extension) {
			return "", "", Fail(415, "attachment_type", "不支持此附件格式。HTML、SVG、可执行文件和压缩包不在下载范围内")
		}
		trimmed := strings.ToLower(strings.TrimSpace(string(data)))
		if strings.HasPrefix(trimmed, "<svg") || strings.HasPrefix(trimmed, "<?xml") || strings.HasPrefix(trimmed, "<!doctype") {
			return "", "", Fail(415, "attachment_type", "不支持下载 XML 或 SVG 附件，请到原始消息中查看")
		}
		if extension == ".json" && !json.Valid(data) {
			return "", "", Fail(415, "attachment_type", "附件不是有效的 JSON 文件，无法下载")
		}
		media = "text/plain; charset=utf-8"
		suffix = ".txt"
	}
	if ref.Type == "image" && !strings.HasPrefix(media, "image/") {
		return "", "", Fail(415, "attachment_type", "附件内容与标记的图片类型不符，无法下载")
	}
	if suffix == ".txt" {
		if extension == "" {
			name += suffix
		}
	} else if extension != suffix {
		name = strings.TrimSuffix(name, path.Ext(name)) + suffix
	}
	return media, name, nil
}

var capabilityKeys = []string{"bot_identity", "repository_read", "receive_mentions", "receive_messages", "history_read", "thread_read", "attachment_read", "send", "card_callback"}

func newCapabilityMatrix() Data {
	out := Data{}
	for _, key := range capabilityKeys {
		out[key] = Data{"status": "unverified", "reason": "尚未检查", "channel_ids": []string{}}
	}
	Object(out, "send")["reason"] = "不会试发消息。人工批准的回复或已配置的通知首次发送成功后，会记录该渠道"
	Object(out, "card_callback")["reason"] = "收到真实卡片操作，并通过签名和身份检查后，会记录该渠道"
	Object(out, "attachment_read")["reason"] = "有权限的成员成功下载原始消息中的附件后，会记录该渠道"
	return out
}
func capabilityMatrix(c Data) Data {
	out := newCapabilityMatrix()
	old := Object(c, "capabilities")
	for _, key := range capabilityKeys {
		entry := Object(old, key)
		if contains([]string{"verified", "observed", "unverified", "unavailable"}, Str(entry, "status")) {
			out[key] = entry
		}
	}
	return out
}
func (s *Service) observeCapability(ctx context.Context, q DB, connectorID, channelID, key string) error {
	if !contains(capabilityKeys, key) {
		return Fail(422, "invalid_capability", "不支持此接入功能")
	}
	co, e := locked(ctx, q, "configs", connectorID)
	if e != nil {
		return e
	}
	if Str(co, "kind") != "connectors" || connectorDisabled(Object(co, "data")) {
		return Conflict
	}
	ch, _, e := s.config(ctx, q, channelID, "channels")
	if e != nil {
		return e
	}
	if !Bool(ch, "enabled") || Str(ch, "connector_id") != connectorID {
		return Conflict
	}
	c := configPublic(co)
	matrix := capabilityMatrix(c)
	entry := Object(matrix, key)
	ids := Strings(entry, "channel_ids")
	if !contains(ids, channelID) {
		ids = append(ids, channelID)
		sort.Strings(ids)
	}
	matrix[key] = Data{"status": "observed", "reason": "已在以下渠道成功使用过；其他渠道尚未确认，权限变更后仍可能失败", "verified_at": time.Now().UTC(), "channel_ids": ids}
	_, e = q.Exec(ctx, "UPDATE configs SET data=jsonb_set(data,'{capabilities}',$2::jsonb),updated_at=now() WHERE id=$1", connectorID, JSON(matrix))
	return e
}

// SendCapabilityObserved trusts only durable real successes for the exact current
// connector/channel versions, never a mutable display flag or a guessed scope.
func (s *Service) SendCapabilityObserved(ctx context.Context, q DB, channelID string) (bool, error) {
	ch, _, e := s.config(ctx, q, channelID, "channels")
	if e != nil {
		return false, e
	}
	if !Bool(ch, "enabled") {
		return false, nil
	}
	co, _, e := s.config(ctx, q, Str(ch, "connector_id"), "connectors")
	if e != nil {
		return false, e
	}
	if connectorDisabled(co) || Str(co, "status") == "failed" {
		return false, nil
	}
	var found bool
	e = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM external_operations o WHERE kind IN ('reply_send','notify') AND status='succeeded' AND target->>'channel_id'=$1 AND COALESCE(target->>'channel_version',request->>'channel_version')=$2 AND COALESCE(target->>'connector_version',request->>'connector_version')=$3)`, channelID, fmt.Sprint(Num(ch, "version")), fmt.Sprint(Num(co, "version"))).Scan(&found)
	return found, e
}

func capabilityReadFailure(e error) string {
	var api *integration.APIError
	if errors.As(e, &api) {
		return api.Diagnostic()
	}
	return "读取检查未完成，请检查接入配置和访问权限后重试"
}
func recordCapabilityProbe(matrix Data, probes *[]Data, key, channelID, externalID, status, reason string) {
	now := time.Now().UTC()
	*probes = append(*probes, Data{"capability": key, "channel_id": channelID, "external_id": externalID, "status": status, "reason": reason, "checked_at": now})
	entry := Object(matrix, key)
	if status == "verified" {
		ids := Strings(entry, "channel_ids")
		if !contains(ids, channelID) {
			ids = append(ids, channelID)
			sort.Strings(ids)
		}
		matrix[key] = Data{"status": "verified", "reason": "已在以下渠道读取成功，其他群或话题尚未检查；后续权限变更仍可能影响读取", "verified_at": now, "channel_ids": ids}
	} else if len(Strings(entry, "channel_ids")) == 0 {
		matrix[key] = Data{"status": status, "reason": reason, "channel_ids": []string{}}
	}
}
func (s *Service) probeConnectorCapabilities(ctx context.Context, c Data) (Data, map[string]int64, error) {
	id := Str(c, "id")
	matrix := capabilityMatrix(c)
	fresh := newCapabilityMatrix()
	for _, key := range []string{"bot_identity", "repository_read", "history_read", "thread_read"} {
		matrix[key] = fresh[key]
	}
	probes := []Data{}
	snapshots := map[string]int64{}
	result := Data{"status": "verified", "verified_at": time.Now().UTC(), "capabilities": matrix, "last_error": ""}
	adapter, _, err := s.sourceAdapter(ctx, s.DB, c)
	if err != nil {
		return nil, nil, err
	}
	result["supported_capabilities"] = decodeData(adapter.Describe())
	channels, e := many(ctx, s.DB, "SELECT to_jsonb(c) FROM configs c WHERE kind='channels' AND data->>'connector_id'=$1 AND data->>'enabled'='true' ORDER BY id LIMIT 20", id)
	if e != nil {
		return nil, nil, e
	}
	unavailable := func(key, reason string) {
		matrix[key] = Data{"status": "unavailable", "reason": reason, "channel_ids": []string{}}
	}
	switch Str(c, "provider") {
	case "feishu", "lark":
		unavailable("repository_read", "飞书 / Lark 接入不支持读取 GitHub 仓库，请另外配置 GitHub 接入")
		cli, _, err := s.supportLark(ctx, s.DB, id)
		if err == nil {
			var info integration.BotInfo
			info, err = cli.BotInfo(ctx)
			if err == nil {
				result["bot_open_id"] = info.OpenID
				matrix["bot_identity"] = Data{"status": "verified", "reason": "已确认应用机器人账号可用，各群的权限仍需分别检查", "verified_at": time.Now().UTC(), "channel_ids": []string{}}
			}
		}
		if err != nil {
			unavailable("bot_identity", capabilityReadFailure(err))
			result["status"] = "failed"
			result["last_error"] = capabilityReadFailure(err)
			break
		}
		for _, raw := range channels {
			ch := configPublic(raw)
			cid, external := Str(ch, "id"), Str(ch, "external_id")
			snapshots[cid] = Num(ch, "version")
			probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			page, err := cli.History(probeCtx, integration.HistoryRequest{ContainerType: "chat", ContainerID: external, PageSize: 10})
			if err == nil {
				for _, message := range page.Items {
					if message.ChatID != "" && message.ChatID != external {
						err = integration.ErrInvalidInput
						break
					}
				}
			}
			if err != nil {
				recordCapabilityProbe(matrix, &probes, "history_read", cid, external, "unavailable", capabilityReadFailure(err))
				cancel()
				continue
			}
			recordCapabilityProbe(matrix, &probes, "history_read", cid, external, "verified", "已读取此群的一页历史消息，话题消息需另行检查")
			thread := ""
			for _, message := range page.Items {
				if message.ThreadID != "" {
					thread = message.ThreadID
					break
				}
			}
			if thread == "" {
				recordCapabilityProbe(matrix, &probes, "thread_read", cid, external, "unverified", "本次读取的历史消息中没有话题 ID，暂时无法检查话题读取权限")
			} else {
				topic, err := cli.History(probeCtx, integration.HistoryRequest{ContainerType: "thread", ContainerID: thread, PageSize: 1})
				if err == nil {
					for _, message := range topic.Items {
						if message.ChatID != "" && message.ChatID != external {
							err = integration.ErrInvalidInput
							break
						}
					}
				}
				if err != nil {
					recordCapabilityProbe(matrix, &probes, "thread_read", cid, thread, "unavailable", capabilityReadFailure(err))
				} else {
					recordCapabilityProbe(matrix, &probes, "thread_read", cid, thread, "verified", "已读取此话题的一页消息，其他话题尚未检查")
				}
			}
			cancel()
		}
	case "github":
		for _, key := range []string{"bot_identity", "receive_mentions", "thread_read", "attachment_read", "card_callback"} {
			unavailable(key, "此功能用于飞书 / Lark 群机器人，不适用于 GitHub 接入")
		}
		cli, _, err := s.supportGitHub(ctx, s.DB, id, "connectors")
		var repo integration.GitHubRepository
		if err == nil {
			repo, err = cli.GetRepository(ctx)
		}
		if err != nil {
			unavailable("repository_read", capabilityReadFailure(err))
			result["status"] = "failed"
			result["last_error"] = capabilityReadFailure(err)
			break
		}
		result["repository"] = repo.FullName
		matrix["repository_read"] = Data{"status": "verified", "reason": "已读取配置中的仓库；本次未检查写入权限或其他仓库", "verified_at": time.Now().UTC(), "channel_ids": []string{}}
		if user, err := cli.AuthenticatedUser(ctx); err == nil {
			result["sender_id"] = strconv.FormatInt(user.ID, 10)
			result["sender_login"] = user.Login
		}
		for _, raw := range channels {
			ch := configPublic(raw)
			cid, external := Str(ch, "id"), Str(ch, "external_id")
			snapshots[cid] = Num(ch, "version")
			probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			var err error
			reason := "已读取此仓库的一页 Issue 列表"
			switch {
			case external == repo.FullName:
				_, err = cli.ListIssues(probeCtx, integration.IssueListRequest{State: "all", Page: 1, PerPage: 1})
			case strings.HasPrefix(external, "issue:") || strings.HasPrefix(external, "pull:"):
				number, parseErr := strconv.Atoi(strings.SplitN(external, ":", 2)[1])
				if parseErr != nil || number < 1 {
					err = integration.ErrInvalidInput
				} else {
					_, err = cli.GetIssue(probeCtx, number)
					if err == nil {
						_, err = cli.ListComments(probeCtx, number, 1)
					}
					reason = "已读取此 Issue 及一页评论"
				}
			default:
				err = integration.ErrInvalidInput
			}
			cancel()
			if err != nil {
				recordCapabilityProbe(matrix, &probes, "history_read", cid, external, "unavailable", capabilityReadFailure(err))
			} else {
				recordCapabilityProbe(matrix, &probes, "history_read", cid, external, "verified", reason)
			}
		}
	default:
		result["status"] = "failed"
		result["last_error"] = "不支持此平台，请检查接入配置中的平台选择"
	}
	result["capability_probes"] = probes
	if len(channels) == 20 {
		entry := Object(matrix, "history_read")
		entry["reason"] = Str(entry, "reason") + "；每次最多检查 20 个已配置且启用的渠道，未列出结果的渠道尚未确认"
	}
	return result, snapshots, nil
}

func (s *Service) observeCapabilityVersion(ctx context.Context, connectorID, channelID, key string, connectorVersion, channelVersion int64) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		co, e := locked(ctx, tx, "configs", connectorID)
		if e != nil {
			return e
		}
		ch, _, e := s.config(ctx, tx, channelID, "channels")
		if e != nil {
			return e
		}
		if Num(co, "version") != connectorVersion || Num(ch, "version") != channelVersion {
			return nil
		}
		return s.observeCapability(ctx, tx, connectorID, channelID, key)
	})
}
