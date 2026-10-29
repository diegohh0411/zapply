-- +goose Up
ALTER TABLE mail_account ADD COLUMN sync_pending jsonb,
 ADD COLUMN sync_window_start timestamptz,
 ADD COLUMN sync_window_reads integer NOT NULL DEFAULT 0,
 ADD COLUMN sync_retry_at timestamptz,
 ADD COLUMN sync_quota_failures integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE mail_account DROP COLUMN sync_pending, DROP COLUMN sync_window_start,
 DROP COLUMN sync_window_reads, DROP COLUMN sync_retry_at, DROP COLUMN sync_quota_failures;
