CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE TABLE principals (
 id uuid PRIMARY KEY, name text NOT NULL, email text NOT NULL UNIQUE, role text NOT NULL DEFAULT 'viewer',
 roles text[] NOT NULL DEFAULT '{}', channel_ids text[] NOT NULL DEFAULT '{}', repository_ids text[] NOT NULL DEFAULT '{}',
 password_hash text NOT NULL DEFAULT '', subject text UNIQUE, active boolean NOT NULL DEFAULT true,
 version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sessions (id_hash text PRIMARY KEY, principal_id uuid NOT NULL REFERENCES principals(id), csrf text NOT NULL, expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE login_transactions (state_hash text PRIMARY KEY, verifier text NOT NULL, nonce text NOT NULL, expires_at timestamptz NOT NULL);
CREATE TABLE configs (
 id uuid PRIMARY KEY, kind text NOT NULL, name text NOT NULL, data jsonb NOT NULL DEFAULT '{}', secrets text NOT NULL DEFAULT '',
 version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(kind,name)
);
CREATE TABLE config_versions (config_id uuid NOT NULL REFERENCES configs(id), version bigint NOT NULL, data jsonb NOT NULL, actor_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(config_id,version));
CREATE TABLE cases (
 id uuid PRIMARY KEY, canonical_case_id uuid NOT NULL REFERENCES cases(id) DEFERRABLE INITIALLY DEFERRED,
 title text NOT NULL, description text NOT NULL DEFAULT '', summary text NOT NULL DEFAULT '', status text NOT NULL DEFAULT 'triage',
 priority text NOT NULL DEFAULT 'P2', module text NOT NULL DEFAULT 'general', category text NOT NULL DEFAULT 'question', support_relevance text NOT NULL DEFAULT 'uncertain',
 assignee_id text NOT NULL DEFAULT '', channel_id text NOT NULL DEFAULT '', reply_mode text NOT NULL DEFAULT 'draft', source text NOT NULL DEFAULT 'manual',
 environment jsonb NOT NULL DEFAULT '{}', created_by text NOT NULL, version bigint NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cases_queue ON cases(status,priority,created_at DESC,id);
CREATE INDEX cases_channel ON cases(channel_id,updated_at DESC);
CREATE TABLE case_events (id uuid PRIMARY KEY, case_id uuid REFERENCES cases(id), type text NOT NULL, actor_id text NOT NULL DEFAULT '', body text NOT NULL DEFAULT '', metadata jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX case_events_cursor ON case_events(case_id,created_at,id);
CREATE TABLE ingest_events (id uuid PRIMARY KEY, connector_id uuid NOT NULL REFERENCES configs(id), event_id text NOT NULL, kind text NOT NULL, payload jsonb NOT NULL, status text NOT NULL DEFAULT 'pending', created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(connector_id,event_id));
CREATE TABLE source_messages (
 id uuid PRIMARY KEY, connector_id uuid NOT NULL REFERENCES configs(id), channel_id text NOT NULL, external_id text NOT NULL,
 case_id uuid REFERENCES cases(id), actor_id text NOT NULL DEFAULT '', content text NOT NULL, thread_id text NOT NULL DEFAULT '',
 source_url text NOT NULL DEFAULT '', source_revision text NOT NULL DEFAULT '', deleted boolean NOT NULL DEFAULT false,
 version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(connector_id,channel_id,external_id)
);
CREATE TABLE source_message_versions (message_id uuid NOT NULL REFERENCES source_messages(id), version bigint NOT NULL, content text NOT NULL, source_revision text NOT NULL, deleted boolean NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(message_id,version));
CREATE TABLE knowledge (
 id uuid PRIMARY KEY, title text NOT NULL, body text NOT NULL, module text NOT NULL DEFAULT 'general', status text NOT NULL DEFAULT 'draft', visibility text NOT NULL DEFAULT 'internal',
 channel_ids text[] NOT NULL DEFAULT '{}', cli_version text NOT NULL DEFAULT '', environment text NOT NULL DEFAULT '', source_url text NOT NULL DEFAULT '', source_revision text NOT NULL DEFAULT '',
 source_id text NOT NULL DEFAULT '', permission_checked_at timestamptz NOT NULL DEFAULT now(), valid_until timestamptz, author_id text NOT NULL,
 version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX knowledge_search ON knowledge USING gin ((title || ' ' || body) gin_trgm_ops);
CREATE TABLE knowledge_versions (knowledge_id uuid NOT NULL REFERENCES knowledge(id), version bigint NOT NULL, snapshot jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(knowledge_id,version));
CREATE TABLE replies (
 id uuid PRIMARY KEY, case_id uuid NOT NULL REFERENCES cases(id), body text NOT NULL DEFAULT '', status text NOT NULL DEFAULT 'draft', author_id text NOT NULL DEFAULT '',
 evidence_ids text[] NOT NULL DEFAULT '{}', evidence jsonb NOT NULL DEFAULT '[]', approval_id text NOT NULL DEFAULT '', external_operation_id text NOT NULL DEFAULT '',
 case_version bigint NOT NULL, version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE dev_tasks (
 id uuid PRIMARY KEY, case_id uuid NOT NULL REFERENCES cases(id), canonical_case_id uuid NOT NULL REFERENCES cases(id), repository_id uuid NOT NULL REFERENCES configs(id),
 title text NOT NULL, goal text NOT NULL, acceptance_criteria text NOT NULL, status text NOT NULL DEFAULT 'pending', stage text NOT NULL DEFAULT 'plan',
 revision bigint NOT NULL DEFAULT 1, epoch bigint NOT NULL DEFAULT 1, base_commit text NOT NULL DEFAULT '', head_commit text NOT NULL DEFAULT '', pr_url text NOT NULL DEFAULT '',
 error text NOT NULL DEFAULT '', confirmed_by text NOT NULL, version bigint NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX one_active_defect_task ON dev_tasks(canonical_case_id,repository_id) WHERE status NOT IN ('completed','cancelled','failed');
CREATE TABLE task_revisions (task_id uuid NOT NULL REFERENCES dev_tasks(id), revision bigint NOT NULL, snapshot jsonb NOT NULL, actor_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(task_id,revision));
CREATE TABLE task_steps (
 id uuid PRIMARY KEY, task_id uuid NOT NULL REFERENCES dev_tasks(id), revision bigint NOT NULL, stage text NOT NULL, status text NOT NULL DEFAULT 'pending_approval',
 input jsonb NOT NULL DEFAULT '{}', output jsonb NOT NULL DEFAULT '{}', approval_id text NOT NULL DEFAULT '', attempt_id text NOT NULL DEFAULT '', error text NOT NULL DEFAULT '',
 version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(task_id,revision,stage)
);
CREATE TABLE run_attempts (
 id uuid PRIMARY KEY, task_id uuid NOT NULL REFERENCES dev_tasks(id), step_id uuid NOT NULL REFERENCES task_steps(id), epoch bigint NOT NULL,
 stage text NOT NULL, status text NOT NULL DEFAULT 'queued', action_digest text NOT NULL, spec_digest text NOT NULL DEFAULT '', spec jsonb NOT NULL DEFAULT '{}',
 result jsonb NOT NULL DEFAULT '{}', error text NOT NULL DEFAULT '', started_at timestamptz, finished_at timestamptz,
 version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE run_events (id uuid PRIMARY KEY, run_id uuid NOT NULL REFERENCES run_attempts(id), sequence bigint NOT NULL, type text NOT NULL, body text NOT NULL DEFAULT '', metadata jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(run_id,sequence));
CREATE TABLE approvals (
 id uuid PRIMARY KEY, kind text NOT NULL, subject_id uuid NOT NULL, subject_type text NOT NULL, status text NOT NULL DEFAULT 'pending',
 summary text NOT NULL, action jsonb NOT NULL, action_hash text NOT NULL, policy_id text NOT NULL DEFAULT '', actor_id text NOT NULL DEFAULT '', reason text NOT NULL DEFAULT '',
 expires_at timestamptz NOT NULL, version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX approval_queue ON approvals(status,created_at DESC,id);
CREATE TABLE approval_decisions (id uuid PRIMARY KEY, approval_id uuid NOT NULL REFERENCES approvals(id), decision text NOT NULL, actor_id text NOT NULL, reason text NOT NULL, policy_id text NOT NULL DEFAULT '', evidence_hash text NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE jobs (
 id uuid PRIMARY KEY, kind text NOT NULL, dedupe_key text NOT NULL UNIQUE, payload jsonb NOT NULL, status text NOT NULL DEFAULT 'pending',
 attempts integer NOT NULL DEFAULT 0, available_at timestamptz NOT NULL DEFAULT now(), lease_token text NOT NULL DEFAULT '', lease_until timestamptz,
 last_error text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_ready ON jobs(available_at,created_at) WHERE status IN ('pending','running');
CREATE TABLE external_operations (
 id uuid PRIMARY KEY, kind text NOT NULL, subject_id text NOT NULL, dedupe_key text NOT NULL UNIQUE, status text NOT NULL DEFAULT 'pending',
 target jsonb NOT NULL, request jsonb NOT NULL, response jsonb NOT NULL DEFAULT '{}', approval_id text NOT NULL DEFAULT '',
 remote_id text NOT NULL DEFAULT '', error text NOT NULL DEFAULT '', version bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE artifacts (
 id uuid PRIMARY KEY, task_id text NOT NULL DEFAULT '', case_id text NOT NULL DEFAULT '', run_id text NOT NULL DEFAULT '', name text NOT NULL,
 relative_path text NOT NULL UNIQUE, sha256 text NOT NULL, size bigint NOT NULL CHECK(size>=0), media_type text NOT NULL, status text NOT NULL DEFAULT 'ready',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE audit_events (id uuid PRIMARY KEY, actor_id text NOT NULL, action text NOT NULL, resource_type text NOT NULL, resource_id text NOT NULL, result text NOT NULL DEFAULT 'success', detail text NOT NULL DEFAULT '', metadata jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX audit_cursor ON audit_events(created_at DESC,id);
CREATE TABLE idempotency_keys (actor_id text NOT NULL, key text NOT NULL, request_hash text NOT NULL, response jsonb NOT NULL, status_code integer NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(actor_id,key));
CREATE TABLE budget_reservations (id uuid PRIMARY KEY, scope text NOT NULL, amount bigint NOT NULL, actual bigint, status text NOT NULL DEFAULT 'reserved', created_at timestamptz NOT NULL DEFAULT now());
