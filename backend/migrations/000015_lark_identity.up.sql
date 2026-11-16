CREATE TABLE lark_login_transactions (
 state_hash text PRIMARY KEY, verifier text NOT NULL,
 connector_id uuid NOT NULL REFERENCES configs(id), connector_version bigint NOT NULL,
 principal_id text NOT NULL DEFAULT '', session_hash text NOT NULL DEFAULT '',
 expires_at timestamptz NOT NULL
);
ALTER TABLE sessions ADD COLUMN identity_binding_id text NOT NULL DEFAULT '';
CREATE INDEX sessions_identity_binding ON sessions(identity_binding_id) WHERE identity_binding_id<>'';
