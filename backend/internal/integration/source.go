package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

var ErrSourceReconcile = errors.New("source reconciliation is ambiguous or incomplete")

// Source owns platform protocol differences; callers own authorization and retries.
type Source interface {
	SourceReader
	Describe() SourceCapabilities
	Parse(http.Header, []byte) (SourceInbound, error)
	Send(context.Context, SourceWrite) (SourceResult, error)
	Reconcile(context.Context, SourceWrite, SourceResult) (SourceResult, bool, error)
}
type SourceInbound struct {
	Event     *Event
	Card      *CardAction
	Challenge string
	Ignored   bool
}
type SourceWrite struct {
	OperationID, ChannelID, ReplyTo, Body, MessageID, Emoji, SenderID, SenderLogin string
	Issue                                                                          int
	ReplyInThread                                                                  bool
	Card                                                                           json.RawMessage
}
type SourceResult struct {
	RemoteID string `json:"remote_id"`
	URL      string `json:"url,omitempty"`
	Issue    int    `json:"issue_number,omitempty"`
}
type LarkSource struct{ Client *LarkClient }
type GitHubSource struct{ Client *GitHubClient }

func (s LarkSource) Parse(h http.Header, b []byte) (SourceInbound, error) {
	in, e := s.Client.ParseWebhook(h, b)
	if e != nil {
		in, e = s.Client.ParseCardAction(h, b)
	}
	return SourceInbound{Event: in.Event, Card: in.CardAction, Challenge: in.Challenge, Ignored: in.Ignored}, e
}
func (s GitHubSource) Parse(h http.Header, b []byte) (SourceInbound, error) {
	in, e := s.Client.ParseWebhook(h, b)
	return SourceInbound{Event: in.Event, Ignored: in.Ignored}, e
}
func (s LarkSource) Send(ctx context.Context, w SourceWrite) (SourceResult, error) {
	if w.MessageID != "" {
		r, e := s.Client.AddReaction(ctx, AddReactionRequest{MessageID: w.MessageID, EmojiType: w.Emoji})
		return SourceResult{RemoteID: r.ReactionID}, e
	}
	body, _ := json.Marshal(map[string]string{"text": w.Body})
	kind := "text"
	if len(w.Card) > 0 {
		body = w.Card
		kind = "interactive"
	}
	r, e := s.Client.SendMessage(ctx, SendMessageRequest{ReceiveIDType: "chat_id", ReceiveID: w.ChannelID, ReplyTo: w.ReplyTo, ReplyInThread: w.ReplyInThread, MessageType: kind, Content: body, UUID: w.OperationID})
	return SourceResult{RemoteID: r.MessageID}, e
}
func (s GitHubSource) Send(ctx context.Context, w SourceWrite) (SourceResult, error) {
	if w.MessageID != "" {
		return SourceResult{}, ErrUnsupportedOperation
	}
	if w.Issue <= 0 {
		return SourceResult{}, ErrInvalidInput
	}
	marker, e := OperationMarker(w.OperationID)
	if e != nil {
		return SourceResult{}, e
	}
	r, e := s.Client.CreateComment(ctx, w.Issue, w.Body+"\n\n"+marker)
	return SourceResult{RemoteID: strconv.FormatInt(r.ID, 10), URL: r.HTMLURL, Issue: w.Issue}, e
}
func (s LarkSource) Reconcile(ctx context.Context, w SourceWrite, r SourceResult) (SourceResult, bool, error) {
	if r.RemoteID == "" {
		return r, false, nil
	}
	messages, e := s.Client.GetMessage(ctx, r.RemoteID)
	if e != nil {
		return r, false, e
	}
	for _, m := range messages {
		if m.MessageID == r.RemoteID && m.ChatID == w.ChannelID && !m.Deleted {
			return r, true, nil
		}
	}
	return r, false, nil
}
func (s GitHubSource) Reconcile(ctx context.Context, w SourceWrite, r SourceResult) (SourceResult, bool, error) {
	marker, e := OperationMarker(w.OperationID)
	if e != nil {
		return r, false, e
	}
	if w.Issue <= 0 {
		return r, false, ErrInvalidInput
	}
	var found *GitHubComment
	for page := 1; page <= 20; page++ {
		comments, e := s.Client.ListComments(ctx, w.Issue, page)
		if e != nil {
			return r, false, e
		}
		for _, c := range comments {
			if !strings.Contains(c.Body, marker) {
				continue
			}
			if r.RemoteID != "" {
				if strconv.FormatInt(c.ID, 10) != r.RemoteID {
					continue
				}
			} else {
				if w.SenderID == "" && w.SenderLogin == "" {
					return r, false, nil
				}
				if w.SenderID != "" && w.SenderID != strconv.FormatInt(c.User.ID, 10) {
					continue
				}
				if w.SenderLogin != "" && !strings.EqualFold(w.SenderLogin, c.User.Login) {
					continue
				}
			}
			if c.Body != w.Body+"\n\n"+marker || found != nil {
				return r, false, ErrSourceReconcile
			}
			copy := c
			found = &copy
		}
		if len(comments) < 100 {
			if found == nil {
				return r, false, nil
			}
			return SourceResult{RemoteID: strconv.FormatInt(found.ID, 10), URL: found.HTMLURL, Issue: w.Issue}, true, nil
		}
	}
	return r, false, ErrSourceReconcile
}
