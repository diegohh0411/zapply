-- The member can correct which job a message is about. A manual link is marked
-- so the classifier treats it as ground truth: it never overwrites it, and later
-- mail from the same sender leans toward the job the member chose.
-- +goose Up
-- +goose StatementBegin
ALTER TABLE mail_message ADD COLUMN job_manual boolean NOT NULL DEFAULT false;
-- Relinking clears the old job's status proposal, which belonged to that job.
GRANT UPDATE(job_id,job_manual,suggested_status_id,status_confidence,status_auto) ON mail_message TO zapply_app;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
REVOKE UPDATE(job_id,job_manual,suggested_status_id,status_confidence,status_auto) ON mail_message FROM zapply_app;
ALTER TABLE mail_message DROP COLUMN job_manual;
-- +goose StatementEnd
