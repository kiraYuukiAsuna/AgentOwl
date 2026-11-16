ALTER TABLE external_operations ADD COLUMN retry_attempts integer NOT NULL DEFAULT 0;
