ALTER TABLE dev_tasks ADD COLUMN mandate_id uuid;
ALTER TABLE dev_tasks ADD COLUMN hold_reason text NOT NULL DEFAULT '';
ALTER TABLE dev_tasks ADD COLUMN next_input_seq bigint NOT NULL DEFAULT 0;
CREATE TABLE source_threads (
 id uuid PRIMARY KEY, connector_id uuid NOT NULL REFERENCES configs(id), channel_key text NOT NULL,
 external_thread_id text NOT NULL, generation bigint NOT NULL DEFAULT 0,
 processed_generation bigint NOT NULL DEFAULT 0,
 UNIQUE(connector_id,channel_key,external_thread_id)
);
ALTER TABLE ingest_events ADD COLUMN thread_id uuid REFERENCES source_threads(id);
ALTER TABLE ingest_events ADD COLUMN thread_generation bigint;
CREATE TABLE task_threads (
 task_id uuid NOT NULL REFERENCES dev_tasks(id), thread_id uuid NOT NULL REFERENCES source_threads(id),
 repository_id uuid NOT NULL REFERENCES configs(id), relation text NOT NULL CHECK(relation IN ('owner','context','execution')),
 routing_active boolean NOT NULL DEFAULT true, routed_generation bigint NOT NULL DEFAULT 0,
 resolved_generation bigint NOT NULL DEFAULT 0, attached_by text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(task_id,thread_id)
);
CREATE UNIQUE INDEX task_thread_owner ON task_threads(thread_id,repository_id) WHERE routing_active AND relation='owner';
CREATE TABLE task_inputs (
 id uuid PRIMARY KEY, task_id uuid NOT NULL REFERENCES dev_tasks(id), seq bigint NOT NULL,
 thread_id uuid REFERENCES source_threads(id), generation bigint,
 source_message_id uuid REFERENCES source_messages(id), source_version bigint,
 kind text NOT NULL, actor_ref jsonb NOT NULL DEFAULT '{}', body text NOT NULL DEFAULT '', digest text NOT NULL,
 disposition text NOT NULL CHECK(disposition IN ('pending','context','apply','review','unrelated','withdrawn','superseded')),
 reason_code text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(task_id,seq), UNIQUE(task_id,source_message_id,source_version,kind)
);
CREATE INDEX task_input_inbox ON task_inputs(task_id,disposition,seq);
CREATE TABLE task_revision_inputs (
 task_id uuid NOT NULL REFERENCES dev_tasks(id), revision bigint NOT NULL, input_id uuid NOT NULL REFERENCES task_inputs(id),
 content_digest text NOT NULL, source_version bigint, PRIMARY KEY(task_id,revision,input_id)
);
CREATE TABLE task_mandates (
 id uuid PRIMARY KEY, task_id uuid NOT NULL REFERENCES dev_tasks(id), revision bigint NOT NULL,
 decision_id uuid, issuer text NOT NULL, sponsor_principal_id uuid REFERENCES principals(id),
 scope jsonb NOT NULL, scope_digest text NOT NULL, policy_ref jsonb NOT NULL DEFAULT '{}',
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','revoked','superseded')),
 reason text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz,
 UNIQUE(task_id,revision)
);
ALTER TABLE dev_tasks ADD CONSTRAINT task_mandate_fk FOREIGN KEY(mandate_id) REFERENCES task_mandates(id);
CREATE TABLE task_events (
 id uuid PRIMARY KEY, task_id uuid NOT NULL REFERENCES dev_tasks(id), sequence bigint GENERATED ALWAYS AS IDENTITY,
 type text NOT NULL, actor text NOT NULL, metadata jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(task_id,sequence)
);
CREATE TABLE run_input_receipts (
 run_id uuid NOT NULL REFERENCES run_attempts(id), input_id uuid NOT NULL REFERENCES task_inputs(id),
 phase text NOT NULL CHECK(phase IN ('delivered','included')), content_digest text NOT NULL, part_id text NOT NULL,
 receipt_key text NOT NULL UNIQUE, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(run_id,input_id,phase,part_id)
);
CREATE TABLE task_input_evaluations (
 id uuid PRIMARY KEY, task_id uuid NOT NULL REFERENCES dev_tasks(id), input_ids jsonb NOT NULL,
 input_digest text NOT NULL, task_revision bigint NOT NULL, policy_version bigint NOT NULL,
 model_version bigint NOT NULL, state text NOT NULL DEFAULT 'pending', result jsonb NOT NULL DEFAULT '{}',
 started_at timestamptz, deadline timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX active_input_evaluation ON task_input_evaluations(task_id) WHERE state IN ('pending','running');
CREATE TABLE admission_evaluations (
 id uuid PRIMARY KEY, case_id uuid NOT NULL REFERENCES cases(id), case_version bigint NOT NULL,
 thread_id uuid REFERENCES source_threads(id), generation bigint NOT NULL DEFAULT 0, input_digest text NOT NULL,
 policy_version bigint NOT NULL, model_version bigint NOT NULL, snapshot jsonb NOT NULL, request_key text NOT NULL DEFAULT '',
 state text NOT NULL DEFAULT 'pending', deadline timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(case_id,case_version,policy_version,request_key)
);
CREATE TABLE admission_decisions (
 id uuid PRIMARY KEY, evaluation_id uuid UNIQUE REFERENCES admission_evaluations(id), case_id uuid NOT NULL REFERENCES cases(id),
 repository_id uuid REFERENCES configs(id), outcome text NOT NULL, reason_code text NOT NULL DEFAULT '',
 evidence jsonb NOT NULL DEFAULT '{}', authority_basis jsonb NOT NULL DEFAULT '{}', task_id uuid REFERENCES dev_tasks(id),
 created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE task_mandates ADD CONSTRAINT mandate_decision_fk FOREIGN KEY(decision_id) REFERENCES admission_decisions(id);
INSERT INTO system_settings(name,data,version) VALUES('admission','{"mode":"off"}',1) ON CONFLICT DO NOTHING;
