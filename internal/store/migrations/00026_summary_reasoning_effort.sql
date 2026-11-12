-- +goose Up
-- +goose StatementBegin
ALTER TABLE platform.model_settings ADD COLUMN summary_reasoning_effort text
 CHECK (summary_reasoning_effort IN ('off','minimal','low','medium','high','xhigh','max'));
-- The migration login can SET ROLE but does not inherit platform function ownership.
SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.summary_settings() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('model',summary_model,'endpoint',summary_endpoint,'max_output_tokens',summary_max_output_tokens,'max_characters',summary_max_characters,'reasoning_effort',summary_reasoning_effort) FROM platform.model_settings WHERE id
$$;
DROP FUNCTION platform.save_summary_settings(text,text,integer,integer);
-- A NULL effort is automatic: summaries keep the bounded default reasoning policy.
CREATE FUNCTION platform.save_summary_settings(p_model text,p_endpoint text,p_max_output_tokens integer,p_max_characters integer,p_reasoning_effort text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.model_settings SET summary_model=p_model,summary_endpoint=p_endpoint,
 summary_max_output_tokens=COALESCE(p_max_output_tokens,summary_max_output_tokens),
 summary_max_characters=COALESCE(p_max_characters,summary_max_characters),
 summary_reasoning_effort=p_reasoning_effort WHERE id;
 UPDATE public.cv_summary SET dirty=true,changed_at=now();
 END $$;
REVOKE EXECUTE ON FUNCTION platform.save_summary_settings(text,text,integer,integer,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.save_summary_settings(text,text,integer,integer,text) TO zapply_app;
UPDATE public.cv_summary SET dirty=true,changed_at=now();
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;
DROP FUNCTION platform.save_summary_settings(text,text,integer,integer,text);
CREATE FUNCTION platform.save_summary_settings(p_model text,p_endpoint text,p_max_output_tokens integer,p_max_characters integer) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.model_settings SET summary_model=p_model,summary_endpoint=p_endpoint,
 summary_max_output_tokens=COALESCE(p_max_output_tokens,summary_max_output_tokens),
 summary_max_characters=COALESCE(p_max_characters,summary_max_characters) WHERE id;
 UPDATE public.cv_summary SET dirty=true,changed_at=now();
 END $$;
REVOKE EXECUTE ON FUNCTION platform.save_summary_settings(text,text,integer,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.save_summary_settings(text,text,integer,integer) TO zapply_app;
CREATE OR REPLACE FUNCTION platform.summary_settings() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('model',summary_model,'endpoint',summary_endpoint,'max_output_tokens',summary_max_output_tokens,'max_characters',summary_max_characters) FROM platform.model_settings WHERE id
$$;
RESET ROLE;
ALTER TABLE platform.model_settings DROP COLUMN summary_reasoning_effort;
-- +goose StatementEnd
