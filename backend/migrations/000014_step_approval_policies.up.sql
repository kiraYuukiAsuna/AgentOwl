-- Keep a former manual override from silently enabling automatic policies.
UPDATE configs SET data=data||jsonb_build_object('mode','manual','approval_version',version+1),
 version=version+1,updated_at=now()
WHERE kind='policies' AND data->>'kind' IN ('plan','edit','test','review','publish_pr')
 AND data->>'mode'='auto'
 AND EXISTS(SELECT 1 FROM system_settings WHERE name='execution' AND data->>'approval_mode'='manual');

-- Unstarted legacy approvals must be prepared under the unified policies.
WITH invalidated AS (
 UPDATE approvals a SET status='stale',reason='审批已统一至策略，请恢复任务并重新审批',version=a.version+1,updated_at=now()
 FROM task_steps st WHERE a.subject_type='step' AND a.subject_id=st.id
 AND st.attempt_id='' AND a.action->>'approval_mode' IN ('manual','auto')
 AND a.status IN ('pending','approved')
 AND NOT EXISTS(SELECT 1 FROM external_operations o WHERE o.approval_id=a.id::text
  AND o.kind='publish_pr' AND o.status NOT IN ('pending','cancelled'))
 RETURNING st.task_id,a.id
), cancelled AS (
 UPDATE external_operations SET status='cancelled',error='审批策略已更新，请重新审批',version=version+1,updated_at=now()
 WHERE kind='publish_pr' AND status='pending' AND approval_id IN(SELECT id::text FROM invalidated)
), blocked AS (
 UPDATE dev_tasks SET status='blocked',error='审批已统一至策略，请恢复任务并重新审批',version=version+1,updated_at=now()
 WHERE id IN(SELECT task_id FROM invalidated) AND status NOT IN ('paused','cancelled','completed')
 RETURNING id,revision,epoch
)
INSERT INTO task_events(id,task_id,type,actor,metadata)
SELECT gen_random_uuid(),id,'block','system:migration',jsonb_build_object('revision',revision,'epoch',epoch,'status','blocked','reason','unified_approval_policies') FROM blocked;

UPDATE system_settings SET data=data-'approval_mode' WHERE name='execution' AND data ? 'approval_mode';
