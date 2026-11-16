package service

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

var githubName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}$`)
var secretFields = []string{"token", "callback_token", "app_secret", "verification_token", "encrypt_key", "webhook_secret"}

func (s *Service) SaveConfig(ctx context.Context, tx pgx.Tx, a Actor, kind, id string, b Data) (Data, error) {
	if kind == "admission-rules" {
		if !a.Has("admin") {
			return nil, Forbidden
		}
		return nil, Fail(410, "policy_moved", "请在审批策略中配置创建开发任务")
	}
	if err := lockExecutionSettings(ctx, tx); err != nil {
		return nil, err
	}
	if !a.Has("admin") && !(kind == "knowledge-sources" && a.Has("knowledge")) {
		return nil, Forbidden
	}
	if !configKinds[kind] {
		return nil, NotFound
	}
	create := id == ""
	old := Data{}
	previous := Data{}
	secrets := Data{}
	var configVersion int64
	if create {
		id = ID()
	} else {
		raw, e := locked(ctx, tx, "configs", id)
		if e != nil {
			return nil, e
		}
		if Str(raw, "kind") != kind {
			return nil, NotFound
		}
		if e = checkVersion(raw, b); e != nil {
			return nil, e
		}
		configVersion = Num(raw, "version")
		old = Object(raw, "data")
		for k, v := range old {
			previous[k] = v
		}
		if Str(raw, "secrets") != "" {
			v, e := s.Vault.Open(Str(raw, "secrets"), []byte(kind+":"+id))
			if e != nil {
				return nil, e
			}
			if e = json.Unmarshal(v, &secrets); e != nil {
				return nil, e
			}
		}
		old["name"] = Str(raw, "name")
	}
	for k, v := range b {
		if contains([]string{"id", "version", "expected_version", "created_at", "updated_at", "secret_configured", "approval_version"}, k) {
			continue
		}
		if kind == "connectors" && (contains([]string{"capabilities", "capability_probes", "last_error", "verified_at", "bot_open_id", "sender_id", "repository", "ws_status", "ws_error", "ws_checked_at"}, k) || k == "status" && !contains([]string{"disabled", "unverified"}, Str(b, "status"))) {
			continue
		}
		if kind == "channels" && contains([]string{"poll_started_at", "poll_cursor", "poll_repository", "poll_last_attempt_at", "last_polled_at", "poll_last_error", "poll_source", "poll_batch_id", "poll_batch_end", "poll_batch_version", "poll_connector_version"}, k) {
			continue
		}
		if contains(secretFields, k) {
			if x, ok := v.(string); ok && x != "" {
				secrets[k] = x
			}
			continue
		}
		old[k] = v
	}
	if kind == "identity-bindings" {
		delete(old, "tenant_id")
		delete(old, "display_name")
		delete(old, "verified_at")
		old["name"] = strings.Join([]string{Str(old, "provider"), Str(old, "app_id"), Str(old, "subject_id")}, ":")
	}
	if kind == "owner-routes" {
		old["name"] = Str(old, "module")
	}
	if e := require(old, "name"); e != nil {
		return nil, e
	}
	if len(Str(old, "name")) > 200 {
		return nil, Fail(422, "validation", "名称过长")
	}
	policyChanged := create || Hash(JSON(policyAuthority(previous))) != Hash(JSON(policyAuthority(old)))
	switch kind {
	case "connectors":
		if value, ok := old["user_login_enabled"]; ok {
			if _, valid := value.(bool); !valid {
				return nil, Fail(422, "validation", "飞书登录开关必须为布尔值")
			}
		}
		if Bool(old, "user_login_enabled") && (!contains([]string{"feishu", "lark"}, Str(old, "provider")) || Str(old, "app_id") == "" || Str(secrets, "app_secret") == "") {
			return nil, Fail(422, "validation", "开启飞书登录需要应用 ID 和应用密钥")
		}
		if !contains([]string{"lark", "feishu", "github"}, Str(old, "provider")) {
			return nil, Fail(422, "validation", "provider 必须为 lark、feishu 或 github")
		}
		if Str(old, "status") != "disabled" {
			old["status"] = "unverified"
		}
		old["capabilities"] = newCapabilityMatrix()
		old["capability_probes"] = []Data{}
		for _, field := range []string{"last_error", "verified_at", "bot_open_id", "sender_id", "repository"} {
			delete(old, field)
		}
		if v, ok := old["ws_enabled"]; ok {
			if _, valid := v.(bool); !valid {
				return nil, Fail(422, "validation", "长连接开关必须为布尔值")
			}
		}
		if Bool(old, "ws_enabled") {
			if !contains([]string{"lark", "feishu"}, Str(old, "provider")) || Str(old, "app_id") == "" || Str(secrets, "app_secret") == "" {
				return nil, Fail(422, "validation", "开启长连接需要飞书 / Lark 应用 ID 和应用密钥")
			}
		}
		for _, field := range []string{"ws_status", "ws_error", "ws_checked_at"} {
			delete(old, field)
		}
		if Str(old, "base_url") != "" {
			u, e := url.Parse(Str(old, "base_url"))
			if e != nil || u.Scheme != "https" || u.User != nil {
				return nil, Fail(422, "validation", "平台地址必须为无凭据的 HTTPS URL")
			}
		}
	case "channels":
		if e := require(old, "connector_id", "external_id"); e != nil {
			return nil, e
		}
		connector, _, e := s.config(ctx, tx, Str(old, "connector_id"), "connectors")
		if e != nil {
			return nil, e
		}
		interval, e := channelPollInterval(old)
		if e != nil {
			return nil, e
		}
		provider := Str(connector, "provider")
		if interval > 0 && (!contains([]string{"github", "feishu", "lark"}, provider) || provider == "github" && strings.HasPrefix(Str(old, "external_id"), "pull:")) {
			return nil, Fail(422, "validation", "定时同步支持飞书群、Lark 群和 GitHub Issue 渠道")
		}
		old["poll_interval_minutes"] = interval
		if interval > 0 && Bool(old, "enabled") && (Num(previous, "poll_interval_minutes") == 0 || !Bool(previous, "enabled") || Str(previous, "connector_id") != Str(old, "connector_id") || Str(previous, "external_id") != Str(old, "external_id")) {
			now := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339Nano)
			if provider == "github" {
				owner, repo := Str(connector, "owner"), Str(connector, "repo")
				if rid := Str(connector, "repository_id"); rid != "" {
					r, _, err := s.config(ctx, tx, rid, "repositories")
					if err != nil {
						return nil, err
					}
					if !Bool(r, "enabled") {
						return nil, Fail(409, "repository_disabled", "仓库未启用")
					}
					owner, repo = Str(r, "owner"), Str(r, "repo")
				}
				if !githubName.MatchString(owner) || !githubName.MatchString(repo) {
					return nil, Fail(422, "validation", "请先正确配置 GitHub 仓库")
				}
				old["poll_started_at"], old["poll_cursor"] = now, now
				old["poll_repository"] = owner + "/" + repo
			} else {
				cursor := ""
				if Str(previous, "connector_id") == Str(old, "connector_id") && Str(previous, "external_id") == Str(old, "external_id") {
					// Manual history can record its root watermark before topic jobs
					// finish. Do not trust it while any history job is unfinished.
					var incomplete bool
					if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE kind='sync_channel' AND payload->>'channel_id'=$1 AND status IN ('pending','running','failed'))`, id).Scan(&incomplete); err != nil {
						return nil, err
					}
					if !incomplete {
						cursor = Str(previous, "last_synced_at")
					}
				}
				old["poll_cursor"] = cursor
				old["poll_source"] = larkPollSource(connector, old)
				old["poll_started_at"] = now
			}
			for _, field := range []string{"poll_last_attempt_at", "last_polled_at", "poll_last_error", "poll_batch_id", "poll_batch_end", "poll_batch_version", "poll_connector_version"} {
				delete(old, field)
			}
		}
		if create {
			old["reply_mode"] = fallback(Str(old, "reply_mode"), "draft")
			old["visibility"] = fallback(Str(old, "visibility"), "internal")
			old["kind"] = fallback(Str(old, "kind"), "chat")
		}
		if !contains([]string{"draft", "mentions", "auto", "manual"}, Str(old, "reply_mode")) {
			return nil, Fail(422, "validation", "无效回复模式")
		}
	case "repositories":
		if _, ok := old["pr_poll_interval_minutes"]; !ok {
			old["pr_poll_interval_minutes"] = 5
		}
		interval, ok := boundedSettingInteger(old["pr_poll_interval_minutes"], 0, 1440)
		if !ok {
			return nil, Fail(422, "validation", "PR 同步间隔必须是 0 到 1440 的整数")
		}
		old["pr_poll_interval_minutes"] = interval
		if !githubName.MatchString(Str(old, "owner")) || !githubName.MatchString(Str(old, "repo")) {
			return nil, Fail(422, "validation", "GitHub owner/repo 无效")
		}
		old["base_branch"] = fallback(Str(old, "base_branch"), "main")
		for _, p := range Strings(old, "allowed_paths") {
			if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
				return nil, Fail(422, "validation", "允许路径必须是仓库内的规范相对路径")
			}
		}
		for key, value := range Object(Object(old, "config"), "test_env") {
			text, ok := value.(string)
			if !ok || len(text) > 4096 || strings.ContainsRune(text, 0) || !contains([]string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOPROXY", "GOSUMDB", "CGO_ENABLED", "GOOS", "GOARCH", "HOME", "TMPDIR", "NPM_CONFIG_CACHE", "CI"}, key) {
				return nil, Fail(422, "validation", "测试环境只允许离线工具链与缓存配置")
			}
		}
		old["url"] = "https://github.com/" + Str(old, "owner") + "/" + Str(old, "repo")
	case "policies":
		if !contains([]string{"create_task", "reply_send", "knowledge_publish", "plan", "edit", "test", "review", "publish_pr"}, Str(old, "kind")) {
			return nil, Fail(422, "validation", "不支持的策略动作")
		}
		if !contains([]string{"deny", "manual", "auto"}, Str(old, "mode")) {
			return nil, Fail(422, "validation", "无效策略模式")
		}
		kind := Str(old, "kind")
		development := developmentApprovalKind(kind)
		if kind != "reply_send" {
			old["channel_ids"] = []string{}
		}
		if !development {
			old["repository_ids"] = []string{}
			if kind != "create_task" {
				old["allowed_paths"] = []string{}
			}
		}
		if kind == "create_task" && policyChanged {
			if Str(old, "mode") == "deny" {
				for field, resource := range map[string]string{"channel_id": "channels", "repository_id": "repositories"} {
					if !validID(Str(old, field)) {
						return nil, Fail(422, "validation", "请选择渠道和仓库")
					}
					if _, _, e := s.config(ctx, tx, Str(old, field), resource); e != nil {
						return nil, e
					}
				}
				for _, key := range []string{"sponsor_principal_id", "condition", "followup_mode", "sender_types", "allow_bot", "allowed_paths"} {
					delete(old, key)
				}
			} else if Bool(old, "enabled") {
				if e := s.validateAdmissionRule(ctx, tx, old); e != nil {
					return nil, e
				}
			}
		} else if kind != "create_task" {
			for _, key := range []string{"channel_id", "repository_id", "sponsor_principal_id", "condition", "followup_mode", "sender_types", "allow_bot"} {
				delete(old, key)
			}
		}
		if !contains([]string{"review", "publish_pr"}, kind) {
			old["require_tests"] = false
		}
		if kind != "create_task" && Str(old, "mode") == "auto" && (Num(old, "max_tokens") < 1 || Num(old, "max_seconds") < 1 || Num(old, "max_files") < 1) {
			return nil, Fail(422, "validation", "自动审批必须设置文件数、Token 和时长上限")
		}
		if value, exists := old["notification_enabled"]; exists {
			if _, ok := value.(bool); !ok {
				return nil, Fail(422, "validation", "通知开关必须为布尔值")
			}
		} else {
			old["notification_enabled"] = true
		}
		if value, exists := old["notification_channel_id"]; exists {
			if _, ok := value.(string); !ok {
				return nil, Fail(422, "validation", "请选择有效的通知渠道")
			}
		}
		if channel := Str(old, "notification_channel_id"); notificationEnabled(old) && channel != "" {
			if !validID(channel) {
				return nil, Fail(422, "validation", "请选择有效的通知渠道")
			}
			ch, _, err := s.config(ctx, tx, channel, "channels")
			if err != nil {
				return nil, err
			}
			if !Bool(ch, "enabled") || Str(ch, "visibility") != "internal" {
				return nil, Fail(422, "validation", "审批通知只能发送到启用的内部渠道")
			}
		}
		if policyChanged {
			old["approval_version"] = configVersion + 1
		} else {
			old["approval_version"] = policyVersion(Data{"approval_version": previous["approval_version"], "version": configVersion})
		}
		if v := Str(old, "expires_at"); v != "" {
			if _, e := time.Parse(time.RFC3339, v); e != nil {
				return nil, Fail(422, "validation", "策略有效期无效")
			}
		}
	case "owner-routes":
		if e := require(old, "module", "primary_id"); e != nil {
			return nil, e
		}
		if !validID(Str(old, "primary_id")) {
			return nil, Fail(422, "validation", "负责人无效")
		}
		if Num(old, "claim_minutes") < 1 {
			old["claim_minutes"] = 15
		}
		if Num(old, "resolve_minutes") < 1 {
			old["resolve_minutes"] = 240
		}
	case "identity-bindings":
		if e := require(old, "principal_id", "provider", "app_id", "subject_id"); e != nil {
			return nil, e
		}
		if !contains([]string{"lark", "feishu"}, Str(old, "provider")) {
			return nil, Fail(422, "validation", "身份绑定平台必须为 lark 或 feishu")
		}
		if !validID(Str(old, "principal_id")) {
			return nil, Fail(422, "validation", "成员无效")
		}
		if len(Str(old, "app_id")) > 200 || len(Str(old, "subject_id")) > 200 {
			return nil, Fail(422, "validation", "应用或账号标识过长")
		}
		if e := validateIdentityBinding(ctx, tx, id, old); e != nil {
			return nil, e
		}
		if _, e := tx.Exec(ctx, "DELETE FROM sessions WHERE identity_binding_id=$1", id); e != nil {
			return nil, e
		}
	case "knowledge-sources":
		if !contains([]string{"repository", "manual", "lark"}, Str(old, "kind")) {
			return nil, Fail(422, "validation", "知识来源类型必须为 repository、manual 或 lark")
		}
		if Str(old, "status") != "revoked" {
			old["status"] = "pending"
		}
	case "runtime-machines":
		if e := require(old, "name", "base_url", "callback_base_url"); e != nil {
			return nil, e
		}
		for _, field := range []string{"base_url", "callback_base_url"} {
			u, err := url.Parse(Str(old, field))
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return nil, Fail(422, "validation", "Runtime Machine 地址必须为无凭据的 HTTP(S) URL")
			}
			old[field] = strings.TrimRight(Str(old, field), "/")
		}
		status := fallback(Str(old, "status"), "active")
		if !contains([]string{"active", "degraded", "draining", "disabled"}, status) {
			return nil, Fail(422, "validation", "Runtime Machine 状态无效")
		}
		old["status"] = status
		if Num(old, "scheduling_weight") < 1 || Num(old, "scheduling_weight") > 1000 {
			return nil, Fail(422, "validation", "调度权重必须为 1 到 1000 的整数")
		}
		for _, image := range Strings(old, "allowed_images") {
			if !strings.Contains(image, "@sha256:") {
				return nil, Fail(422, "validation", "Runtime Machine 镜像白名单必须使用 digest 固定")
			}
		}
		for _, secretKey := range []string{"token", "callback_token"} {
			if value := Str(secrets, secretKey); value != "" && len(value) < 32 {
				return nil, Fail(422, "validation", "Runtime Machine 令牌至少为 32 个字符")
			}
		}
	}
	enc := ""
	var e error
	if len(secrets) > 0 {
		enc, e = s.Vault.Seal(JSON(secrets), []byte(kind+":"+id))
		if e != nil {
			return nil, e
		}
	}
	if create {
		_, e = tx.Exec(ctx, "INSERT INTO configs(id,kind,name,data,secrets) VALUES($1,$2,$3,$4,$5)", id, kind, Str(old, "name"), JSON(old), enc)
	} else {
		_, e = tx.Exec(ctx, "UPDATE configs SET name=$2,data=$3,secrets=$4,version=version+1,updated_at=now() WHERE id=$1", id, Str(old, "name"), JSON(old), enc)
	}
	if e != nil {
		return nil, e
	}
	raw, e := locked(ctx, tx, "configs", id)
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO config_versions(config_id,version,data,actor_id) VALUES($1,$2,$3,$4)", id, Num(raw, "version"), JSON(old), a.ID); e != nil {
		return nil, e
	}
	if kind == "policies" && policyChanged {
		if _, e = tx.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE (policy_id=$1 OR (kind='create_task' AND action->>'rule_id'=$1)) AND status IN ('pending','approved')", id); e != nil {
			return nil, e
		}
	}
	if kind == "knowledge-sources" && !create {
		if e = s.withdrawSource(ctx, tx, id); e != nil {
			return nil, e
		}
	}

	if e = audit(ctx, tx, a, "save", kind, id, ""); e != nil {
		return nil, e
	}
	if !create && (kind != "policies" || policyChanged) && contains([]string{"repositories", "channels", "connectors", "policies"}, kind) {
		if e = s.revalidateTaskAuthorities(ctx, tx, id); e != nil {
			return nil, e
		}
	}
	return configPublic(raw), nil
}

