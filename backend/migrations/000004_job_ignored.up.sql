ALTER TABLE jobs ADD COLUMN ignored_at timestamptz;
ALTER TABLE jobs ADD COLUMN ignored_by text NOT NULL DEFAULT '';
CREATE INDEX jobs_unignored_failed ON jobs (created_at DESC, id DESC) WHERE status='failed' AND ignored_at IS NULL;
