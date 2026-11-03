-- +goose Up
-- +goose StatementBegin
ALTER TABLE model_operation DROP CONSTRAINT model_operation_kind_check;
ALTER TABLE model_operation ADD CONSTRAINT model_operation_kind_check CHECK(kind IN ('diagnostic','decision','summary'));
ALTER TABLE platform.model_settings ADD COLUMN summary_model text;
ALTER TABLE platform.model_settings ADD COLUMN summary_endpoint text;
ALTER TABLE cv_draft ADD COLUMN summary text;
CREATE TABLE cv_summary (
 user_id uuid NOT NULL, job_id uuid NOT NULL, input_hash text NOT NULL DEFAULT '',
 last_summary text NOT NULL DEFAULT '', operation_id uuid, dirty boolean NOT NULL DEFAULT true, changed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,job_id), FOREIGN KEY(user_id,job_id) REFERENCES cv_draft(user_id,job_id) ON DELETE CASCADE
);
ALTER TABLE cv_summary ENABLE ROW LEVEL SECURITY;
ALTER TABLE cv_summary FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON cv_summary TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());
GRANT SELECT,INSERT,UPDATE ON cv_summary TO zapply_app,zapply_worker;
GRANT SELECT ON cv_summary TO zapply_platform;
CREATE POLICY platform_scan ON cv_summary TO zapply_platform USING(true);
GRANT INSERT ON cv_draft TO zapply_worker;
CREATE FUNCTION platform.summary_settings() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ SELECT jsonb_build_object('model',summary_model,'endpoint',summary_endpoint) FROM platform.model_settings WHERE id $$;
CREATE FUNCTION platform.save_summary_settings(p_model text,p_endpoint text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.model_settings SET summary_model=p_model,summary_endpoint=p_endpoint WHERE id;
 UPDATE public.cv_summary SET dirty=true,changed_at=now();
 END $$;
CREATE FUNCTION platform.dirty_summaries() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$ SELECT COALESCE(jsonb_agg(x),'[]') FROM (SELECT user_id,job_id FROM cv_summary WHERE dirty AND changed_at < now()-interval '700 milliseconds' ORDER BY changed_at LIMIT 100) x $$;
CREATE FUNCTION platform.invalidate_summary() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$ BEGIN
 IF NEW.topic LIKE 'evidence.%' OR NEW.topic LIKE 'skill.%' OR NEW.topic LIKE 'organization.%' OR NEW.topic LIKE 'profile.%' OR NEW.topic IN ('job.updated','cv_draft.updated','match_run.changed') THEN
 UPDATE cv_summary SET dirty=true,changed_at=now() WHERE user_id=NEW.user_id AND (NEW.topic NOT IN ('job.updated','cv_draft.updated') OR job_id=COALESCE((NEW.payload->>'id')::uuid,(NEW.payload->>'job_id')::uuid));
 END IF;
 RETURN NEW; END $$;
CREATE TRIGGER invalidate_summary AFTER INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION platform.invalidate_summary();
GRANT UPDATE ON cv_summary TO zapply_platform;
GRANT SELECT ON outbox TO zapply_platform;
REVOKE EXECUTE ON FUNCTION platform.summary_settings(),platform.save_summary_settings(text,text),platform.dirty_summaries(),platform.invalidate_summary() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.summary_settings() TO zapply_app,zapply_worker;
GRANT EXECUTE ON FUNCTION platform.save_summary_settings(text,text) TO zapply_app;
GRANT EXECUTE ON FUNCTION platform.dirty_summaries() TO zapply_worker;
ALTER FUNCTION platform.summary_settings() OWNER TO zapply_platform;
ALTER FUNCTION platform.save_summary_settings(text,text) OWNER TO zapply_platform;
ALTER FUNCTION platform.dirty_summaries() OWNER TO zapply_platform;
ALTER FUNCTION platform.invalidate_summary() OWNER TO zapply_platform;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DROP TRIGGER invalidate_summary ON outbox;
DROP FUNCTION platform.invalidate_summary(),platform.dirty_summaries(),platform.save_summary_settings(text,text),platform.summary_settings();
DROP TABLE cv_summary;
ALTER TABLE cv_draft DROP COLUMN summary;
ALTER TABLE platform.model_settings DROP COLUMN summary_model,DROP COLUMN summary_endpoint;
ALTER TABLE model_operation DROP CONSTRAINT model_operation_kind_check;
ALTER TABLE model_operation ADD CONSTRAINT model_operation_kind_check CHECK(kind IN ('diagnostic','decision'));
-- +goose StatementEnd
