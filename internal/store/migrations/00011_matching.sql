-- +goose Up
-- +goose StatementBegin
ALTER TABLE profile ADD COLUMN auto_match boolean NOT NULL DEFAULT true;
ALTER TABLE llm_operation ADD CONSTRAINT llm_operation_kind_check CHECK(kind IN ('diagnostic','decision'));
ALTER TABLE llm_operation DROP CONSTRAINT llm_operation_result_check;
ALTER TABLE llm_operation ADD CONSTRAINT llm_operation_result_check CHECK(length(result)<=262144);
GRANT INSERT ON llm_operation TO zapply_worker;

CREATE TABLE llm_request (
 user_id uuid NOT NULL, operation_id uuid PRIMARY KEY, body jsonb NOT NULL CHECK(jsonb_typeof(body)='object'), created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(user_id,operation_id) REFERENCES llm_operation(user_id,id) ON DELETE CASCADE
);
CREATE TABLE decision_request_item (
 user_id uuid NOT NULL, operation_id uuid NOT NULL, request_key text NOT NULL, model text NOT NULL,
 state_hash text NOT NULL, question_hash text NOT NULL,
 PRIMARY KEY(operation_id,request_key), FOREIGN KEY(user_id,operation_id) REFERENCES llm_operation(user_id,id) ON DELETE CASCADE
);
CREATE TABLE decision_answer (
 user_id uuid NOT NULL REFERENCES app_user(id), model text NOT NULL, state_hash text NOT NULL, question_hash text NOT NULL,
 answer jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,model,state_hash,question_hash)
);
CREATE TABLE match_run (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES app_user(id), job_id uuid NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 255), trigger text NOT NULL CHECK(trigger IN ('auto','manual')),
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','running','blocked','needs_review','completed','partial','failed','cancelled')),
 stage text NOT NULL DEFAULT 'read' CHECK(stage IN ('read','score','finalize')), model text,
 job_hash text NOT NULL, library_fingerprint text NOT NULL, skills_hash text NOT NULL,
 snapshot jsonb NOT NULL, result jsonb NOT NULL DEFAULT '{}', score double precision, must_met int NOT NULL DEFAULT 0, must_total int NOT NULL DEFAULT 0,
 blocked_reason text NOT NULL DEFAULT '', next_eligible_at timestamptz,
 rerun_requested boolean NOT NULL DEFAULT false, version int NOT NULL DEFAULT 1,
 total int NOT NULL DEFAULT 0, answered int NOT NULL DEFAULT 0, cached int NOT NULL DEFAULT 0, unanswered int NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz,
 UNIQUE(user_id,id), UNIQUE(user_id,idempotency_key), FOREIGN KEY(user_id,job_id) REFERENCES job(user_id,id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX match_run_active ON match_run(user_id,job_id) WHERE status IN ('pending','running','blocked','needs_review');
CREATE INDEX match_run_latest ON match_run(user_id,job_id,created_at DESC,id DESC);
CREATE TABLE match_run_item (
 user_id uuid NOT NULL, run_id uuid NOT NULL, evidence_id uuid NOT NULL, content_hash text NOT NULL, facts jsonb NOT NULL,
 PRIMARY KEY(user_id,run_id,evidence_id), FOREIGN KEY(user_id,run_id) REFERENCES match_run(user_id,id) ON DELETE CASCADE
);
CREATE TABLE match_run_requirement (
 user_id uuid NOT NULL, run_id uuid NOT NULL, text_hash text NOT NULL, facts jsonb NOT NULL,
 PRIMARY KEY(user_id,run_id,text_hash), FOREIGN KEY(user_id,run_id) REFERENCES match_run(user_id,id) ON DELETE CASCADE
);
CREATE TABLE match_run_question (
 user_id uuid NOT NULL, run_id uuid NOT NULL, stage text NOT NULL, subject text NOT NULL,
 state_hash text NOT NULL, question_hash text NOT NULL, state jsonb NOT NULL, question jsonb NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('cached','pending','answered','discarded','unanswered')),
 tries int NOT NULL DEFAULT 0, operation_id uuid, request_key text, answer jsonb,
 PRIMARY KEY(user_id,run_id,stage,subject), FOREIGN KEY(user_id,run_id) REFERENCES match_run(user_id,id) ON DELETE CASCADE,
 FOREIGN KEY(user_id,operation_id) REFERENCES llm_operation(user_id,id)
);
CREATE TABLE match_run_operation (
 user_id uuid NOT NULL, run_id uuid NOT NULL, operation_id uuid NOT NULL, ingested boolean NOT NULL DEFAULT false,
 PRIMARY KEY(user_id,run_id,operation_id), FOREIGN KEY(user_id,run_id) REFERENCES match_run(user_id,id) ON DELETE CASCADE,
 FOREIGN KEY(user_id,operation_id) REFERENCES llm_operation(user_id,id)
);
CREATE TABLE match_override (
 user_id uuid NOT NULL, job_id uuid NOT NULL, text_hash text NOT NULL, class text NOT NULL CHECK(class IN ('must','preferred','duty','ignore')),
 PRIMARY KEY(user_id,job_id,text_hash), FOREIGN KEY(user_id,job_id) REFERENCES job(user_id,id) ON DELETE CASCADE
);
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['llm_request','decision_request_item','decision_answer','match_run','match_run_item','match_run_requirement','match_run_question','match_override','match_run_operation'] LOOP
 EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant ON %I TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id())',t);
 EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON %I TO zapply_app,zapply_worker',t);
 END LOOP;
