-- Keep IDs and versions so existing task mandates retain their original authority.
UPDATE configs SET kind='policies', data=data || jsonb_build_object(
  'kind','create_task',
  'mode',CASE WHEN (SELECT data->>'mode' FROM system_settings WHERE name='admission')='auto' THEN 'auto' ELSE 'manual' END,
  'enabled',COALESCE((data->>'enabled')::boolean,false) AND COALESCE((SELECT data->>'mode'<>'off' FROM system_settings WHERE name='admission'),false)
) WHERE kind='admission-rules';

-- Historical decisions remain inspectable; migration never grants new approval.
INSERT INTO approvals(id,kind,subject_id,subject_type,status,summary,action,action_hash,reason,expires_at,created_at,updated_at)
SELECT d.id,'create_task',d.id,'admission',CASE WHEN d.task_id IS NOT NULL THEN 'executed' ELSE 'stale' END,
  '创建开发任务 · '||c.title,
  jsonb_build_object('case_id',d.case_id,'repository_id',d.repository_id,'task_id',d.task_id,'goal',d.evidence->>'goal','acceptance_criteria',d.evidence->>'acceptance_criteria','historical',true),
  '',COALESCE(NULLIF(d.reason_code,''),'历史任务创建记录'),d.created_at+interval '24 hours',d.created_at,d.created_at
FROM admission_decisions d JOIN cases c ON c.id=d.case_id
ON CONFLICT(id) DO NOTHING;