func (s *Service) DeleteConfig(ctx context.Context, tx pgx.Tx, a Actor, kind, id string, b Data) (Data, error) {
	if kind == "admission-rules" {
		if !a.Has("admin") {
			return nil, Forbidden
		}
		return nil, Fail(410, "policy_moved", "请在审批策略中配置创建开发任务")
	}
	if err := lockExecutionSettings(ctx, tx); err != nil {
		return nil, err
	}
	if !a.Has("admin") && !(kind == "knowledge-sources" && a.Has("knowledge")) {
		return nil, Forbidden
	}
	if !configKinds[kind] {
		return nil, NotFound
	}
	raw, e := locked(ctx, tx, "configs", id)
	if e != nil {
		return nil, e
	}
	if Str(raw, "kind") != kind {
		return nil, NotFound
	}
	if e = checkVersion(raw, b); e != nil {
		return nil, e
	}
	force := Bool(b, "force")
	if force {
		if e = s.detachConfigReferences(ctx, tx, kind, id); e != nil {
			return nil, e
		}
	} else if e = s.ensureConfigDeletable(ctx, tx, kind, id); e != nil {
		return nil, e
	}
	if kind == "policies" {
		if _, e = tx.Exec(ctx, "UPDATE approvals SET status='stale',version=version+1,updated_at=now() WHERE (policy_id=$1 OR (kind='create_task' AND action->>'rule_id'=$1)) AND status IN ('pending','approved')", id); e != nil {
			return nil, e
		}
	}
	if _, e = tx.Exec(ctx, "DELETE FROM config_versions WHERE config_id=$1", id); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM configs WHERE id=$1 AND kind=$2", id, kind); e != nil {
		return nil, e
	}
	if e = audit(ctx, tx, a, "delete", kind, id, map[bool]string{true: "force", false: ""}[force]); e != nil {
		return nil, e
	}
	if e = s.revalidateTaskAuthorities(ctx, tx, id); e != nil {
		return nil, e
	}
	return Data{"id": id, "deleted": true}, nil
}

