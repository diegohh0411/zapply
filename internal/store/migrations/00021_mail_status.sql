-- Mail proposes job status changes. The request that links a message to a job
-- also asks what that job's status should be after the email. A confident move forward is
-- applied; anything else waits for the member to apply or dismiss it.
-- +goose Up
-- +goose StatementBegin
ALTER TABLE mail_message
 -- no_change: the email leaves the job where it is. review: a proposal waiting
 -- for the member. applied: the job was moved (by the worker or the member).
 -- dismissed: the member declined the proposal. undone: an applied move was reverted.
 ADD COLUMN status_outcome text CHECK(status_outcome IN ('no_change','review','applied','dismissed','undone')),
 ADD COLUMN suggested_status_id uuid,
 -- The job's status before an applied move, so it can be undone.
 ADD COLUMN previous_status_id uuid,
 ADD COLUMN status_confidence real CHECK(status_confidence BETWEEN 0 AND 1),
 ADD COLUMN status_auto boolean NOT NULL DEFAULT false,
 ADD FOREIGN KEY(user_id,suggested_status_id) REFERENCES job_status(user_id,id) ON DELETE SET NULL (suggested_status_id),
 ADD FOREIGN KEY(user_id,previous_status_id) REFERENCES job_status(user_id,id) ON DELETE SET NULL (previous_status_id);
CREATE INDEX mail_message_review ON mail_message(user_id,received_at DESC) WHERE status_outcome='review';

-- The member resolves proposals; nothing else about a message is theirs to write.
GRANT UPDATE(status_outcome,previous_status_id,updated_at) ON mail_message TO zapply_app;
-- The worker moves jobs when it applies a proposal.
GRANT UPDATE ON job TO zapply_worker;
-- Moving a job locks its status row FOR UPDATE, which needs an UPDATE grant.
GRANT UPDATE(updated_at) ON job_status TO zapply_worker;
GRANT SELECT,INSERT ON job_activity TO zapply_worker;
ALTER POLICY tenant ON job_activity TO zapply_app,zapply_worker;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER POLICY tenant ON job_activity TO zapply_app;
REVOKE SELECT,INSERT ON job_activity FROM zapply_worker;
REVOKE UPDATE(updated_at) ON job_status FROM zapply_worker;
REVOKE UPDATE ON job FROM zapply_worker;
REVOKE UPDATE(status_outcome,previous_status_id,updated_at) ON mail_message FROM zapply_app;
DROP INDEX mail_message_review;
ALTER TABLE mail_message DROP COLUMN status_outcome, DROP COLUMN suggested_status_id, DROP COLUMN previous_status_id,
 DROP COLUMN status_confidence, DROP COLUMN status_auto;
-- +goose StatementEnd
