-- +goose Up
-- +goose StatementBegin
-- Extraction operations read a posting's fields with a typed extraction model
-- (OpenRouter /api/alpha/extractions). They are metered like decisions.
ALTER TABLE model_operation DROP CONSTRAINT model_operation_kind_check;
ALTER TABLE model_operation ADD CONSTRAINT model_operation_kind_check CHECK(kind IN ('diagnostic','decision','summary','extraction'));
ALTER TABLE platform.model_settings ADD COLUMN extraction_model text DEFAULT 'cloudflare/clairic';
SET LOCAL ROLE zapply_platform;
CREATE FUNCTION platform.selected_extraction_model() RETURNS text LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT extraction_model FROM platform.model_settings WHERE id
$$;
REVOKE EXECUTE ON FUNCTION platform.selected_extraction_model() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.selected_extraction_model() TO zapply_app,zapply_worker;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.selected_extraction_model();
ALTER TABLE platform.model_settings DROP COLUMN extraction_model;
ALTER TABLE model_operation DROP CONSTRAINT model_operation_kind_check;
ALTER TABLE model_operation ADD CONSTRAINT model_operation_kind_check CHECK(kind IN ('diagnostic','decision','summary'));
-- +goose StatementEnd
