UPDATE configs
SET data = data - 'tenant_id',
    name = COALESCE(data->>'provider','') || ':' || COALESCE(data->>'app_id','') || ':' || COALESCE(data->>'subject_id',''),
    version = version + 1,
    updated_at = now()
WHERE kind = 'identity-bindings'
  AND (data ? 'tenant_id'
    OR name IS DISTINCT FROM COALESCE(data->>'provider','') || ':' || COALESCE(data->>'app_id','') || ':' || COALESCE(data->>'subject_id',''));
