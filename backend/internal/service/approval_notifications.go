package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

func policyVersion(p Data) int64 {
	if version := Num(p, "approval_version"); version > 0 {
		return version
	}
	return Num(p, "version")
}

func policyAuthority(p Data) Data {
	d := Data{}
	for _, key := range []string{"kind", "mode", "expires_at", "channel_id", "repository_id", "sponsor_principal_id", "condition", "followup_mode"} {
		d[key] = Str(p, key)
	}
	for _, key := range []string{"enabled", "require_tests", "allow_bot"} {
		d[key] = Bool(p, key)
	}
	for _, key := range []string{"max_files", "max_tokens", "max_seconds"} {
		d[key] = Num(p, key)
	}
	for _, key := range []string{"channel_ids", "repository_ids", "allowed_paths", "sender_types"} {
		d[key] = Strings(p, key)
	}
	return d
}

func (s *Service) approvalNotificationPolicy(ctx context.Context, q DB, ap Data) (Data, error) {
	id := fallback(Str(Object(ap, "action"), "notification_policy_id"), Str(ap, "policy_id"))
	if id == "" {
		_, p, err := s.evaluatePolicy(ctx, q, Str(ap, "kind"), Object(ap, "action"))
		return p, err
	}
	p, _, err := s.config(ctx, q, id, "policies")
	if err == NotFound {
		return Data{"notification_enabled": false}, nil
	}
	return p, err
}

func notificationEnabled(p Data) bool {
	enabled, ok := p["notification_enabled"].(bool)
	return !ok || enabled
}

var approvalNotificationLabels = map[string]string{
	"pending": "待审批", "auto_approved": "已自动批准", "denied": "策略禁止执行",
	"approved": "审批已通过", "rejected": "审批已拒绝", "revoked": "批准已撤销", "blocked": "任务建议需补充信息",
}

func (s *Service) enqueueApprovalNotification(ctx context.Context, tx pgx.Tx, ap Data, event string) error {
	policy, err := s.approvalNotificationPolicy(ctx, tx, ap)
	if err != nil {
		return err
	}
	if !notificationEnabled(policy) {
		return nil
	}
	key := "approval-notify:" + Str(ap, "id")
	if event != "pending" {
		key += fmt.Sprintf(":%s:%d", event, Num(ap, "version"))
	}
	return enqueue(ctx, tx, "notify", key, Data{"approval_id": Str(ap, "id"), "case_id": Str(Object(ap, "action"), "case_id"), "approval_event": event, "approval_version": Num(ap, "version"), "event_at": time.Now().UTC().Format(time.RFC3339), "reason": Str(ap, "reason")}, 0)
}

