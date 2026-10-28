-- Part 10: mail inbox. A member connects a Gmail account; the worker syncs new
-- inbox messages and classifies each once. Design: docs/parts/10-mail-plan.md.
--
-- Retention is enforced here, not only in code: a message classified `other`
-- (or one that failed) cannot hold a sender, subject or snippet.
-- Refresh tokens live in platform.mail_credential, which no application role
-- can read directly.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE mail_account (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
 provider text NOT NULL CHECK(provider IN ('google')),
 address text NOT NULL CHECK(address = lower(address) AND length(address) BETWEEN 3 AND 320),
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','paused','needs_reconnect','disconnected')),
 -- Gmail's cursor for history.list; NULL until the first sync bootstraps it.
 history_id text,
 watch_expires_at timestamptz, last_synced_at timestamptz,
 -- A sync holds this lease so a push arriving mid-run cannot start a second one.
 sync_lease_until timestamptz,
 last_error text NOT NULL DEFAULT '' CHECK(length(last_error) <= 500),
 version int NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,id), UNIQUE(provider,address)
);
CREATE TABLE mail_message (
 id uuid PRIMARY KEY, user_id uuid NOT NULL, account_id uuid NOT NULL,
 provider_message_id text NOT NULL, thread_id text NOT NULL DEFAULT '',
 received_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','classified','failed')),
 -- Set when the first question is answered; the job link may follow.
 category text CHECK(category IN ('received','assessment','interview','rejection','offer','other')),
 job_id uuid,
 from_address text CHECK(length(from_address) <= 998), subject text CHECK(length(subject) <= 998), snippet text CHECK(length(snippet) <= 2000),
 classified_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,id), UNIQUE(account_id,provider_message_id),
 FOREIGN KEY(user_id,account_id) REFERENCES mail_account(user_id,id) ON DELETE CASCADE,
 FOREIGN KEY(user_id,job_id) REFERENCES job(user_id,id) ON DELETE SET NULL (job_id),
 CHECK((state='classified') = (classified_at IS NOT NULL)),
 CHECK(state <> 'classified' OR category IS NOT NULL),
 CHECK(category IS DISTINCT FROM 'other' OR (from_address IS NULL AND subject IS NULL AND snippet IS NULL)),
 CHECK(state <> 'failed' OR (from_address IS NULL AND subject IS NULL AND snippet IS NULL))
);
CREATE INDEX mail_message_inbox ON mail_message(user_id,received_at DESC,id DESC) WHERE state='classified' AND category <> 'other';
CREATE INDEX mail_message_job ON mail_message(user_id,job_id,received_at DESC) WHERE job_id IS NOT NULL;
-- One model request per question. The request body holds the message content
-- until the answer is ingested, then platform.scrub_mail_request blanks it.
CREATE TABLE mail_operation (
 user_id uuid NOT NULL, message_id uuid NOT NULL, operation_id uuid PRIMARY KEY,
 stage text NOT NULL CHECK(stage IN ('category','job')),
 ingested boolean NOT NULL DEFAULT false,
 -- true once the answer was valid and applied to the message.
 ok boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(user_id,message_id) REFERENCES mail_message(user_id,id) ON DELETE CASCADE,
 FOREIGN KEY(user_id,operation_id) REFERENCES model_operation(user_id,id)
);
CREATE INDEX mail_operation_open ON mail_operation(user_id,message_id) WHERE NOT ingested;
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['mail_account','mail_message','mail_operation'] LOOP
 EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant ON %I TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id())',t);
 END LOOP;
END $$;
-- The API reads mail and changes an account's status; only the worker writes messages.
GRANT SELECT,INSERT,UPDATE ON mail_account TO zapply_app;
GRANT SELECT,UPDATE ON mail_account TO zapply_worker;
GRANT SELECT ON mail_message TO zapply_app;
GRANT SELECT,INSERT,UPDATE ON mail_message TO zapply_worker;
GRANT SELECT,INSERT,UPDATE ON mail_operation TO zapply_worker;

