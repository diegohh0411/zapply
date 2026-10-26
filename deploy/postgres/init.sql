-- Development and test clusters only. In production, create zapply_app with
-- LOGIN and a managed secret; it must not be superuser or BYPASSRLS.
CREATE ROLE zapply_app LOGIN PASSWORD 'zapply_app' NOSUPERUSER NOBYPASSRLS;