func (s *Service) detachConfigReferences(ctx context.Context, q DB, kind, id string) error {
	switch kind {
	case "channels":
		if _, e := q.Exec(ctx, `UPDATE configs SET data=jsonb_set(data,'{notification_enabled}','false'::jsonb),version=version+1,updated_at=now() WHERE kind='policies' AND data->>'notification_channel_id'=$1`, id); e != nil {
			return e
		}
		if _, e := q.Exec(ctx, `UPDATE configs SET data=jsonb_set(data,'{enabled}','false'::jsonb)||jsonb_build_object('approval_version',version+1),version=version+1,updated_at=now() WHERE kind='policies' AND data->>'kind'='create_task' AND data->>'channel_id'=$1`, id); e != nil {
			return e
		}
		if _, e := q.Exec(ctx, "UPDATE cases SET channel_id='',version=version+1,updated_at=now() WHERE channel_id=$1", id); e != nil {
			return e
		}
		if _, e := q.Exec(ctx, "UPDATE configs SET data=data-'channel_id',version=version+1,updated_at=now() WHERE kind='owner-routes' AND data->>'channel_id'=$1", id); e != nil {
			return e
		}
		if _, e := q.Exec(ctx, `UPDATE configs SET data=jsonb_set(data,'{channel_ids}',COALESCE((SELECT jsonb_agg(value) FROM jsonb_array_elements_text(data->'channel_ids') AS ids(value) WHERE value<>$1),'[]'::jsonb))||jsonb_build_object('approval_version',version+1),version=version+1,updated_at=now() WHERE kind='policies' AND data->'channel_ids' ? $1`, id); e != nil {
			return e
		}
		if _, e := q.Exec(ctx, "UPDATE knowledge SET channel_ids=array_remove(channel_ids,$1),version=version+1,updated_at=now() WHERE $1=ANY(channel_ids)", id); e != nil {
			return e
		}
		if _, e := q.Exec(ctx, "UPDATE principals SET channel_ids=array_remove(channel_ids,$1),version=version+1,updated_at=now() WHERE $1=ANY(channel_ids)", id); e != nil {
			return e
		}
		return nil
	case "policies", "owner-routes", "identity-bindings":
		return nil
	default:
		return s.ensureConfigDeletable(ctx, q, kind, id)
	}
}

