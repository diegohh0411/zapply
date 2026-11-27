-- Job alerts by preferences (docs/parts/13-alerts-by-preferences.md). Members
-- no longer watch boards or searches: suppliers add postings to the catalog
-- for the whole platform, and each labeled posting becomes an alert for every
-- member whose job preferences it fits. An alert points at its catalog posting,
-- once per member.
--
-- Watches, the board poller and job_alert_key go. Search charges stay: they
-- are members' usage history.

-- +goose Up
-- +goose StatementBegin
-- notification forces row security, and the migration login is no tenant.
ALTER TABLE notification NO FORCE ROW LEVEL SECURITY;
DELETE FROM notification WHERE kind='job_alert';
ALTER TABLE notification FORCE ROW LEVEL SECURITY;
ALTER TABLE notification DROP CONSTRAINT notification_job_alert_fk;
DROP TRIGGER notify_job_alert ON job_alert;
DROP TABLE job_alert_key, job_alert;
DROP TRIGGER index_job_watch ON job_watch;
DROP TABLE job_watch;

SET LOCAL ROLE zapply_platform;
DROP FUNCTION platform.index_job_watch(), platform.my_ats_boards(), platform.ats_boards_due(int,interval),
 platform.record_ats_poll(text,text,jsonb,text,interval), platform.ats_postings_delivered(text,text,text[]),
 platform.ats_postings_uncataloged(text,text,int), platform.ats_postings_cataloged(text,text,text[]),
 platform.search_watches_due(integer), platform.open_search_charge(uuid,uuid,bigint,bigint,timestamptz),
 platform.close_search_charge(uuid,integer,integer), platform.record_search_poll(uuid,timestamptz,text,text),
 platform.my_search_watches(), platform.watching_members(uuid,int);
RESET ROLE;
DROP TABLE platform.job_watch_index, platform.ats_posting, platform.ats_board, platform.search_watch, platform.posting_backfill;

CREATE TABLE job_alert (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
 -- The catalog posting (platform.posting); no foreign key across the boundary.
 posting_id uuid NOT NULL,
 company text NOT NULL CHECK(length(company) <= 200),
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 500),
 location text NOT NULL DEFAULT '' CHECK(length(location) <= 500),
 url text NOT NULL CHECK(length(url) <= 2048 AND url ~ '^https://'),
 remote boolean NOT NULL DEFAULT false,
 published_at timestamptz,
 found_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,id), UNIQUE(user_id,posting_id)
);
ALTER TABLE job_alert ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_alert FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON job_alert TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());
REVOKE ALL ON job_alert FROM PUBLIC,zapply_app;
GRANT SELECT ON job_alert TO zapply_app;
GRANT SELECT,INSERT ON job_alert TO zapply_worker;
ALTER TABLE notification ADD CONSTRAINT notification_job_alert_fk FOREIGN KEY(user_id,job_alert_id) REFERENCES job_alert(user_id,id) ON DELETE CASCADE;
CREATE TRIGGER notify_job_alert AFTER INSERT ON job_alert FOR EACH ROW EXECUTE FUNCTION public.notify_job_alert();

-- Postings wait to be routed to members. Everything already in the catalog
-- counts as routed: members hear only about what suppliers find from now on.
ALTER TABLE platform.posting ADD COLUMN routed_at timestamptz;
UPDATE platform.posting SET routed_at=now();
CREATE INDEX posting_unrouted ON platform.posting(first_seen_at) WHERE routed_at IS NULL;
GRANT UPDATE(routed_at) ON platform.posting TO zapply_worker;

-- Each supplier's progress: where its next run starts and how the last went.
-- A run reads the discovery window [cursor_at, window_end] page by page;
-- the window is fixed while it has pages left, so later discoveries cannot
-- shift them. running_until is the lease of the run in progress.
CREATE TABLE platform.supplier (
 name text PRIMARY KEY CHECK(name ~ '^[a-z0-9_:]{1,100}$'),
 cursor_at timestamptz NOT NULL DEFAULT now(),
 window_end timestamptz,
 page integer NOT NULL DEFAULT 0 CHECK(page >= 0),
 running_until timestamptz,
 next_run_at timestamptz NOT NULL DEFAULT now(),
 last_ok_at timestamptz,
 failures integer NOT NULL DEFAULT 0,
 last_error text NOT NULL DEFAULT '' CHECK(length(last_error) <= 500),
 credits_total bigint NOT NULL DEFAULT 0
);
GRANT SELECT,INSERT,UPDATE ON platform.supplier TO zapply_worker;
GRANT SELECT,INSERT,UPDATE,DELETE ON platform.supplier TO zapply_platform;

