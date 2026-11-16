package service

import (
	"agentowl/backend/internal/integration"
	"context"
	"strconv"
	"strings"
)

func (s *Service) sourceAdapter(ctx context.Context, q DB, co Data) (integration.Source, Data, error) {
	switch Str(co, "provider") {
	case "github":
		c, d, e := s.supportGitHub(ctx, q, Str(co, "id"), "connectors")
		return integration.GitHubSource{Client: c}, d, e
	case "feishu", "lark":
		c, d, e := s.supportLark(ctx, q, Str(co, "id"))
		return integration.LarkSource{Client: c}, d, e
	default:
		return nil, nil, integration.ErrUnsupportedOperation
	}
}
func sourceWrite(op, ch, co, c Data) integration.SourceWrite {
	request := Object(op, "request")
	env := Object(c, "environment")
	write := integration.SourceWrite{OperationID: Str(op, "id"), ChannelID: Str(ch, "external_id"), Body: integration.Redact(Str(request, "body")), Issue: int(Num(env, "source_number")), SenderID: Str(co, "sender_id"), SenderLogin: Str(co, "sender_login")}
	if write.Issue <= 0 || Str(op, "kind") == "notify" {
		external := Str(ch, "external_id")
		if strings.HasPrefix(external, "issue:") || strings.HasPrefix(external, "pull:") {
			write.Issue, _ = strconv.Atoi(strings.SplitN(external, ":", 2)[1])
		}
	}
	if Str(op, "kind") == "reply_send" {
		write.ReplyTo = Str(env, "last_message_id")
		write.ReplyInThread = Str(env, "platform_thread_id") != ""
		if Str(co, "provider") == "github" {
			write.Body = replyFeedbackBody(op, write.Body)
		} else if Str(request, "card_nonce") != "" && len(Object(request, "feedback_context")) > 0 {
			write.Card = replyFeedbackCard(op, write.Body)
		}
	} else if Str(op, "kind") == "read_mark" {
		write.MessageID = Str(request, "message_id")
		write.Emoji = fallback(Str(request, "emoji_type"), "Typing")
	} else if Str(request, "card_nonce") != "" {
		write.Card = notificationCard(Str(op, "id"), request)
	}
	return write
}
