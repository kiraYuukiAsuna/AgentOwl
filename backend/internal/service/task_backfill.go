package service

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Backfill only recorded facts; historical runs never acquire synthetic receipts.
func (s *Service) backfillTaskRuntime(ctx context.Context) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := lockExecutionSettings(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO source_threads(id,connector_id,channel_key,external_thread_id)
 SELECT gen_random_uuid(),connector_id,channel_id,thread_id FROM (
 SELECT DISTINCT m.connector_id,m.channel_id,CASE WHEN c.environment->>'p2p_chat'='true' THEN 'p2p:'||m.actor_id ELSE COALESCE(NULLIF(m.thread_id,''),m.external_id) END AS thread_id
 FROM source_messages m JOIN cases c ON c.id=m.case_id WHERE NOT EXISTS(SELECT 1 FROM source_threads th WHERE th.connector_id=m.connector_id AND th.channel_key=m.channel_id AND th.external_thread_id=CASE WHEN c.environment->>'p2p_chat'='true' THEN 'p2p:'||m.actor_id ELSE COALESCE(NULLIF(m.thread_id,''),m.external_id) END)
 ORDER BY m.connector_id,m.channel_id,thread_id LIMIT 100) source ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		rows, err := many(ctx, tx, `SELECT to_jsonb(t) FROM dev_tasks t WHERE mandate_id IS NULL ORDER BY id LIMIT 100 FOR UPDATE`)
		if err != nil {
			return err
		}
		for _, t := range rows {
			id := Str(t, "id")
			scope := Data{"repository_id": Str(t, "repository_id"), "case_id": Str(t, "case_id"), "goal": Str(t, "goal"), "acceptance_criteria": Str(t, "acceptance_criteria"), "followup_mode": "manual", "historical_scope_available": false}
			mid := ID()
			if _, err = tx.Exec(ctx, `INSERT INTO task_mandates(id,task_id,revision,issuer,sponsor_principal_id,scope,scope_digest,reason) VALUES($1,$2,$3,'legacy_confirmation',(SELECT id FROM principals WHERE id::text=$4),$5,$6,'历史记录缺少冻结的来源和路径范围，请重新确认')`, mid, id, Num(t, "revision"), Str(t, "confirmed_by"), JSON(scope), Hash(JSON(scope))); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE dev_tasks SET mandate_id=$2,hold_reason=CASE WHEN status IN ('completed','cancelled') THEN '' ELSE 'authorization_changed' END WHERE id=$1`, id, mid); err != nil {
				return err
			}
			messages, e := many(ctx, tx, `SELECT to_jsonb(source) FROM (SELECT DISTINCT m.connector_id,m.channel_id,CASE WHEN c.environment->>'p2p_chat'='true' THEN 'p2p:'||m.actor_id ELSE COALESCE(NULLIF(m.thread_id,''),m.external_id) END AS thread_id FROM source_messages m JOIN cases c ON c.id=m.case_id WHERE m.case_id=$1 ORDER BY m.connector_id,m.channel_id,thread_id) source`, Str(t, "case_id"))
			if e != nil {
				return e
			}
			for _, m := range messages {
				th, e := one(ctx, tx, `INSERT INTO source_threads(id,connector_id,channel_key,external_thread_id) VALUES($1,$2,$3,$4) ON CONFLICT(connector_id,channel_key,external_thread_id) DO UPDATE SET channel_key=EXCLUDED.channel_key RETURNING to_jsonb(source_threads)`, ID(), Str(m, "connector_id"), Str(m, "channel_id"), Str(m, "thread_id"))
				if e != nil {
					return e
				}
				active := !contains([]string{"completed", "cancelled"}, Str(t, "status"))
				tag, e := tx.Exec(ctx, `INSERT INTO task_threads(task_id,thread_id,repository_id,relation,routing_active,routed_generation,resolved_generation,attached_by) VALUES($1,$2,$3,'owner',$4,$5,$5,'migration') ON CONFLICT DO NOTHING`, id, Str(th, "id"), Str(t, "repository_id"), active, Num(th, "processed_generation"))
				if e != nil {
					return e
				}
				if tag.RowsAffected() == 0 {
					if e = taskEvent(ctx, tx, id, "legacy_routing_conflict", "migration", Data{"thread_id": Str(th, "id")}); e != nil {
						return e
					}
				}
			}
			if err = taskEvent(ctx, tx, id, "legacy_authorization_recorded", "migration", Data{"mandate_id": mid}); err != nil {
				return err
			}
		}
		return nil
	})
}
