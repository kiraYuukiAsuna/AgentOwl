package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

// Serialize scheduling across workers; queued/running jobs prevent overlap.
func (s *Service) scheduleGitHubPolls(ctx context.Context) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		var acquired bool
		if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(728690106)").Scan(&acquired); err != nil || !acquired {
			return err
		}
		rows, err := many(ctx, tx, `SELECT to_jsonb(c) FROM configs c JOIN configs co ON co.id::text=c.data->>'connector_id' AND co.kind='connectors' WHERE c.kind='channels' AND c.data->>'enabled'='true' AND co.data->>'provider'='github' AND COALESCE(c.data->>'poll_interval_minutes','0') <> '0' ORDER BY COALESCE(c.data->>'poll_last_attempt_at',''),c.id LIMIT 1000 FOR UPDATE OF c`)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, raw := range rows {
			c := configPublic(raw)
			interval, err := channelPollInterval(c)
			if err != nil || interval == 0 {
				continue
			}
			last, _ := time.Parse(time.RFC3339Nano, Str(c, "poll_last_attempt_at"))
			if !last.IsZero() && now.Before(last.Add(time.Duration(interval)*time.Minute)) {
				continue
			}
			var active bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE kind='poll_github' AND status IN ('pending','running') AND payload->>'channel_id'=$1)`, Str(c, "id")).Scan(&active); err != nil {
				return err
			}
			if active {
				continue
			}
			if err = enqueue(ctx, tx, "poll_github", "github-poll:"+Str(c, "id")+":"+ID(), Data{"channel_id": Str(c, "id"), "version": Num(c, "version")}, 0); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE configs SET data=data||$2::jsonb WHERE id=$1`, Str(c, "id"), JSON(Data{"poll_last_attempt_at": now.Format(time.RFC3339Nano)})); err != nil {
				return err
			}
		}
		return nil
	})
}

type githubPollItem struct {
	issue    integration.Event
	comments []integration.Event
}

