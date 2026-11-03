-- +goose Up
GRANT SELECT, UPDATE ON cv_draft TO zapply_worker;
ALTER POLICY tenant ON cv_draft TO zapply_app, zapply_worker;

-- +goose Down
ALTER POLICY tenant ON cv_draft TO zapply_app;
REVOKE SELECT, UPDATE ON cv_draft FROM zapply_worker;