END $$;
REVOKE UPDATE,DELETE ON llm_request,decision_request_item,decision_answer FROM zapply_app,zapply_worker;
GRANT SELECT ON job,job_status,profile,evidence,highlight,skill,evidence_skill,highlight_skill,organization TO zapply_worker;
ALTER POLICY tenant ON job TO zapply_app,zapply_worker;
ALTER POLICY tenant ON job_status TO zapply_app,zapply_worker;

CREATE TABLE platform.model_settings(id boolean PRIMARY KEY DEFAULT true CHECK(id), decision_model text, version int NOT NULL DEFAULT 1, updated_at timestamptz NOT NULL DEFAULT now());
INSERT INTO platform.model_settings(id) VALUES(true);
CREATE TABLE platform.model_quote(model text PRIMARY KEY, quote jsonb NOT NULL, fetched_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE platform.match_index(run_id uuid PRIMARY KEY REFERENCES match_run(id) ON DELETE CASCADE,user_id uuid NOT NULL,status text NOT NULL,updated_at timestamptz NOT NULL DEFAULT now());
GRANT SELECT,INSERT,UPDATE ON platform.model_settings,platform.model_quote,platform.match_index TO zapply_platform;
GRANT SELECT ON platform.model_settings,platform.model_quote TO zapply_worker;
GRANT INSERT,UPDATE ON platform.model_quote TO zapply_worker;

GRANT SELECT,UPDATE(id) ON public.job TO zapply_platform;
CREATE POLICY platform_match_job ON job TO zapply_platform USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());
CREATE FUNCTION platform.lock_match_job(p_id uuid) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ DECLARE result uuid;BEGIN
 SELECT id INTO result FROM public.job WHERE user_id=public.app_user_id() AND id=p_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'job not found' USING ERRCODE='ZA404'; END IF;RETURN result;END $$;
