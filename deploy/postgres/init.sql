-- Development and test clusters only. In production, create zapply_app and
-- zapply_relay with LOGIN and managed secrets; neither may be superuser or
-- BYPASSRLS.
CREATE ROLE zapply_app LOGIN PASSWORD 'zapply_app' NOSUPERUSER NOBYPASSRLS;
CREATE ROLE zapply_relay LOGIN PASSWORD 'zapply_relay' NOSUPERUSER NOBYPASSRLS;
