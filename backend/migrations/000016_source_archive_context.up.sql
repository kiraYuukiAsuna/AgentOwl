ALTER TABLE source_messages ADD COLUMN root_id text NOT NULL DEFAULT '';
ALTER TABLE source_messages ADD COLUMN parent_id text NOT NULL DEFAULT '';
ALTER TABLE source_messages ADD COLUMN platform_thread_id text NOT NULL DEFAULT '';
ALTER TABLE source_messages ADD COLUMN actor_type text NOT NULL DEFAULT '';
ALTER TABLE source_messages ADD COLUMN sender_name text NOT NULL DEFAULT '';
ALTER TABLE source_messages ADD COLUMN message_type text NOT NULL DEFAULT '';
ALTER TABLE source_messages ADD COLUMN mentioned_subjects text[] NOT NULL DEFAULT '{}';
ALTER TABLE source_messages ADD COLUMN bot boolean NOT NULL DEFAULT false;
ALTER TABLE source_messages ADD COLUMN occurred_at timestamptz;
ALTER TABLE source_messages ADD COLUMN raw_digest text NOT NULL DEFAULT '';
ALTER TABLE source_messages ADD COLUMN content_digest text NOT NULL DEFAULT '';

UPDATE source_messages SET occurred_at=created_at WHERE occurred_at IS NULL;

CREATE INDEX source_messages_channel_time ON source_messages(connector_id,channel_id,occurred_at,external_id);
CREATE INDEX source_messages_roots ON source_messages(connector_id,channel_id,root_id,parent_id,platform_thread_id);
CREATE INDEX source_messages_actor_time ON source_messages(connector_id,channel_id,actor_id,occurred_at);

ALTER TABLE source_message_versions ADD COLUMN root_id text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN parent_id text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN thread_id text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN platform_thread_id text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN actor_id text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN actor_type text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN sender_name text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN message_type text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN mentioned_subjects text[] NOT NULL DEFAULT '{}';
ALTER TABLE source_message_versions ADD COLUMN bot boolean NOT NULL DEFAULT false;
ALTER TABLE source_message_versions ADD COLUMN occurred_at timestamptz;
ALTER TABLE source_message_versions ADD COLUMN source_url text NOT NULL DEFAULT '';
ALTER TABLE source_message_versions ADD COLUMN content_digest text NOT NULL DEFAULT '';

CREATE TABLE source_actors (
 id uuid PRIMARY KEY,
 connector_id uuid NOT NULL REFERENCES configs(id),
 subject_id text NOT NULL,
 name text NOT NULL DEFAULT '',
 kind text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(connector_id,subject_id)
);

CREATE TABLE source_message_files (
 message_id uuid NOT NULL REFERENCES source_messages(id),
 file_key text NOT NULL,
 connector_id uuid NOT NULL REFERENCES configs(id),
 channel_id text NOT NULL,
 kind text NOT NULL DEFAULT '',
 name text NOT NULL DEFAULT '',
 artifact_id uuid REFERENCES artifacts(id),
 status text NOT NULL DEFAULT 'referenced',
 error text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(message_id,file_key)
);
CREATE INDEX source_message_files_status ON source_message_files(status,updated_at);

CREATE TABLE source_sync_status (
 connector_id uuid NOT NULL REFERENCES configs(id),
 channel_id text NOT NULL,
 thread_key text NOT NULL DEFAULT '',
 backfilled boolean NOT NULL DEFAULT false,
 synced_until timestamptz,
 last_attempt_at timestamptz,
 last_error text NOT NULL DEFAULT '',
 coverage jsonb NOT NULL DEFAULT '{}',
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(connector_id,channel_id,thread_key)
);