func (s *Service) ensureConfigDeletable(ctx context.Context, q DB, kind, id string) error {
	used := func(sql, message string, args ...any) error {
		var found bool
		if e := q.QueryRow(ctx, sql, args...).Scan(&found); e != nil {
			return e
		}
		if found {
			return Fail(409, "config_in_use", message)
		}
		return nil
	}
	switch kind {
	case "connectors":
		return used(`SELECT EXISTS(
			SELECT 1 FROM configs WHERE kind IN ('channels','repositories','knowledge-sources') AND (data->>'connector_id'=$1 OR data->>'repository_id'=$1)
			UNION ALL SELECT 1 FROM source_messages WHERE connector_id=$1::uuid
			UNION ALL SELECT 1 FROM ingest_events WHERE connector_id=$1::uuid
			UNION ALL SELECT 1 FROM external_operations WHERE target->>'connector_id'=$1 OR request->>'connector_id'=$1
		)`, "已有渠道、仓库、知识来源、历史消息或平台操作使用该平台账号，请先解除关联或停用", id)
	case "channels":
		return used(`SELECT EXISTS(
			SELECT 1 FROM cases WHERE channel_id=$1
			UNION ALL SELECT 1 FROM source_messages WHERE channel_id=$1
			UNION ALL SELECT 1 FROM configs WHERE kind='owner-routes' AND data->>'channel_id'=$1
			UNION ALL SELECT 1 FROM configs WHERE kind='policies' AND (data->'channel_ids' ? $1 OR data->>'channel_id'=$1 OR data->>'notification_channel_id'=$1)
			UNION ALL SELECT 1 FROM knowledge WHERE $1=ANY(channel_ids)
			UNION ALL SELECT 1 FROM principals WHERE $1=ANY(channel_ids)
			UNION ALL SELECT 1 FROM external_operations WHERE target->>'channel_id'=$1
		)`, "已有问题、消息、通知路由、权限或平台操作使用该渠道，请先解除关联或停用", id)
	case "repositories":
		return used(`SELECT EXISTS(
			SELECT 1 FROM dev_tasks WHERE repository_id=$1::uuid
			UNION ALL SELECT 1 FROM configs WHERE kind IN ('connectors','knowledge-sources') AND data->>'repository_id'=$1
			UNION ALL SELECT 1 FROM configs WHERE kind='policies' AND (data->'repository_ids' ? $1 OR data->>'repository_id'=$1)
			UNION ALL SELECT 1 FROM principals WHERE $1=ANY(repository_ids)
			UNION ALL SELECT 1 FROM external_operations WHERE target->>'repository_id'=$1 OR request->>'repository_id'=$1
		)`, "已有开发任务、账号、知识来源、权限或平台操作使用该仓库，请先解除关联或停用", id)
	case "knowledge-sources":
		return used("SELECT EXISTS(SELECT 1 FROM knowledge WHERE source_id=$1)", "已有知识条目来自该来源，请先撤回或改绑相关知识", id)
	case "runtime-machines":
		return used("SELECT EXISTS(SELECT 1 FROM run_attempts WHERE runtime_machine_id=$1::uuid)", "已有运行记录绑定该机器，请改为停用或排空后再删除", id)
	}
	return nil
}