func (s *Service) pollGitHubChannel(ctx context.Context, p Data) error {
	ch, _, err := s.config(ctx, s.DB, Str(p, "channel_id"), "channels")
	if err != nil {
		return err
	}
	if !Bool(ch, "enabled") || Num(ch, "poll_interval_minutes") <= 0 || Num(ch, "version") != Num(p, "version") {
		return nil
	}
	co, secret, err := s.config(ctx, s.DB, Str(ch, "connector_id"), "connectors")
	if err != nil {
		return err
	}
	if connectorDisabled(co) || Str(co, "provider") != "github" {
		return Fail(409, "poll_connector_disabled", "GitHub 平台账号已停用或不可用")
	}
	cursor, err := time.Parse(time.RFC3339Nano, Str(ch, "poll_cursor"))
	if err != nil {
		return Fail(409, "poll_cursor_missing", "轮询起点无效，请关闭后重新开启定时轮询")
	}
	started, err := time.Parse(time.RFC3339Nano, Str(ch, "poll_started_at"))
	if err != nil {
		return Fail(409, "poll_cursor_missing", "轮询起点无效，请关闭后重新开启定时轮询")
	}
	end := time.Now().UTC()
	since := cursor.Add(-5 * time.Minute)
	if since.Before(started) {
		since = started.Add(-time.Second)
	}
	adapter, _, err := s.sourceAdapter(ctx, s.DB, co)
	if err != nil {
		return err
	}
	owner, repo := Str(co, "owner"), Str(co, "repo")
	repoVersion := int64(0)
	if rid := Str(co, "repository_id"); rid != "" {
		r, repoSecrets, e := s.config(ctx, s.DB, rid, "repositories")
		if e != nil {
			return e
		}
		owner, repo, repoVersion = Str(r, "owner"), Str(r, "repo"), Num(r, "version")
		for k, v := range repoSecrets {
			secret["repository_"+k] = v
		}
	}
	repository := owner + "/" + repo
	if !strings.EqualFold(Str(ch, "poll_repository"), repository) {
		return Fail(409, "poll_repository_changed", "GitHub 仓库已更改，请关闭后重新开启定时轮询")
	}
	scope := Str(ch, "external_id")
	number := 0
	if strings.HasPrefix(scope, "issue:") {
		number, err = strconv.Atoi(strings.TrimPrefix(scope, "issue:"))
		if err != nil || number < 1 {
			return Fail(422, "invalid_channel", "Issue 渠道 ID 无效")
		}
	} else if !strings.EqualFold(scope, repository) {
		return Fail(422, "invalid_channel", "轮询渠道的仓库范围与平台账号不一致")
	}
	issues := []integration.Event{}
	cursorPage := ""
	for page := 1; ; page++ {
		if page > 200 {
			return Fail(422, "poll_page_limit", "轮询超过分页上限，请缩小渠道范围")
		}
		result, e := adapter.ReadThread(ctx, integration.SourceRead{ChannelID: scope, Repository: repository, Cursor: cursorPage, Since: since})
		if e != nil {
			return e
		}
		issues = append(issues, result.Events...)
		if result.NextCursor == "" {
			break
		}
		cursorPage = result.NextCursor
	}
	items := make([]githubPollItem, 0, len(issues))
	for _, issue := range issues {
		item := githubPollItem{issue: issue}
		cursorPage = ""
		for page := 1; ; page++ {
			if page > 200 {
				return Fail(422, "poll_page_limit", "Issue 评论轮询超过分页上限")
			}
			result, e := adapter.ReadThread(ctx, integration.SourceRead{ThreadID: issue.ChannelID, Repository: repository, Cursor: cursorPage, Since: since})
			if e != nil {
				return e
			}
			for _, comment := range result.Events {
				when := sourceClock(comment.SourceRevision)
				if !when.Before(started) && !when.After(end) {
					item.comments = append(item.comments, comment)
				}
			}
			if result.NextCursor == "" {
				break
			}
			cursorPage = result.NextCursor
		}
		items = append(items, item)
	}
	knownSecrets := []string{}
	for _, value := range secret {
		if v, ok := value.(string); ok && v != "" {
			knownSecrets = append(knownSecrets, v)
		}
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := lockExecutionSettings(ctx, tx); err != nil {
			return err
		}
		raw, e := locked(ctx, tx, "configs", Str(ch, "id"))
		if e != nil {
			return e
		}
		current := configPublic(raw)
		if Num(current, "version") != Num(ch, "version") || !Bool(current, "enabled") || Num(current, "poll_interval_minutes") <= 0 || Str(current, "poll_cursor") != Str(ch, "poll_cursor") {
			return nil
		}
		currentCoRaw, e := locked(ctx, tx, "configs", Str(co, "id"))
		if e != nil {
			return e
		}
		currentCo := configPublic(currentCoRaw)
		if Num(currentCo, "version") != Num(co, "version") || connectorDisabled(currentCo) {
			return Conflict
		}
		if rid := Str(co, "repository_id"); rid != "" {
			rawRepo, e := locked(ctx, tx, "configs", rid)
			if e != nil {
				return e
			}
			r := configPublic(rawRepo)
			if Num(r, "version") != repoVersion || !Bool(r, "enabled") {
				return Conflict
			}
		}
		for _, item := range items {
			issue := item.issue
			issueID := issue.MessageID
			text := integration.Redact(issue.Text, knownSecrets...)
			// A comment also updates the issue timestamp, so do not treat an unchanged old issue as new.
			var existing string
			e = tx.QueryRow(ctx, `SELECT content FROM source_messages WHERE connector_id=$1 AND channel_id=$2 AND external_id=$3 AND NOT deleted`, Str(co, "id"), Str(ch, "id"), issueID).Scan(&existing)
			if e != nil && !errors.Is(e, pgx.ErrNoRows) {
				return e
			}
			changed := e == nil && existing != boundedText(text, 64000) || errors.Is(e, pgx.ErrNoRows) && !issue.OccurredAt.Before(started) && !issue.OccurredAt.After(end)
			if changed && !sourceClock(issue.SourceRevision).Before(started) && !sourceClock(issue.SourceRevision).After(end) {
				ev := issue
				ev.ID = "poll:" + strings.TrimPrefix(ev.ID, "history:")
				ev.Action = "updated"
				ev.Text = text

				target, routeErr := s.eventChannel(ctx, tx, Str(co, "id"), ev)
				if routeErr != nil && !errors.Is(routeErr, NotFound) {
					return routeErr
				}
				if routeErr != nil || Str(target, "id") != Str(current, "id") {
					continue
				}
				if _, _, e = s.persistEvent(ctx, tx, Str(co, "id"), current, ev); e != nil {
					return e
				}
			}
			for _, comment := range item.comments {
				ev := comment
				ev.ID = "poll:" + strings.TrimPrefix(ev.ID, "history:")
				ev.Action = "created"
				ev.Text = integration.Redact(ev.Text, knownSecrets...)
				ev.Bot = ev.Bot || Str(co, "sender_id") == ev.ActorID
				if sourceClock(ev.SourceRevision).After(ev.OccurredAt) {
					ev.Action = "edited"
				}

				target, routeErr := s.eventChannel(ctx, tx, Str(co, "id"), ev)
				if routeErr != nil && !errors.Is(routeErr, NotFound) {
					return routeErr
				}
				if routeErr != nil || Str(target, "id") != Str(current, "id") {
					continue
				}
				if strings.HasPrefix(strings.TrimSpace(ev.Text), "/agentowl feedback") {
					feedbackTx, beginErr := tx.Begin(ctx)
					if beginErr != nil {
						return beginErr
					}
					_, handled, feedbackErr := s.ingestGitHubFeedback(ctx, feedbackTx, Str(co, "id"), current, ev)
					if feedbackErr != nil {
						if rollbackErr := feedbackTx.Rollback(ctx); rollbackErr != nil {
							return rollbackErr
						}
						var se *Error
						if !errors.As(feedbackErr, &se) || se.Status >= 500 {
							return feedbackErr
						}
					} else if commitErr := feedbackTx.Commit(ctx); commitErr != nil {
						return commitErr
					}
					if handled {
						continue
					}
				}
				if _, _, e = s.persistEvent(ctx, tx, Str(co, "id"), current, ev); e != nil {
					return e
				}
			}
		}
		_, e = tx.Exec(ctx, `UPDATE configs SET data=data||$2::jsonb,updated_at=now() WHERE id=$1`, Str(ch, "id"), JSON(Data{"poll_cursor": end.Format(time.RFC3339Nano), "last_polled_at": end.Format(time.RFC3339Nano), "poll_last_error": ""}))
		return e
	})
}
