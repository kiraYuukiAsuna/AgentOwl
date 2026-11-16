package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type SourceRead struct {
	ChannelID, ThreadID, Cursor, Repository string
	Since, Until                            time.Time
}
type SourcePage struct {
	Events     []Event
	Threads    []string
	NextCursor string
}
type SourceReader interface {
	ReadThread(context.Context, SourceRead) (SourcePage, error)
}
type AttachmentReader interface {
	ReadAttachment(context.Context, string, Attachment) ([]byte, error)
}
type SourceCapabilities struct{ Threads, Attachments, Reactions, WriteIdempotency bool }

func (s LarkSource) Describe() SourceCapabilities   { return SourceCapabilities{true, true, true, true} }
func (s GitHubSource) Describe() SourceCapabilities { return SourceCapabilities{Threads: true} }
func (s LarkSource) ReadAttachment(ctx context.Context, message string, ref Attachment) ([]byte, error) {
	return s.Client.FetchResource(ctx, message, ref.Key, ref.Type)
}
func (s LarkSource) ReadThread(ctx context.Context, r SourceRead) (SourcePage, error) {
	kind, id := "chat", r.ChannelID
	if r.ThreadID != "" {
		kind, id = "thread", r.ThreadID
		r.Since, r.Until = time.Time{}, time.Time{}
	}
	p, err := s.Client.History(ctx, HistoryRequest{ContainerType: kind, ContainerID: id, PageToken: r.Cursor, StartTime: r.Since, EndTime: r.Until, PageSize: 50})
	if err != nil {
		return SourcePage{}, err
	}
	out := SourcePage{}
	for _, m := range p.Items {
		ev, err := m.Event()
		if err != nil {
			return out, err
		}
		if ev.ChannelID == "" {
			ev.ChannelID = r.ChannelID
		}
		if ev.ChannelID != r.ChannelID {
			return out, ErrInvalidInput
		}
		out.Events = append(out.Events, ev)
		if kind == "chat" && ev.ThreadID != "" {
			out.Threads = append(out.Threads, ev.ThreadID)
		}
	}
	if p.HasMore {
		if p.PageToken == "" || p.PageToken == r.Cursor {
			return out, ErrInvalidInput
		}
		out.NextCursor = p.PageToken
	}
	return out, nil
}
func (s GitHubSource) ReadThread(ctx context.Context, r SourceRead) (SourcePage, error) {
	page := 1
	if r.Cursor != "" {
		n, e := strconv.Atoi(r.Cursor)
		if e != nil || n < 1 {
			return SourcePage{}, ErrInvalidInput
		}
		page = n
	}
	out := SourcePage{}
	if r.ThreadID != "" {
		n, e := strconv.Atoi(strings.TrimPrefix(r.ThreadID, "issue:"))
		if e != nil || n < 1 {
			return out, ErrInvalidInput
		}
		comments, e := s.Client.ListCommentsSince(ctx, n, page, r.Since)
		if e != nil {
			return out, e
		}
		for _, c := range comments {
			if c.ID <= 0 || c.User.ID <= 0 || c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
				return out, ErrInvalidInput
			}
			ev := Event{ID: fmt.Sprintf("history:comment:%d:%s", c.ID, c.UpdatedAt.Format(time.RFC3339Nano)), Kind: "github.issue_comment", ChannelID: fmt.Sprintf("issue:%d", n), MessageID: fmt.Sprintf("comment:%d", c.ID), Number: n, Repository: r.Repository, ActorID: strconv.FormatInt(c.User.ID, 10), Text: Redact(c.Body), SourceURL: c.HTMLURL, SourceRevision: c.UpdatedAt.Format(time.RFC3339Nano), OccurredAt: c.CreatedAt, Bot: c.User.Type == "Bot"}
			out.Events = append(out.Events, ev)
		}
		if len(comments) == 100 {
			out.NextCursor = strconv.Itoa(page + 1)
		}
		return out, nil
	}
	var issues []GitHubIssue
	if strings.HasPrefix(r.ChannelID, "issue:") || strings.HasPrefix(r.ChannelID, "pull:") {
		n, e := strconv.Atoi(strings.SplitN(r.ChannelID, ":", 2)[1])
		if e != nil || n < 1 {
			return out, ErrInvalidInput
		}
		i, e := s.Client.GetIssue(ctx, n)
		if e != nil {
			return out, e
		}
		issues = []GitHubIssue{i}
	} else {
		var e error
		issues, e = s.Client.ListIssues(ctx, IssueListRequest{State: "all", Since: r.Since, Page: page, PerPage: 100})
		if e != nil {
			return out, e
		}
		if len(issues) == 100 {
			out.NextCursor = strconv.Itoa(page + 1)
		}
	}
	for _, i := range issues {
		if i.PullRequest != nil {
			continue
		}
		if i.ID <= 0 || i.Number <= 0 || i.User.ID <= 0 || i.CreatedAt.IsZero() || i.UpdatedAt.IsZero() {
			return out, ErrInvalidInput
		}
		ev := Event{ID: fmt.Sprintf("history:issue:%d:%s", i.ID, i.UpdatedAt.Format(time.RFC3339Nano)), Kind: "github.issues", ChannelID: fmt.Sprintf("issue:%d", i.Number), MessageID: fmt.Sprintf("issue:%d", i.ID), Number: i.Number, Repository: r.Repository, ActorID: strconv.FormatInt(i.User.ID, 10), Text: Redact(i.Title + "\n\n" + i.Body), SourceURL: i.HTMLURL, SourceRevision: i.UpdatedAt.Format(time.RFC3339Nano), OccurredAt: i.CreatedAt, Bot: i.User.Type == "Bot"}
		out.Events = append(out.Events, ev)
		out.Threads = append(out.Threads, ev.ChannelID)
	}
	return out, nil
}