func (s *Service) queueApprovalNotification(ctx context.Context, p Data) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		ap, err := locked(ctx, tx, "approvals", Str(p, "approval_id"))
		if err != nil {
			return err
		}
		event := fallback(Str(p, "approval_event"), "pending")
		label := approvalNotificationLabels[event]
		if label == "" {
			return Fail(422, "invalid_notification", "不支持的审批通知")
		}
		if event == "pending" && (Str(ap, "status") != "pending" || Num(p, "approval_version") > 0 && Num(p, "approval_version") != Num(ap, "version")) {
			return nil
		}
		policy, err := s.approvalNotificationPolicy(ctx, tx, ap)
		if err != nil {
			return err
		}
		if !notificationEnabled(policy) {
			return nil
		}
		action := Object(ap, "action")
		cid, module := Str(action, "case_id"), "*"
		if cid != "" {
			c, e := one(ctx, tx, "SELECT to_jsonb(c) FROM cases c WHERE id=$1", cid)
			if e != nil {
				return e
			}
			module = Str(c, "module")
		}
		channelID := Str(policy, "notification_channel_id")
		if channelID == "" {
			routes, e := many(ctx, tx, `SELECT to_jsonb(c) FROM configs c WHERE kind='owner-routes' AND data->>'enabled'='true' AND data->>'module' IN ($1,'*') ORDER BY (data->>'module'=$1) DESC,id`, module)
			if e != nil {
				return e
			}
			if len(routes) == 0 {
				return Fail(422, "notification_unrouted", "请在审批策略中选择通知渠道，或配置对应的值班通知渠道")
			}
			channelID = Str(configPublic(routes[0]), "channel_id")
		}
		ch, _, err := s.config(ctx, tx, channelID, "channels")
		if err != nil {
			return err
		}
		if !Bool(ch, "enabled") || Str(ch, "visibility") != "internal" {
			return Fail(422, "notification_unrouted", "审批通知需要启用的内部渠道")
		}
		co, _, err := s.config(ctx, tx, Str(ch, "connector_id"), "connectors")
		if err != nil {
			return err
		}
		if connectorDisabled(co) {
			return Fail(422, "notification_unrouted", "通知渠道的平台账号已停用")
		}
		body := label + "：" + boundedText(Str(ap, "summary"), 600)
		if reason := Str(p, "reason"); reason != "" {
			body += "\n原因：" + boundedText(reason, 1000)
		}
		if at := Str(p, "event_at"); at != "" {
			body += "\n时间：" + at
		}
		link := strings.TrimRight(s.Config.PublicURL, "/") + "/approvals/" + Str(ap, "id")
		body += "\n" + link
		actions := Data{}
		if event == "pending" {
			actions = Data{"approve": Data{"approval_id": Str(ap, "id"), "expected_version": Num(ap, "version")}, "reject": Data{"approval_id": Str(ap, "id"), "expected_version": Num(ap, "version")}}
		}
		request := Data{"case_id": cid, "approval_id": Str(ap, "id"), "approval_event": event, "approval_version": Num(p, "approval_version"), "notification_policy_id": Str(policy, "id"), "body": integration.Redact(body), "card_nonce": Token(), "card_actions": actions, "approval_url": link}
		target := Data{"channel_id": channelID, "channel_version": Num(ch, "version"), "connector_id": Str(co, "id"), "connector_version": Num(co, "version"), "external_id": Str(ch, "external_id")}
		key := fmt.Sprintf("approval-notification:%s:%s:%d", Str(ap, "id"), event, Num(p, "approval_version"))
		op, err := one(ctx, tx, `INSERT INTO external_operations(id,kind,subject_id,dedupe_key,target,request) VALUES($1,'notify',$2,$3,$4,$5) ON CONFLICT(dedupe_key) DO UPDATE SET dedupe_key=EXCLUDED.dedupe_key RETURNING to_jsonb(external_operations)`, ID(), Str(ap, "id"), key, JSON(target), JSON(request))
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, "dispatch_external", "dispatch:"+Str(op, "id"), Data{"operation_id": Str(op, "id")}, 0)
	})
}

// Recheck queued notices so switching notifications off also cancels unsent ones.
func (s *Service) approvalNotificationAllowed(ctx context.Context, q DB, op Data) (bool, error) {
	r := Object(op, "request")
	if Str(op, "kind") != "notify" || Str(r, "approval_id") == "" {
		return true, nil
	}
	ap, err := one(ctx, q, "SELECT to_jsonb(a) FROM approvals a WHERE id=$1", Str(r, "approval_id"))
	if err != nil {
		return false, err
	}
	if Str(r, "approval_event") == "pending" && (Str(ap, "status") != "pending" || Num(r, "approval_version") > 0 && Num(r, "approval_version") != Num(ap, "version")) {
		return false, nil
	}
	policy, err := s.approvalNotificationPolicy(ctx, q, ap)
	if err != nil {
		return false, err
	}
	if !notificationEnabled(policy) {
		return false, nil
	}
	if channel := Str(policy, "notification_channel_id"); channel != "" && channel != Str(Object(op, "target"), "channel_id") {
		return false, nil
	}
	return true, nil
}
