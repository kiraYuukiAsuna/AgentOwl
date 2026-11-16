ALTER TABLE approvals ADD COLUMN policy_version bigint NOT NULL DEFAULT 0;
ALTER TABLE approval_decisions ADD COLUMN policy_version bigint NOT NULL DEFAULT 0;
