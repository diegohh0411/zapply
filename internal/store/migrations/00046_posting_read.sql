-- A posting read's extraction and the retries that replace an answer whose
-- description sentences the page does not confirm (posting.Check). The first
-- attempt is the read's own operation; each retry is a new operation, quoted,
-- reserved and settled like any extraction. next_operation_id links an
-- attempt to the one that replaced it, so a poll follows the chain to its
-- latest answer. problems keeps why an answer was rejected, for diagnosis.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE posting_read (
 user_id uuid NOT NULL,
 operation_id uuid NOT NULL,
 attempt integer NOT NULL CHECK(attempt BETWEEN 1 AND 3),
 problems text[] NOT NULL DEFAULT '{}',
 next_operation_id uuid,
 PRIMARY KEY(user_id,operation_id),
 FOREIGN KEY(user_id,operation_id) REFERENCES model_operation(user_id,id) ON DELETE CASCADE,
 FOREIGN KEY(user_id,next_operation_id) REFERENCES model_operation(user_id,id) ON DELETE CASCADE
);
ALTER TABLE posting_read ENABLE ROW LEVEL SECURITY;
ALTER TABLE posting_read FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON posting_read TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());
GRANT SELECT,INSERT,UPDATE ON posting_read TO zapply_app,zapply_worker;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE posting_read;
-- +goose StatementEnd
