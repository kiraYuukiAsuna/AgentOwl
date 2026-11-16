WITH p2p_cases AS (
  SELECT c.id AS case_id,
         c.channel_id AS old_channel_id,
         c.environment,
         ch.data->>'connector_id' AS connector_id,
         COALESCE(c.environment->>'external_channel_id', ch.data->>'external_id') AS external_id,
         COALESCE((
           SELECT sm.actor_id FROM source_messages sm
           WHERE sm.case_id=c.id AND sm.connector_id::text=ch.data->>'connector_id' AND sm.channel_id=c.channel_id AND sm.actor_id<>''
           ORDER BY sm.created_at DESC, sm.external_id DESC LIMIT 1
         ), '') AS actor_id
  FROM cases c
  JOIN configs ch ON ch.id::text=c.channel_id AND ch.kind='channels'
  WHERE c.environment->>'p2p_chat'='true'
), mapped AS (
  SELECT *, 'private:' || connector_id || ':' || CASE WHEN actor_id<>'' THEN actor_id ELSE external_id END AS private_channel_id
  FROM p2p_cases
), updated_messages AS (
  UPDATE source_messages sm
  SET channel_id=m.private_channel_id
  FROM mapped m
  WHERE sm.case_id=m.case_id AND sm.connector_id::text=m.connector_id AND sm.channel_id=m.old_channel_id
  RETURNING sm.id
), updated_cases AS (
  UPDATE cases c
  SET channel_id='',
      environment = COALESCE(c.environment,'{}'::jsonb)
        || jsonb_build_object('private_connector_id', m.connector_id, 'external_channel_id', m.external_id)
        || CASE WHEN m.actor_id<>'' THEN jsonb_build_object('private_actor_id', m.actor_id) ELSE '{}'::jsonb END,
      updated_at=now(), version=version+1
  FROM mapped m
  WHERE c.id=m.case_id
  RETURNING m.old_channel_id
), removable_channels AS (
  SELECT DISTINCT old_channel_id FROM updated_cases
  WHERE old_channel_id<>''
    AND NOT EXISTS (SELECT 1 FROM cases c WHERE c.channel_id=old_channel_id)
    AND NOT EXISTS (SELECT 1 FROM source_messages sm WHERE sm.channel_id=old_channel_id)
)
UPDATE configs ch
SET data=jsonb_set(ch.data,'{enabled}','false'::jsonb), updated_at=now(), version=version+1
WHERE ch.kind='channels' AND ch.id::text IN (SELECT old_channel_id FROM removable_channels);
