-- Notification edits must not revoke execution authority.
UPDATE configs SET data=data || jsonb_build_object('approval_version',version,'notification_enabled',true,'notification_channel_id','') WHERE kind='policies';