func channelPollInterval(m Data) (int64, error) {
	v, ok := m["poll_interval_minutes"]
	if !ok || v == nil {
		return 0, nil
	}
	var number float64
	switch x := v.(type) {
	case float64:
		number = x
	case int:
		number = float64(x)
	case int64:
		number = float64(x)
	case json.Number:
		var e error
		number, e = x.Float64()
		if e != nil {
			return 0, Fail(422, "validation", "轮询间隔必须是 0 到 1440 的整数")
		}
	default:
		return 0, Fail(422, "validation", "轮询间隔必须是 0 到 1440 的整数")
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > 1440 || math.Trunc(number) != number {
		return 0, Fail(422, "validation", "轮询间隔必须是 0 到 1440 的整数")
	}
	return int64(number), nil
}

func (s *Service) SaveUser(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if err := lockExecutionSettings(ctx, tx); err != nil {
		return nil, err
	}
	if !a.Has("admin") {
		return nil, Forbidden
	}
	create := id == ""
	m := Data{"role": "viewer", "roles": []string{}, "channel_ids": []string{}, "repository_ids": []string{}, "active": true}
	if create {
		id = ID()
	} else {
		var e error
		m, e = locked(ctx, tx, "principals", id)
		if e != nil {
			return nil, e
		}
		if e = checkVersion(m, b); e != nil {
			return nil, e
		}
	}
	for _, k := range []string{"name", "email", "role", "roles", "channel_ids", "repository_ids", "active", "subject"} {
		if v, ok := b[k]; ok {
			m[k] = v
		}
	}
	if e := require(m, "name", "email"); e != nil {
		return nil, e
	}
	if !strings.Contains(Str(m, "email"), "@") {
		return nil, Fail(422, "validation", "邮箱格式无效")
	}
	if !contains([]string{"admin", "developer", "oncall", "knowledge", "viewer"}, Str(m, "role")) {
		return nil, Fail(422, "validation", "角色无效")
	}
	for _, r := range Strings(m, "roles") {
		if !contains([]string{"admin", "developer", "oncall", "knowledge", "viewer"}, r) {
			return nil, Fail(422, "validation", "角色无效")
		}
	}
	if id == a.ID && (!Bool(m, "active") || Str(m, "role") != "admin" && !contains(Strings(m, "roles"), "admin")) {
		return nil, Fail(422, "validation", "不能撤销自己的管理员权限")
	}
	hash := Str(m, "password_hash")
	if pw := Str(b, "password"); pw != "" {
		if len(pw) < 12 || len(pw) > 72 {
			return nil, Fail(422, "validation", "密码长度必须为 12～72 字节")
		}
		h, e := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if e != nil {
			return nil, e
		}
		hash = string(h)
	}
	if create && s.Config.AuthMode == "local" && hash == "" {
		return nil, Fail(422, "validation", "本地账号需要设置密码")
	}
	var subject any
	if Str(m, "subject") != "" {
		subject = Str(m, "subject")
	}
	var e error
	if create {
		_, e = tx.Exec(ctx, "INSERT INTO principals(id,name,email,role,roles,channel_ids,repository_ids,password_hash,subject,active) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", id, Str(m, "name"), strings.ToLower(Str(m, "email")), Str(m, "role"), Strings(m, "roles"), Strings(m, "channel_ids"), Strings(m, "repository_ids"), hash, subject, Bool(m, "active"))
	} else {
		_, e = tx.Exec(ctx, "UPDATE principals SET name=$2,email=$3,role=$4,roles=$5,channel_ids=$6,repository_ids=$7,password_hash=$8,subject=$9,active=$10,version=version+1,updated_at=now() WHERE id=$1", id, Str(m, "name"), strings.ToLower(Str(m, "email")), Str(m, "role"), Strings(m, "roles"), Strings(m, "channel_ids"), Strings(m, "repository_ids"), hash, subject, Bool(m, "active"))
	}
	if e != nil {
		return nil, e
	}
	if !create {
		if _, e = tx.Exec(ctx, "DELETE FROM sessions WHERE principal_id=$1", id); e != nil {
			return nil, e
		}
	}
	if e = audit(ctx, tx, a, "save", "principals", id, ""); e != nil {
		return nil, e
	}
	if !create {
		if e = s.revalidateTaskAuthorities(ctx, tx, id); e != nil {
			return nil, e
		}
	}
	return one(ctx, tx, "SELECT to_jsonb(p)-'password_hash' FROM principals p WHERE id=$1", id)
}

func (s *Service) DeleteUser(ctx context.Context, tx pgx.Tx, a Actor, id string, b Data) (Data, error) {
	if !a.Has("admin") {
		return nil, Forbidden
	}
	if id == a.ID {
		return nil, Fail(422, "validation", "不能删除自己的账号")
	}
	m, e := locked(ctx, tx, "principals", id)
	if e != nil {
		return nil, e
	}
	if e = checkVersion(m, b); e != nil {
		return nil, e
	}
	if (Str(m, "role") == "admin" || contains(Strings(m, "roles"), "admin")) && Bool(m, "active") {
		var admins int
		if e = tx.QueryRow(ctx, "SELECT count(*) FROM principals WHERE active AND (role='admin' OR 'admin'=ANY(roles)) AND id<>$1", id).Scan(&admins); e != nil {
			return nil, e
		}
		if admins == 0 {
			return nil, Fail(422, "validation", "不能删除最后一个管理员")
		}
	}
	var used bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM cases WHERE created_by=$1 OR assignee_id=$1
		UNION ALL SELECT 1 FROM knowledge WHERE author_id=$1
		UNION ALL SELECT 1 FROM approvals WHERE actor_id=$1
		UNION ALL SELECT 1 FROM approval_decisions WHERE actor_id=$1
		UNION ALL SELECT 1 FROM dev_tasks WHERE confirmed_by=$1
		UNION ALL SELECT 1 FROM configs WHERE kind='identity-bindings' AND data->>'principal_id'=$1
		UNION ALL SELECT 1 FROM configs WHERE kind='owner-routes' AND (data->>'primary_id'=$1 OR data->>'backup_id'=$1)
	)`, id).Scan(&used); e != nil {
		return nil, e
	}
	if used {
		return nil, Fail(409, "principal_in_use", "已有问题、知识、审批或路由使用该成员，请先解除关联或停用账号")
	}
	if _, e = tx.Exec(ctx, "DELETE FROM sessions WHERE principal_id=$1", id); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM principals WHERE id=$1", id); e != nil {
		return nil, e
	}
	if e = audit(ctx, tx, a, "delete", "principals", id, ""); e != nil {
		return nil, e
	}
	return Data{"id": id, "deleted": true}, nil
}
