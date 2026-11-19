-- +goose Up
-- +goose StatementBegin
-- Summaries are sized by the rows they fill on the page, not by characters,
-- and their output token cap is fixed in code.
ALTER TABLE platform.model_settings ADD COLUMN summary_rows integer NOT NULL DEFAULT 2
 CHECK (summary_rows BETWEEN 1 AND 4);
ALTER TABLE platform.model_settings DROP COLUMN summary_max_characters, DROP COLUMN summary_max_output_tokens;
-- The migration login can SET ROLE but does not inherit platform function ownership.
SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.summary_settings() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('model',summary_model,'endpoint',summary_endpoint,'rows',summary_rows,'reasoning_effort',summary_reasoning_effort) FROM platform.model_settings WHERE id
$$;
DROP FUNCTION platform.save_summary_settings(text,text,integer,integer,text);
CREATE FUNCTION platform.save_summary_settings(p_model text,p_endpoint text,p_rows integer,p_reasoning_effort text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.model_settings SET summary_model=p_model,summary_endpoint=p_endpoint,
 summary_rows=COALESCE(p_rows,summary_rows),
 summary_reasoning_effort=p_reasoning_effort WHERE id;
 UPDATE public.cv_summary SET dirty=true,changed_at=now();
 END $$;
REVOKE EXECUTE ON FUNCTION platform.save_summary_settings(text,text,integer,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.save_summary_settings(text,text,integer,text) TO zapply_app;
UPDATE public.cv_summary SET dirty=true,changed_at=now();
RESET ROLE;
-- A summary that wraps to the wrong number of rows is sent back to the model
-- with feedback. Each turn after the first request is kept here, in order,
-- since model_request bodies never change.
CREATE TABLE summary_turn (
 operation_id uuid NOT NULL REFERENCES model_operation(id) ON DELETE CASCADE,
 n integer NOT NULL CHECK (n >= 0),
 user_id uuid NOT NULL,
 role text NOT NULL CHECK (role IN ('assistant','user')),
 content text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (operation_id, n)
);
ALTER TABLE summary_turn ENABLE ROW LEVEL SECURITY;
ALTER TABLE summary_turn FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON summary_turn TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());
GRANT SELECT ON summary_turn TO zapply_app;
GRANT SELECT,INSERT ON summary_turn TO zapply_worker;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE summary_turn;
ALTER TABLE platform.model_settings
 ADD COLUMN summary_max_output_tokens integer NOT NULL DEFAULT 2048 CHECK (summary_max_output_tokens IN (1024,2048,4096,8192)),
 ADD COLUMN summary_max_characters integer NOT NULL DEFAULT 500 CHECK (summary_max_characters IN (100,200,300,400,500));
SET LOCAL ROLE zapply_platform;
DROP FUNCTION platform.save_summary_settings(text,text,integer,text);
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
CREATE OR REPLACE FUNCTION platform.summary_settings() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('model',summary_model,'endpoint',summary_endpoint,'max_output_tokens',summary_max_output_tokens,'max_characters',summary_max_characters,'reasoning_effort',summary_reasoning_effort) FROM platform.model_settings WHERE id
$$;
RESET ROLE;
ALTER TABLE platform.model_settings DROP COLUMN summary_rows;
-- +goose StatementEnd
