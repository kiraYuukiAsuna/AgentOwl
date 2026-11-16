CREATE TABLE system_settings (
 name text PRIMARY KEY,
 data jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(data) = 'object'),
 secret text NOT NULL DEFAULT '',
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 updated_by uuid REFERENCES principals(id)
);
