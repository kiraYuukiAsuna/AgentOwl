CREATE TABLE runtime_machines (
 id uuid PRIMARY KEY,
 name text NOT NULL,
 base_url text NOT NULL,
 callback_base_url text NOT NULL DEFAULT '',
 labels jsonb NOT NULL DEFAULT '{}',
 scheduling_weight integer NOT NULL DEFAULT 1,
 status text NOT NULL DEFAULT 'active',
 allowed_images text[] NOT NULL DEFAULT '{}',
 execution_version bigint NOT NULL DEFAULT 0,
 model_version bigint NOT NULL DEFAULT 0,
 capabilities jsonb NOT NULL DEFAULT '{}',
 secret_ref text NOT NULL DEFAULT '',
 callback_secret_ref text NOT NULL DEFAULT '',
 version bigint NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(name)
);

ALTER TABLE run_attempts ADD COLUMN runtime_machine_id uuid REFERENCES runtime_machines(id);
