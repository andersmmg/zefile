-- Per-user storage quotas, in bytes. Zero means unlimited: the account is
-- bounded only by the instance quota, if any. Existing rows default to zero,
-- so upgrading changes no one's effective limit.

-- +goose Up
ALTER TABLE users ADD COLUMN quota_bytes INTEGER NOT NULL DEFAULT 0;