REVOKE EXECUTE ON FUNCTION platform.lock_match_job(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.lock_match_job(uuid) TO zapply_app,zapply_worker;
ALTER FUNCTION platform.lock_match_job(uuid) OWNER TO zapply_platform;
CREATE FUNCTION platform.get_model_settings() RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner(); RETURN (SELECT to_jsonb(m)-'id' FROM platform.model_settings m WHERE id); END $$;
CREATE FUNCTION platform.save_model_settings(p_model text,p_version int) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 IF p_model IS NOT NULL AND (length(btrim(p_model)) NOT BETWEEN 1 AND 255) THEN RAISE EXCEPTION 'invalid model' USING ERRCODE='ZA422'; END IF;
 UPDATE platform.model_settings SET decision_model=p_model,version=version+1,updated_at=now() WHERE id AND version=p_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'settings changed' USING ERRCODE='ZA412'; END IF;
 PERFORM platform.audit_event('model_settings.saved',public.app_user_id(),jsonb_build_object('decision_model',p_model));
 RETURN platform.get_model_settings(); END $$;
CREATE FUNCTION platform.selected_decision_model() RETURNS text LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ SELECT decision_model FROM platform.model_settings WHERE id $$;
-- Catalogue calls use the existing encrypted key but do not create paid operations.
CREATE FUNCTION platform.catalogue_key() RETURNS TABLE(id uuid,ciphertext bytea,encryption_version text) LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner(); RETURN QUERY SELECT k.id,k.ciphertext,k.encryption_version FROM platform.system_key k WHERE active; END $$;
CREATE FUNCTION platform.match_operations_dispatched(p_ids uuid[]) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 RETURN EXISTS(SELECT FROM platform.llm_attempt WHERE user_id=public.app_user_id() AND operation_id=ANY(p_ids));END $$;
REVOKE EXECUTE ON FUNCTION platform.match_operations_dispatched(uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.match_operations_dispatched(uuid[]) TO zapply_app,zapply_worker;
ALTER FUNCTION platform.match_operations_dispatched(uuid[]) OWNER TO zapply_platform;
CREATE FUNCTION platform.cancel_operation(p_id uuid) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 -- Same lock as reserve: cancellation cannot win after a dispatch is fenced.
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 UPDATE public.llm_operation SET status='cancelled',version=version+1,updated_at=now() WHERE id=p_id AND user_id=public.app_user_id() AND status IN ('pending','blocked');
 IF NOT FOUND THEN RETURN false; END IF;
 PERFORM platform.emit(public.app_user_id(),'llm_operation.changed',p_id);RETURN true;END $$;
CREATE FUNCTION platform.index_match_run() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 INSERT INTO platform.match_index(run_id,user_id,status) VALUES(NEW.id,NEW.user_id,NEW.status) ON CONFLICT(run_id) DO UPDATE SET status=EXCLUDED.status,updated_at=now();RETURN NEW;END $$;
CREATE TRIGGER index_match_run AFTER INSERT OR UPDATE ON match_run FOR EACH ROW EXECUTE FUNCTION platform.index_match_run();
CREATE FUNCTION platform.active_match_runs(p_limit int) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT COALESCE(jsonb_agg(jsonb_build_object('user_id',user_id,'run_id',run_id)),'[]') FROM (SELECT * FROM platform.match_index WHERE status IN ('pending','running','blocked','needs_review') ORDER BY updated_at LIMIT least(greatest(p_limit,1),1000)) x $$;
CREATE FUNCTION platform.operation_costs(p_ids uuid[]) RETURNS text LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT platform.money(COALESCE(sum(u.amount),0)::bigint) FROM platform.llm_usage u JOIN platform.llm_attempt a ON a.id=u.attempt_id WHERE a.user_id=public.app_user_id() AND a.operation_id=ANY(p_ids) $$;
CREATE FUNCTION platform.decision_quote(p_model text) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ SELECT quote FROM platform.model_quote WHERE model=p_model $$;
REVOKE EXECUTE ON FUNCTION platform.decision_quote(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.decision_quote(text) TO zapply_app,zapply_worker;
ALTER FUNCTION platform.decision_quote(text) OWNER TO zapply_platform;
GRANT EXECUTE ON FUNCTION platform.get_model_settings(),platform.save_model_settings(text,int),platform.catalogue_key() TO zapply_app;
GRANT EXECUTE ON FUNCTION platform.selected_decision_model(),platform.cancel_operation(uuid),platform.operation_costs(uuid[]) TO zapply_app,zapply_worker;
GRANT EXECUTE ON FUNCTION platform.active_match_runs(int) TO zapply_worker;
REVOKE EXECUTE ON FUNCTION platform.get_model_settings(),platform.save_model_settings(text,int),platform.selected_decision_model(),platform.catalogue_key(),platform.cancel_operation(uuid),platform.index_match_run(),platform.active_match_runs(int),platform.operation_costs(uuid[]) FROM PUBLIC;
ALTER FUNCTION platform.get_model_settings() OWNER TO zapply_platform;
ALTER FUNCTION platform.save_model_settings(text,int) OWNER TO zapply_platform;
ALTER FUNCTION platform.selected_decision_model() OWNER TO zapply_platform;
ALTER FUNCTION platform.catalogue_key() OWNER TO zapply_platform;
ALTER FUNCTION platform.cancel_operation(uuid) OWNER TO zapply_platform;
ALTER FUNCTION platform.index_match_run() OWNER TO zapply_platform;
ALTER FUNCTION platform.active_match_runs(int) OWNER TO zapply_platform;
ALTER FUNCTION platform.operation_costs(uuid[]) OWNER TO zapply_platform;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER index_match_run ON match_run;
DROP POLICY platform_match_job ON job;
DROP FUNCTION platform.match_operations_dispatched(uuid[]),platform.lock_match_job(uuid),platform.decision_quote(text),platform.operation_costs(uuid[]),platform.active_match_runs(int),platform.index_match_run(),platform.cancel_operation(uuid),platform.catalogue_key(),platform.selected_decision_model(),platform.save_model_settings(text,int),platform.get_model_settings();
DROP TABLE platform.match_index,platform.model_quote,platform.model_settings;
DROP TABLE match_run_operation,match_override,match_run_question,match_run_requirement,match_run_item,match_run,decision_answer,decision_request_item,llm_request;
ALTER TABLE profile DROP COLUMN auto_match;
REVOKE INSERT ON llm_operation FROM zapply_worker;
REVOKE SELECT ON job,job_status,profile,evidence,highlight,skill,evidence_skill,highlight_skill,organization FROM zapply_worker;
ALTER POLICY tenant ON job TO zapply_app;
ALTER POLICY tenant ON job_status TO zapply_app;
REVOKE SELECT,UPDATE(id) ON job FROM zapply_platform;
ALTER TABLE llm_operation DROP CONSTRAINT llm_operation_kind_check;
-- Retain larger result budget: historical results must survive a rollback.
-- +goose StatementEnd