SET LOCAL ROLE zapply_platform;
-- Who alerts go to: every active member with preferences, except the catalog's
-- own member, with what they want.
CREATE FUNCTION platform.alert_audience() RETURNS TABLE(user_id uuid,seniority_min text,seniority_max text,areas text[],countries text[],
 remote_ok boolean,excluded_employment text[]) LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT p.user_id,p.seniority_min,p.seniority_max,p.areas,p.countries,p.remote_ok,p.excluded_employment
 FROM public.job_preference p JOIN platform.membership m ON m.user_id=p.user_id AND m.status='active'
 WHERE p.user_id<>'00000000-0000-7000-8000-0000000ca7a1' $$;
-- What suppliers should ask for: every country an active member accepts, and
-- whether anyone takes remote work from anywhere.
CREATE FUNCTION platform.supply_scope() RETURNS TABLE(countries text[],remote boolean) LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT COALESCE((SELECT array_agg(DISTINCT c ORDER BY c) FROM platform.alert_audience() a, unnest(a.countries) c),'{}'),
  EXISTS(SELECT FROM platform.alert_audience() a WHERE a.remote_ok) $$;
-- The platform pays for searches, so plans no longer allow search watchers
-- and a TheirStack credit has no member price.
CREATE OR REPLACE FUNCTION platform.plan_json(p platform.plan) RETURNS jsonb LANGUAGE sql STABLE
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT jsonb_build_object('id',p.id,'name',p.name,'cap_7d_usd',platform.money(p.cap_7d),'cap_30d_usd',platform.money(p.cap_30d),'version',p.version)
$function$;
DROP FUNCTION platform.save_plan(uuid,text,bigint,bigint,integer,integer), platform.search_allowance(uuid);
CREATE FUNCTION platform.save_plan(p_id uuid, p_name text, p_week bigint, p_month bigint, p_version integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ DECLARE p platform.plan; u uuid; BEGIN
 PERFORM platform.assert_owner();
 IF p_version = 0 THEN
   INSERT INTO platform.plan(id,name,cap_7d,cap_30d) VALUES(p_id,p_name,p_week,p_month) RETURNING * INTO p;
 ELSE
   UPDATE platform.plan SET name=p_name,cap_7d=p_week,cap_30d=p_month,version=version+1,updated_at=now()
   WHERE id=p_id AND version=p_version RETURNING * INTO p;
   IF NOT FOUND THEN RAISE EXCEPTION 'plan changed or missing' USING ERRCODE='ZA412'; END IF;
 END IF;
 PERFORM platform.audit_event('plan.saved',p_id,platform.plan_json(p));
 FOR u IN SELECT user_id FROM platform.membership WHERE plan_id=p_id LOOP
   PERFORM platform.emit(u,'membership.changed',u,p.version);
 END LOOP;
 RETURN platform.plan_json(p);
END $function$;
DROP FUNCTION platform.search_config(), platform.save_search_settings(uuid,bigint,integer,integer);
CREATE OR REPLACE FUNCTION platform.search_settings_json() RETURNS jsonb LANGUAGE sql STABLE
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('provider','theirstack','key_id',key_id,'max_results',max_results,'version',version,'updated_at',updated_at)
 FROM platform.search_settings $$;
CREATE FUNCTION platform.save_search_settings(p_key_id uuid,p_max_results integer,p_version integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 IF p_key_id IS NOT NULL AND NOT EXISTS(SELECT FROM platform.system_key WHERE id=p_key_id AND kind='theirstack') THEN
  RAISE EXCEPTION 'choose a TheirStack key' USING ERRCODE='ZA422';
 END IF;
 UPDATE platform.search_settings SET key_id=p_key_id,max_results=p_max_results,version=version+1,updated_at=now()
 WHERE id AND version=p_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'search settings changed' USING ERRCODE='ZA412'; END IF;
 PERFORM platform.audit_event('search_settings.saved','00000000-0000-0000-0000-000000000000',platform.search_settings_json());
 RETURN platform.search_settings_json();
END $$;
-- The provider's key and page size, for the suppliers.
CREATE FUNCTION platform.search_config() RETURNS TABLE(key_id uuid,ciphertext bytea,encryption_version text,max_results integer)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT k.id,k.ciphertext,k.encryption_version,s.max_results FROM platform.search_settings s
 JOIN platform.system_key k ON k.id=s.key_id AND k.kind='theirstack' $$;
REVOKE EXECUTE ON FUNCTION platform.alert_audience(),platform.supply_scope(),platform.save_plan(uuid,text,bigint,bigint,integer),
 platform.save_search_settings(uuid,integer,integer),platform.search_config() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.alert_audience(),platform.supply_scope(),platform.search_config() TO zapply_worker;
GRANT EXECUTE ON FUNCTION platform.save_plan(uuid,text,bigint,bigint,integer),platform.save_search_settings(uuid,integer,integer) TO zapply_app;
RESET ROLE;
ALTER TABLE platform.plan DROP COLUMN max_search_watchers;
ALTER TABLE platform.search_settings DROP COLUMN credit_price;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Watches and their alerts are gone for good; this step does not come back.
DO $$ BEGIN RAISE EXCEPTION '00048 cannot be reversed'; END $$;
-- +goose StatementEnd
