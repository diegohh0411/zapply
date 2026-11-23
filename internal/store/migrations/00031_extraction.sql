-- +goose Up
-- +goose StatementBegin
-- Extraction models read typed fields out of text (a scraped posting) through
-- the same quote, reserve and settle path as decisions. The owner selects one.
ALTER TABLE model_operation DROP CONSTRAINT model_operation_kind_check;
ALTER TABLE model_operation ADD CONSTRAINT model_operation_kind_check CHECK(kind IN ('diagnostic','decision','summary','extraction'));
ALTER TABLE platform.model_settings ADD COLUMN extraction_model text
 CHECK (extraction_model IS NULL OR length(btrim(extraction_model)) BETWEEN 1 AND 255);
-- The migration login can SET ROLE but does not inherit platform function ownership.
SET LOCAL ROLE zapply_platform;
CREATE FUNCTION platform.selected_extraction_model() RETURNS text LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT extraction_model FROM platform.model_settings WHERE id
$$;
CREATE FUNCTION platform.save_extraction_model(p_model text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.model_settings SET extraction_model=p_model WHERE id;
 PERFORM platform.audit_event('model_settings.saved',public.app_user_id(),jsonb_build_object('extraction_model',p_model));
 END $$;
REVOKE EXECUTE ON FUNCTION platform.selected_extraction_model(),platform.save_extraction_model(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.selected_extraction_model() TO zapply_app,zapply_worker;
GRANT EXECUTE ON FUNCTION platform.save_extraction_model(text) TO zapply_app;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.save_extraction_model(text),platform.selected_extraction_model();
ALTER TABLE platform.model_settings DROP COLUMN extraction_model;
-- Settled extraction operations keep their ledger rows, so the old check
-- applies to new rows only.
ALTER TABLE model_operation DROP CONSTRAINT model_operation_kind_check;
ALTER TABLE model_operation ADD CONSTRAINT model_operation_kind_check CHECK(kind IN ('diagnostic','decision','summary')) NOT VALID;
-- +goose StatementEnd