-- Platform tables: cross-tenant lookups the webhook and the recovery scan need,
-- and the credential store. Only the functions below touch them.
CREATE TABLE platform.mail_credential (
 account_id uuid PRIMARY KEY REFERENCES mail_account(id) ON DELETE CASCADE, user_id uuid NOT NULL,
 ciphertext bytea NOT NULL, encryption_version text NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE platform.mail_account_index (
 account_id uuid PRIMARY KEY, user_id uuid NOT NULL, provider text NOT NULL, address text NOT NULL, status text NOT NULL,
 watch_expires_at timestamptz, last_synced_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(provider,address)
);
CREATE TABLE platform.mail_work_index (
 message_id uuid PRIMARY KEY, user_id uuid NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE platform.mail_oauth_flow (
 state_hash text PRIMARY KEY, user_id uuid NOT NULL, verifier text NOT NULL, expires_at timestamptz NOT NULL
);
GRANT SELECT,INSERT,UPDATE,DELETE ON platform.mail_credential,platform.mail_account_index,platform.mail_work_index,platform.mail_oauth_flow TO zapply_platform;

GRANT SELECT ON mail_account,mail_operation TO zapply_platform;
CREATE POLICY platform_mail_account ON mail_account FOR SELECT TO zapply_platform USING(user_id=app_user_id());
CREATE POLICY platform_mail_operation ON mail_operation FOR SELECT TO zapply_platform USING(user_id=app_user_id());
GRANT SELECT,UPDATE(body) ON model_request TO zapply_platform;
CREATE POLICY platform_scrub ON model_request TO zapply_platform USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());

CREATE FUNCTION platform.index_mail_account() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM platform.mail_account_index WHERE account_id=OLD.id;
  RETURN OLD;
 END IF;
 INSERT INTO platform.mail_account_index(account_id,user_id,provider,address,status,watch_expires_at,last_synced_at)
  VALUES(NEW.id,NEW.user_id,NEW.provider,NEW.address,NEW.status,NEW.watch_expires_at,NEW.last_synced_at)
  ON CONFLICT(account_id) DO UPDATE SET status=EXCLUDED.status,watch_expires_at=EXCLUDED.watch_expires_at,last_synced_at=EXCLUDED.last_synced_at,updated_at=now();
 RETURN NEW; END $$;
CREATE TRIGGER index_mail_account AFTER INSERT OR UPDATE OR DELETE ON mail_account FOR EACH ROW EXECUTE FUNCTION platform.index_mail_account();
CREATE FUNCTION platform.index_mail_message() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM platform.mail_work_index WHERE message_id=OLD.id;
  RETURN OLD;
 END IF;
 IF NEW.state <> 'pending' THEN
  DELETE FROM platform.mail_work_index WHERE message_id=NEW.id;
  RETURN NEW;
 END IF;
 INSERT INTO platform.mail_work_index(message_id,user_id) VALUES(NEW.id,NEW.user_id)
  ON CONFLICT(message_id) DO UPDATE SET updated_at=now();
 RETURN NEW; END $$;
CREATE TRIGGER index_mail_message AFTER INSERT OR UPDATE OR DELETE ON mail_message FOR EACH ROW EXECUTE FUNCTION platform.index_mail_message();

-- Webhook: Gmail names the mailbox by address and carries no session.
CREATE FUNCTION platform.mail_account_for_address(p_provider text,p_address text) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('user_id',user_id,'account_id',account_id,'status',status) FROM platform.mail_account_index WHERE provider=p_provider AND address=lower(p_address) $$;
-- Recovery scan: accounts whose sync or watch is due, and messages still waiting.
CREATE FUNCTION platform.mail_accounts_due(p_limit int,p_sync_before timestamptz,p_watch_before timestamptz) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT COALESCE(jsonb_agg(jsonb_build_object('user_id',user_id,'account_id',account_id,
   'sync',(last_synced_at IS NULL OR last_synced_at < p_sync_before),
   'watch',(p_watch_before IS NOT NULL AND (watch_expires_at IS NULL OR watch_expires_at < p_watch_before)))),'[]')
 FROM (SELECT * FROM platform.mail_account_index WHERE status='active'
   AND (last_synced_at IS NULL OR last_synced_at < p_sync_before
    OR (p_watch_before IS NOT NULL AND (watch_expires_at IS NULL OR watch_expires_at < p_watch_before)))
   ORDER BY updated_at LIMIT least(greatest(p_limit,1),1000)) x $$;
-- Hands out each waiting message at most once per interval: picking one bumps it.
CREATE FUNCTION platform.mail_pending_messages(p_limit int,p_older_than timestamptz) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 WITH picked AS (
  UPDATE platform.mail_work_index w SET updated_at=now()
  WHERE w.message_id IN (SELECT message_id FROM platform.mail_work_index WHERE updated_at < p_older_than ORDER BY updated_at LIMIT least(greatest(p_limit,1),1000) FOR UPDATE SKIP LOCKED)
  RETURNING w.user_id,w.message_id)
 SELECT COALESCE(jsonb_agg(jsonb_build_object('user_id',user_id,'message_id',message_id)),'[]') FROM picked $$;

CREATE FUNCTION platform.save_mail_credential(p_account uuid,p_ciphertext bytea,p_version text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 IF NOT EXISTS(SELECT FROM public.mail_account WHERE id=p_account AND user_id=public.app_user_id()) THEN
  RAISE EXCEPTION 'mail account not found' USING ERRCODE='ZA404';
 END IF;
 INSERT INTO platform.mail_credential(account_id,user_id,ciphertext,encryption_version) VALUES(p_account,public.app_user_id(),p_ciphertext,p_version)
  ON CONFLICT(account_id) DO UPDATE SET ciphertext=EXCLUDED.ciphertext,encryption_version=EXCLUDED.encryption_version,updated_at=now(); END $$;
CREATE FUNCTION platform.load_mail_credential(p_account uuid) RETURNS TABLE(ciphertext bytea,encryption_version text) LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT c.ciphertext,c.encryption_version FROM platform.mail_credential c WHERE c.account_id=p_account AND c.user_id=public.app_user_id() $$;
CREATE FUNCTION platform.delete_mail_credential(p_account uuid) RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 DELETE FROM platform.mail_credential WHERE account_id=p_account AND user_id=public.app_user_id() $$;

-- OAuth: the callback arrives without a session, so the state itself is the
-- capability. Only its hash is stored, and taking it deletes it.
CREATE FUNCTION platform.start_mail_oauth(p_state_hash text,p_verifier text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 DELETE FROM platform.mail_oauth_flow WHERE expires_at < now();
 INSERT INTO platform.mail_oauth_flow(state_hash,user_id,verifier,expires_at) VALUES(p_state_hash,public.app_user_id(),p_verifier,now()+interval '10 minutes'); END $$;
CREATE FUNCTION platform.take_mail_oauth(p_state_hash text) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 DELETE FROM platform.mail_oauth_flow WHERE state_hash=p_state_hash AND expires_at >= now() RETURNING jsonb_build_object('user_id',user_id,'verifier',verifier) $$;

-- Blanks the content of a finished mail request. Restricted to mail operations
-- of the current tenant; the model and questions stay so replays still parse.
CREATE FUNCTION platform.scrub_mail_request(p_operation uuid) RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 UPDATE public.model_request r SET body=jsonb_build_object('model',r.body->'model','state','{}'::jsonb,'questions',COALESCE(r.body->'questions','{}'::jsonb))
 WHERE r.operation_id=p_operation AND r.user_id=public.app_user_id()
  AND EXISTS(SELECT FROM public.mail_operation m WHERE m.operation_id=p_operation AND m.user_id=public.app_user_id()) $$;

DO $$ DECLARE f text; BEGIN
 FOREACH f IN ARRAY ARRAY['platform.index_mail_account()','platform.index_mail_message()',
  'platform.mail_account_for_address(text,text)','platform.mail_accounts_due(int,timestamptz,timestamptz)','platform.mail_pending_messages(int,timestamptz)',
  'platform.save_mail_credential(uuid,bytea,text)','platform.load_mail_credential(uuid)','platform.delete_mail_credential(uuid)',
  'platform.start_mail_oauth(text,text)','platform.take_mail_oauth(text)','platform.scrub_mail_request(uuid)'] LOOP
  EXECUTE format('REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC',f);
  EXECUTE format('ALTER FUNCTION %s OWNER TO zapply_platform',f);
 END LOOP;
END $$;
GRANT EXECUTE ON FUNCTION platform.mail_account_for_address(text,text),platform.start_mail_oauth(text,text),platform.take_mail_oauth(text),platform.save_mail_credential(uuid,bytea,text) TO zapply_app;
GRANT EXECUTE ON FUNCTION platform.mail_accounts_due(int,timestamptz,timestamptz),platform.mail_pending_messages(int,timestamptz),platform.load_mail_credential(uuid),platform.scrub_mail_request(uuid) TO zapply_worker;
GRANT EXECUTE ON FUNCTION platform.save_mail_credential(uuid,bytea,text),platform.delete_mail_credential(uuid) TO zapply_worker;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER index_mail_message ON mail_message;
DROP TRIGGER index_mail_account ON mail_account;
DROP FUNCTION platform.scrub_mail_request(uuid),platform.take_mail_oauth(text),platform.start_mail_oauth(text,text),platform.delete_mail_credential(uuid),
 platform.load_mail_credential(uuid),platform.save_mail_credential(uuid,bytea,text),platform.mail_pending_messages(int,timestamptz),
 platform.mail_accounts_due(int,timestamptz,timestamptz),platform.mail_account_for_address(text,text),platform.index_mail_message(),platform.index_mail_account();
DROP POLICY platform_scrub ON model_request;
REVOKE SELECT,UPDATE(body) ON model_request FROM zapply_platform;
DROP TABLE platform.mail_oauth_flow,platform.mail_work_index,platform.mail_account_index,platform.mail_credential;
DROP TABLE mail_operation,mail_message,mail_account;
-- +goose StatementEnd
