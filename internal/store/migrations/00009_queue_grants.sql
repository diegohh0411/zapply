-- +goose Up
REVOKE ALL ON SCHEMA river FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA river FROM PUBLIC, zapply_app, zapply_worker;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA river FROM PUBLIC, zapply_app, zapply_worker;
GRANT USAGE ON SCHEMA river TO zapply_app, zapply_worker;
GRANT SELECT, INSERT, UPDATE ON river.river_job TO zapply_app;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA river TO zapply_app, zapply_worker;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA river TO zapply_worker;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA river TO zapply_app, zapply_worker;

-- +goose Down
REVOKE ALL ON ALL TABLES IN SCHEMA river FROM zapply_app,zapply_worker;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA river FROM zapply_app,zapply_worker;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA river FROM zapply_app,zapply_worker;
