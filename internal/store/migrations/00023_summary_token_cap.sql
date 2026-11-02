-- +goose Up
-- +goose StatementBegin
ALTER TABLE platform.model_settings ADD COLUMN summary_max_output_tokens integer NOT NULL DEFAULT 2048
 CHECK (summary_max_output_tokens IN (1024,2048,4096,8192));
CREATE OR REPLACE FUNCTION platform.summary_settings() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('model',summary_model,'endpoint',summary_endpoint,'max_output_tokens',summary_max_output_tokens) FROM platform.model_settings WHERE id
$$;
CREATE FUNCTION platform.save_summary_settings(p_model text,p_endpoint text,p_max_output_tokens integer) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.model_settings SET summary_model=p_model,summary_endpoint=p_endpoint,
 summary_max_output_tokens=COALESCE(p_max_output_tokens,summary_max_output_tokens) WHERE id;
 UPDATE public.cv_summary SET dirty=true,changed_at=now();
 END $$;
REVOKE EXECUTE ON FUNCTION platform.save_summary_settings(text,text,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.save_summary_settings(text,text,integer) TO zapply_app;
ALTER FUNCTION platform.save_summary_settings(text,text,integer) OWNER TO zapply_platform;
-- Retry existing summaries with the new budget after deployment.
UPDATE public.cv_summary SET dirty=true,changed_at=now();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.save_summary_settings(text,text,integer);
CREATE OR REPLACE FUNCTION platform.summary_settings() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('model',summary_model,'endpoint',summary_endpoint) FROM platform.model_settings WHERE id
$$;
ALTER TABLE platform.model_settings DROP COLUMN summary_max_output_tokens;
-- +goose StatementEnd
