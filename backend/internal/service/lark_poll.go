package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// The cursor belongs to a specific platform account and group, not merely a channel ID.
func larkPollSource(connector, channel Data) string {
	return Hash(JSON(Data{"provider": Str(connector, "provider"), "app_id": Str(connector, "app_id"), "base_url": Str(connector, "base_url"), "external_id": Str(channel, "external_id")}))
}

// Each run uses the existing history-sync jobs. Advance the cursor only after
// every page and thread in the batch has finished successfully.
func (s *Service) scheduleLarkPolls(ctx context.Context) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		var acquired bool
		if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(728690107)").Scan(&acquired); err != nil || !acquired {
			return err
		}
		rows, err := many(ctx, tx, `SELECT to_jsonb(c) FROM configs c JOIN configs co ON co.id::text=c.data->>'connector_id' AND co.kind='connectors' WHERE c.kind='channels' AND c.data->>'enabled'='true' AND co.data->>'provider' IN ('feishu','lark') AND COALESCE(c.data->>'poll_interval_minutes','0') <> '0' ORDER BY COALESCE(c.data->>'poll_last_attempt_at',''),c.id LIMIT 1000 FOR UPDATE OF c`)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, raw := range rows {
			ch := configPublic(raw)
			interval, err := channelPollInterval(ch)
			if err != nil || interval == 0 {
				continue
			}
			// Keep the connector snapshot stable while finalizing or scheduling a batch.
			if _, err := tx.Exec(ctx, `SELECT id FROM configs WHERE id=$1 FOR SHARE`, Str(ch, "connector_id")); err != nil {
				return err
			}
			co, _, err := s.config(ctx, tx, Str(ch, "connector_id"), "connectors")
			if err != nil {
				return err
			}
			batch := Str(ch, "poll_batch_id")
			if batch != "" {
				var active, failed, total int
				var reason string
				err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status IN ('pending','running')), count(*) FILTER (WHERE status='failed'), count(*), COALESCE(max(NULLIF(last_error,'')) FILTER (WHERE status='failed'),'') FROM jobs WHERE kind='sync_channel' AND payload->>'sync_id'=$1`, batch).Scan(&active, &failed, &total, &reason)
				if err != nil {
					return err
				}
				if active > 0 {
					continue
				}
				patch := Data{}
				if total == 0 || failed > 0 {
					if reason == "" {
						reason = "定时同步未完成，请检查运行记录"
					}
					patch["poll_last_error"] = reason
				} else if Num(ch, "version") != Num(ch, "poll_batch_version") || Num(co, "version") != Num(ch, "poll_connector_version") || connectorDisabled(co) || larkPollSource(co, ch) != Str(ch, "poll_source") {
					patch["poll_last_error"] = "渠道或平台账号配置已更改，将重新同步"
				} else {
					patch["poll_cursor"] = Str(ch, "poll_batch_end")
					patch["last_polled_at"] = now.Format(time.RFC3339Nano)
					patch["poll_last_error"] = ""
				}
				if _, err = tx.Exec(ctx, `UPDATE configs SET data=(data-'poll_batch_id'-'poll_batch_end'-'poll_batch_version'-'poll_connector_version')||$2::jsonb WHERE id=$1`, Str(ch, "id"), JSON(patch)); err != nil {
					return err
				}
				// Wait until the next scan; this also keeps the retry interval in force.
				continue
			}
			if connectorDisabled(co) || larkPollSource(co, ch) != Str(ch, "poll_source") {
				if _, err = tx.Exec(ctx, `UPDATE configs SET data=data||$2::jsonb WHERE id=$1`, Str(ch, "id"), JSON(Data{"poll_last_error": "平台账号已停用或发生变更，请检查后重新开启定时同步"})); err != nil {
					return err
				}
				continue
			}
			last, _ := time.Parse(time.RFC3339Nano, Str(ch, "poll_last_attempt_at"))
			if !last.IsZero() && now.Before(last.Add(time.Duration(interval)*time.Minute)) {
				continue
			}
			var active bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE kind='sync_channel' AND status IN ('pending','running') AND payload->>'channel_id'=$1)`, Str(ch, "id")).Scan(&active); err != nil {
				return err
			}
			if active {
				continue
			}
			p := Data{"channel_id": Str(ch, "id"), "version": Num(ch, "version"), "sync_id": ID(), "end_time": now.Format(time.RFC3339Nano), "periodic": true}
			if cursor := Str(ch, "poll_cursor"); cursor != "" {
				t, err := time.Parse(time.RFC3339Nano, cursor)
				if err != nil {
					if _, err = tx.Exec(ctx, `UPDATE configs SET data=data||$2::jsonb WHERE id=$1`, Str(ch, "id"), JSON(Data{"poll_last_error": "同步进度无效，请关闭后重新开启定时同步"})); err != nil {
						return err
					}
					continue
				}
				p["start_time"] = t.Add(-5 * time.Minute).Format(time.RFC3339Nano)
			}
			if err = enqueue(ctx, tx, "sync_channel", "lark-periodic:"+Str(ch, "id")+":"+Str(p, "sync_id"), p, 0); err != nil {
				return err
			}
			patch := Data{"poll_batch_id": Str(p, "sync_id"), "poll_batch_end": Str(p, "end_time"), "poll_batch_version": Num(ch, "version"), "poll_connector_version": Num(co, "version"), "poll_last_attempt_at": now.Format(time.RFC3339Nano)}
			if _, err = tx.Exec(ctx, `UPDATE configs SET data=data||$2::jsonb WHERE id=$1`, Str(ch, "id"), JSON(patch)); err != nil {
				return err
			}
		}
		return nil
	})
}
