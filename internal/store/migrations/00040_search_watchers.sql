-- Search watchers: a saved query (title terms, locations, country, remote)
-- polled through a job search provider (TheirStack) across every company,
-- including employers that post only on LinkedIn or local boards.
--
-- Unlike boards, searches cost provider credits, so they are an entitlement
-- of the member's plan (max_search_watchers; none without an active plan) and
-- every poll is a charge on the member's ledger. A poll asks only for postings
-- discovered since its cursor, so a quiet query costs next to nothing: the
-- provider charges per posting returned. Creating or resuming a watcher moves
-- its cursor to now, so what was already open is never fetched or announced.
--
-- job_alert_key keeps every identity of each posting a member was alerted
-- about (the ATS posting, its canonical URL, company+title+city), so the same
-- opening found by two watchers, or on a board and an aggregator, alerts once.
-- +goose Up
-- +goose StatementBegin
ALTER TABLE platform.plan ADD COLUMN max_search_watchers integer NOT NULL DEFAULT 0 CHECK(max_search_watchers BETWEEN 0 AND 100);
ALTER TABLE platform.system_key DROP CONSTRAINT system_key_kind_check,
 ADD CONSTRAINT system_key_kind_check CHECK(kind IN ('openrouter','firecrawl','crawl4ai','theirstack'));

-- The owner's search provider: its key and what one of its credits costs.
-- Search watchers are available once both are set.
CREATE TABLE platform.search_settings (
 id boolean PRIMARY KEY DEFAULT true CHECK(id),
 key_id uuid REFERENCES platform.system_key(id),
 credit_price bigint CHECK(credit_price > 0), -- picodollars per credit
 max_results integer NOT NULL DEFAULT 25 CHECK(max_results BETWEEN 1 AND 100),
 version integer NOT NULL DEFAULT 1,
 updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO platform.search_settings DEFAULT VALUES;

ALTER TABLE job_watch ADD COLUMN kind text NOT NULL DEFAULT 'board' CHECK(kind IN ('board','search')),
 ADD COLUMN country text CHECK(country ~ '^[A-Z]{2}$'),
 ADD COLUMN every_hours integer CHECK(every_hours IN (1,2,6,24)),
 ALTER COLUMN ats DROP NOT NULL, ALTER COLUMN board DROP NOT NULL;
-- A board watch names a board; a search names what to look for and how often.
ALTER TABLE job_watch ADD CONSTRAINT job_watch_shape CHECK(CASE kind
 WHEN 'board' THEN ats IS NOT NULL AND board IS NOT NULL AND every_hours IS NULL AND country IS NULL
 ELSE ats IS NULL AND board IS NULL AND every_hours IS NOT NULL AND cardinality(keywords) > 0 END);

CREATE TABLE platform.search_watch (
 watch_id uuid PRIMARY KEY, user_id uuid NOT NULL, name text NOT NULL,
 keywords text[] NOT NULL, locations text[] NOT NULL, country text, remote_only boolean NOT NULL,
 paused boolean NOT NULL, every_hours integer NOT NULL, created_at timestamptz NOT NULL,
 -- Postings discovered at or after the cursor are new.
 cursor_at timestamptz NOT NULL DEFAULT now(),
 next_poll_at timestamptz NOT NULL,
 last_ok_at timestamptz,
 failures integer NOT NULL DEFAULT 0,
 last_error text NOT NULL DEFAULT '' CHECK(length(last_error) <= 500),
 -- Why the last due poll did not run: plan, provider_unavailable, or the
 -- ledger's reason (usage_limit, membership_suspended, ...).
 blocked_reason text NOT NULL DEFAULT ''
);
CREATE INDEX search_watch_due ON platform.search_watch(next_poll_at) WHERE NOT paused;
CREATE INDEX search_watch_user ON platform.search_watch(user_id,created_at);

CREATE TABLE platform.search_charge (
 charge_id uuid PRIMARY KEY,
 kind text NOT NULL DEFAULT 'search' CHECK(kind='search'),
 user_id uuid NOT NULL,
 -- The watch may be deleted; the charge keeps what it was called.
 watch_id uuid NOT NULL,
 name text NOT NULL,
 provider text NOT NULL CHECK(provider IN ('theirstack')),
 credit_price bigint NOT NULL CHECK(credit_price >= 0),
 credits integer CHECK(credits >= 0),
 results integer CHECK(results >= 0),
 FOREIGN KEY(charge_id,kind,user_id) REFERENCES platform.charge(id,kind,user_id)
);
CREATE INDEX search_charge_watch ON platform.search_charge(user_id,watch_id);
GRANT SELECT,INSERT,UPDATE,DELETE ON platform.search_settings,platform.search_watch,platform.search_charge TO zapply_platform;

CREATE TABLE job_alert_key (
 user_id uuid NOT NULL,
 -- ats:<ats>:<board>:<posting>, url:<normalized URL> or fp:<company|title|city>.
 key text NOT NULL CHECK(length(key) BETWEEN 1 AND 2100),
 alert_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,key),
 FOREIGN KEY(user_id,alert_id) REFERENCES job_alert(user_id,id) ON DELETE CASCADE
);
CREATE INDEX job_alert_key_alert ON job_alert_key(user_id,alert_id);
ALTER TABLE job_alert_key ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_alert_key FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON job_alert_key TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());
REVOKE ALL ON job_alert_key FROM PUBLIC,zapply_app;
GRANT SELECT,INSERT,UPDATE ON job_alert_key TO zapply_worker;
-- Search alerts carry the provider's posting id and the posting's company.
ALTER TABLE job_alert DROP CONSTRAINT job_alert_posting_key_check,
 ADD CONSTRAINT job_alert_posting_key_check CHECK(length(posting_key) BETWEEN 1 AND 300);

SET LOCAL ROLE zapply_platform;

CREATE OR REPLACE FUNCTION platform.plan_json(p platform.plan) RETURNS jsonb LANGUAGE sql STABLE
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT jsonb_build_object('id',p.id,'name',p.name,'cap_7d_usd',platform.money(p.cap_7d),'cap_30d_usd',platform.money(p.cap_30d),
  'max_search_watchers',p.max_search_watchers,'version',p.version)
$function$;

DROP FUNCTION platform.save_plan(uuid,text,bigint,bigint,integer);
CREATE FUNCTION platform.save_plan(p_id uuid,p_name text,p_week bigint,p_month bigint,p_max_search integer,p_version integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ DECLARE p platform.plan; u uuid; BEGIN
 PERFORM platform.assert_owner();
 IF p_version = 0 THEN
   INSERT INTO platform.plan(id,name,cap_7d,cap_30d,max_search_watchers) VALUES(p_id,p_name,p_week,p_month,COALESCE(p_max_search,0)) RETURNING * INTO p;
 ELSE
   UPDATE platform.plan SET name=p_name,cap_7d=p_week,cap_30d=p_month,max_search_watchers=COALESCE(p_max_search,max_search_watchers),version=version+1,updated_at=now()
   WHERE id=p_id AND version=p_version RETURNING * INTO p;
   IF NOT FOUND THEN RAISE EXCEPTION 'plan changed or missing' USING ERRCODE='ZA412'; END IF;
 END IF;
 PERFORM platform.audit_event('plan.saved',p_id,platform.plan_json(p));
 FOR u IN SELECT user_id FROM platform.membership WHERE plan_id=p_id LOOP
   PERFORM platform.emit(u,'membership.changed',u,p.version);
 END LOOP;
 RETURN platform.plan_json(p);
END $function$;

-- p_max_search NULL keeps a plan's search watchers (none for a new plan).

-- How many search watchers a member's plan allows: none without an active plan.
CREATE FUNCTION platform.search_allowance(p_user uuid) RETURNS integer LANGUAGE sql STABLE
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT COALESCE((SELECT p.max_search_watchers FROM platform.membership m JOIN platform.plan p ON p.id=m.plan_id
  WHERE m.user_id=p_user AND m.status='active'),0) $$;

CREATE OR REPLACE FUNCTION platform.index_job_watch() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
DECLARE allowed integer; used integer; BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM platform.job_watch_index WHERE watch_id=OLD.id;
  DELETE FROM platform.search_watch WHERE watch_id=OLD.id;
  RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' AND NEW.kind<>OLD.kind THEN RAISE EXCEPTION 'a watch keeps its kind' USING ERRCODE='ZA422'; END IF;
 IF NEW.kind='search' THEN
  IF TG_OP='INSERT' THEN
   -- Serialized per member, so two creations cannot both take the last slot.
   PERFORM 1 FROM platform.budget_account WHERE user_id=NEW.user_id FOR UPDATE;
   allowed:=platform.search_allowance(NEW.user_id);
   SELECT count(*) INTO used FROM platform.search_watch WHERE user_id=NEW.user_id;
   IF used>=allowed THEN
    RAISE EXCEPTION 'your plan allows % search watchers', allowed USING ERRCODE='ZA403';
   END IF;
   INSERT INTO platform.search_watch(watch_id,user_id,name,keywords,locations,country,remote_only,paused,every_hours,created_at,cursor_at,next_poll_at)
    VALUES(NEW.id,NEW.user_id,NEW.company,NEW.keywords,NEW.locations,NEW.country,NEW.remote_only,NEW.paused,NEW.every_hours,NEW.created_at,
     now(),now()+make_interval(hours=>NEW.every_hours));
  ELSE
   UPDATE platform.search_watch SET name=NEW.company,keywords=NEW.keywords,locations=NEW.locations,country=NEW.country,
    remote_only=NEW.remote_only,paused=NEW.paused,every_hours=NEW.every_hours,
    -- Resuming skips what was discovered while paused.
    cursor_at=CASE WHEN OLD.paused AND NOT NEW.paused THEN now() ELSE cursor_at END,
    next_poll_at=CASE WHEN OLD.paused AND NOT NEW.paused THEN now()+make_interval(hours=>NEW.every_hours)
     ELSE least(next_poll_at,now()+make_interval(hours=>NEW.every_hours)) END,
    blocked_reason=CASE WHEN NEW.paused THEN '' ELSE blocked_reason END
   WHERE watch_id=NEW.id;
  END IF;
  RETURN NEW;
 END IF;
 INSERT INTO platform.ats_board(ats,board) VALUES(NEW.ats,NEW.board) ON CONFLICT DO NOTHING;
 INSERT INTO platform.job_watch_index(watch_id,user_id,ats,board,company,keywords,locations,remote_only,paused,created_at)
  VALUES(NEW.id,NEW.user_id,NEW.ats,NEW.board,NEW.company,NEW.keywords,NEW.locations,NEW.remote_only,NEW.paused,NEW.created_at)
  ON CONFLICT(watch_id) DO UPDATE SET company=EXCLUDED.company,keywords=EXCLUDED.keywords,locations=EXCLUDED.locations,
   remote_only=EXCLUDED.remote_only,paused=EXCLUDED.paused;
 RETURN NEW; END $function$;

-- Hands out the search watchers whose poll is due, moving each one's next poll
-- forward. Those beyond their member's plan are marked and wait instead.
CREATE FUNCTION platform.search_watches_due(p_limit integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE out jsonb; BEGIN
 WITH ranked AS (
  SELECT s.watch_id,row_number() OVER (PARTITION BY s.user_id ORDER BY s.created_at,s.watch_id) AS rank,platform.search_allowance(s.user_id) AS allowed
  FROM platform.search_watch s WHERE s.user_id IN (SELECT user_id FROM platform.search_watch WHERE next_poll_at<=now() AND NOT paused))
 UPDATE platform.search_watch s SET blocked_reason='plan',next_poll_at=now()+make_interval(hours=>s.every_hours)
 FROM ranked r WHERE r.watch_id=s.watch_id AND r.rank>r.allowed AND NOT s.paused AND s.next_poll_at<=now();
 WITH picked AS (
  UPDATE platform.search_watch s SET next_poll_at=now()+make_interval(hours=>s.every_hours)
  WHERE s.watch_id IN (SELECT watch_id FROM platform.search_watch d WHERE d.next_poll_at<=now() AND NOT d.paused
   ORDER BY d.next_poll_at LIMIT least(greatest(p_limit,1),500) FOR UPDATE SKIP LOCKED)
  RETURNING s.*)
 SELECT COALESCE(jsonb_agg(jsonb_build_object('watch_id',watch_id,'user_id',user_id,'name',name,'keywords',keywords,'locations',locations,
  'country',country,'remote_only',remote_only,'cursor_at',cursor_at)),'[]') INTO out FROM picked;
 RETURN out;
END $$;

-- The search provider's key and prices, for the worker. No row when search
-- watchers are not configured.
CREATE FUNCTION platform.search_config() RETURNS TABLE(key_id uuid,ciphertext bytea,encryption_version text,credit_price bigint,max_results integer)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT k.id,k.ciphertext,k.encryption_version,s.credit_price,s.max_results FROM platform.search_settings s
 JOIN platform.system_key k ON k.id=s.key_id AND k.kind='theirstack' WHERE s.credit_price IS NOT NULL $$;

-- Opens the charge for one poll of the signed-in member's watcher.
CREATE FUNCTION platform.open_search_charge(p_id uuid,p_watch uuid,p_credit_price bigint,p_cost bigint,p_deadline timestamptz) RETURNS text LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE u uuid:=public.app_user_id(); w platform.search_watch; reason text; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=u FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'member not found' USING ERRCODE='ZA404'; END IF;
 SELECT * INTO w FROM platform.search_watch WHERE watch_id=p_watch AND user_id=u;
 IF NOT FOUND THEN RETURN 'watch_not_found'; END IF;
 IF p_cost<=0 THEN RAISE EXCEPTION 'a search is never free' USING ERRCODE='ZA422'; END IF;
 reason:=platform.open_charge(p_id,u,'search',p_cost,p_deadline);
 IF reason='' THEN
  INSERT INTO platform.search_charge(charge_id,user_id,watch_id,name,provider,credit_price) VALUES(p_id,u,p_watch,w.name,'theirstack',p_credit_price);
 END IF;
 RETURN reason;
END $$;

-- Settles a poll at the credits the provider charged for the postings it returned.
CREATE FUNCTION platform.close_search_charge(p_id uuid,p_credits integer,p_results integer) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE u uuid:=public.app_user_id(); s platform.search_charge; BEGIN
 SELECT * INTO s FROM platform.search_charge WHERE charge_id=p_id AND user_id=u;
 IF NOT FOUND THEN RAISE EXCEPTION 'charge not found' USING ERRCODE='ZA404'; END IF;
 IF p_credits IS NULL OR p_credits<0 OR p_results IS NULL OR p_results<0 THEN RAISE EXCEPTION 'invalid credits' USING ERRCODE='ZA422'; END IF;
 PERFORM platform.close_charge(p_id,u,p_credits::bigint*s.credit_price,'provider_response',jsonb_build_object('credits',p_credits,'results',p_results));
 UPDATE platform.search_charge SET credits=p_credits,results=p_results WHERE charge_id=p_id AND credits IS NULL;
END $$;

-- Records one poll: p_blocked when it did not run, p_error when the provider
-- failed (backing off, up to a day), else the cursor it reached.
CREATE FUNCTION platform.record_search_poll(p_watch uuid,p_cursor timestamptz,p_error text,p_blocked text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 IF p_blocked IS NOT NULL THEN
  UPDATE platform.search_watch SET blocked_reason=left(p_blocked,100) WHERE watch_id=p_watch;
 ELSIF p_error IS NOT NULL THEN
  UPDATE platform.search_watch SET failures=failures+1,last_error=left(p_error,500),blocked_reason='',
   next_poll_at=now()+least(make_interval(hours=>every_hours)*power(2,least(failures,6)),interval '1 day')
  WHERE watch_id=p_watch;
 ELSE
  UPDATE platform.search_watch SET cursor_at=greatest(cursor_at,COALESCE(p_cursor,cursor_at)),last_ok_at=now(),failures=0,last_error='',blocked_reason=''
  WHERE watch_id=p_watch;
 END IF;
END $$;

-- The signed-in member's search watchers: their allowance, the provider's
-- prices, and how each watcher is doing.
CREATE FUNCTION platform.my_search_watches() RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object(
  'allowed',platform.search_allowance(public.app_user_id()),
  'used',(SELECT count(*) FROM platform.search_watch WHERE user_id=public.app_user_id()),
  'available',EXISTS(SELECT FROM platform.search_config()),
  'credit_usd',(SELECT platform.money(credit_price) FROM platform.search_settings WHERE credit_price IS NOT NULL),
  'max_results',(SELECT max_results FROM platform.search_settings),
  'watches',COALESCE((SELECT jsonb_agg(jsonb_build_object('watch_id',s.watch_id,'last_ok_at',s.last_ok_at,'last_error',s.last_error,
    'blocked_reason',s.blocked_reason,'next_poll_at',s.next_poll_at,
    'spent_30d_usd',platform.money((SELECT COALESCE(sum(e.amount),0)::bigint FROM platform.search_charge c JOIN platform.ledger_entry e ON e.charge_id=c.charge_id
      WHERE c.user_id=s.user_id AND c.watch_id=s.watch_id AND e.accounted_at>now()-interval '30 days'))))
   FROM platform.search_watch s WHERE s.user_id=public.app_user_id()),'[]'::jsonb)) $$;

CREATE FUNCTION platform.search_settings_json() RETURNS jsonb LANGUAGE sql STABLE
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('provider','theirstack','key_id',key_id,'credit_usd',CASE WHEN credit_price IS NULL THEN NULL ELSE platform.money(credit_price) END,
  'max_results',max_results,'version',version,'updated_at',updated_at) FROM platform.search_settings $$;

CREATE FUNCTION platform.search_settings() RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN platform.search_settings_json();
END $$;

CREATE FUNCTION platform.save_search_settings(p_key_id uuid,p_credit_price bigint,p_max_results integer,p_version integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 IF p_key_id IS NOT NULL AND NOT EXISTS(SELECT FROM platform.system_key WHERE id=p_key_id AND kind='theirstack') THEN
  RAISE EXCEPTION 'choose a TheirStack key' USING ERRCODE='ZA422';
 END IF;
 UPDATE platform.search_settings SET key_id=p_key_id,credit_price=p_credit_price,max_results=p_max_results,version=version+1,updated_at=now()
 WHERE id AND version=p_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'search settings changed' USING ERRCODE='ZA412'; END IF;
 PERFORM platform.audit_event('search_settings.saved','00000000-0000-0000-0000-000000000000',platform.search_settings_json());
 RETURN platform.search_settings_json();
END $$;

CREATE OR REPLACE FUNCTION platform.usage_entries(p_before bigint, p_limit integer) RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
WITH charges AS (
 SELECT u.id, u.amount, u.accounted_at,
  CASE WHEN c.kind='search' THEN 'alerts' WHEN mo.operation_id IS NOT NULL THEN 'mail' ELSE 'jobs' END AS app,
  CASE WHEN c.kind='search' THEN 'search'
       WHEN c.kind='scrape' THEN 'scrape'
       WHEN mo.operation_id IS NOT NULL THEN 'mail'
       WHEN o.kind='summary' THEN 'summary'
       WHEN o.kind='extraction' THEN 'extraction'
       WHEN r.id IS NOT NULL THEN 'match'
       ELSE 'other' END AS step,
  COALESCE(cs.job_id, r.job_id, ms.job_id) AS job_id,
  sc.watch_id, sc.name AS watch_name
 FROM platform.ledger_entry u
 JOIN platform.charge c ON c.id=u.charge_id
 LEFT JOIN platform.model_charge a ON a.charge_id=u.charge_id
 LEFT JOIN platform.search_charge sc ON sc.charge_id=u.charge_id
 LEFT JOIN public.model_operation o ON o.user_id=a.user_id AND o.id=a.operation_id
 LEFT JOIN public.mail_operation mo ON mo.user_id=a.user_id AND mo.operation_id=a.operation_id
 LEFT JOIN public.mail_message ms ON ms.user_id=mo.user_id AND ms.id=mo.message_id
 LEFT JOIN public.cv_summary cs ON cs.user_id=a.user_id AND cs.operation_id=a.operation_id
 LEFT JOIN public.match_run_operation mro ON mro.user_id=a.user_id AND mro.operation_id=a.operation_id
 LEFT JOIN public.match_run r ON r.user_id=mro.user_id AND r.id=mro.run_id
 WHERE u.user_id=public.app_user_id()
),
groups AS (
 SELECT app, job_id, watch_id, max(id) AS last_id, max(accounted_at) AS last_at, sum(amount)::bigint AS total,
  array_agg(DISTINCT step ORDER BY step) AS steps, (array_agg(watch_name ORDER BY id DESC))[1] AS watch_name
 FROM charges GROUP BY app, job_id, watch_id
 HAVING p_before IS NULL OR max(id) < p_before
 ORDER BY max(id) DESC
 LIMIT least(greatest(p_limit,1),100)
)
SELECT COALESCE(jsonb_agg(jsonb_build_object('id',g.last_id::text,'amount_usd',platform.money(g.total),'accounted_at',g.last_at,
 'app',g.app,'title',COALESCE(j.title,g.watch_name),'company',NULLIF(j.company,''),'steps',g.steps) ORDER BY g.last_id DESC),'[]')
FROM groups g LEFT JOIN public.job j ON j.user_id=public.app_user_id() AND j.id=g.job_id
$function$;
REVOKE EXECUTE ON FUNCTION platform.save_plan(uuid,text,bigint,bigint,integer,integer),platform.search_allowance(uuid),
 platform.search_watches_due(integer),platform.search_config(),platform.open_search_charge(uuid,uuid,bigint,bigint,timestamptz),
 platform.close_search_charge(uuid,integer,integer),platform.record_search_poll(uuid,timestamptz,text,text),platform.my_search_watches(),
 platform.search_settings_json(),platform.search_settings(),platform.save_search_settings(uuid,bigint,integer,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.save_plan(uuid,text,bigint,bigint,integer,integer),platform.my_search_watches(),
 platform.search_settings(),platform.save_search_settings(uuid,bigint,integer,integer) TO zapply_app;
GRANT EXECUTE ON FUNCTION platform.search_watches_due(integer),platform.search_config(),platform.open_search_charge(uuid,uuid,bigint,bigint,timestamptz),
 platform.close_search_charge(uuid,integer,integer),platform.record_search_poll(uuid,timestamptz,text,text) TO zapply_worker;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT FROM platform.charge WHERE kind='search') THEN
  RAISE EXCEPTION 'the ledger holds search charges, which cannot be rolled back';
 END IF;
 IF EXISTS(SELECT FROM public.job_watch WHERE kind='search') THEN
  RAISE EXCEPTION 'members have search watchers; delete them before rolling back';
 END IF;
 IF EXISTS(SELECT FROM platform.system_key WHERE kind='theirstack') THEN
  RAISE EXCEPTION 'a TheirStack key is stored; delete it before rolling back';
 END IF;
END $$;
SET LOCAL ROLE zapply_platform;
DROP FUNCTION platform.save_search_settings(uuid,bigint,integer,integer),platform.search_settings(),platform.search_settings_json(),
 platform.my_search_watches(),platform.record_search_poll(uuid,timestamptz,text,text),platform.close_search_charge(uuid,integer,integer),
 platform.open_search_charge(uuid,uuid,bigint,bigint,timestamptz),platform.search_config(),platform.search_watches_due(integer);
CREATE OR REPLACE FUNCTION platform.index_job_watch() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM platform.job_watch_index WHERE watch_id=OLD.id;
  RETURN OLD;
 END IF;
 INSERT INTO platform.ats_board(ats,board) VALUES(NEW.ats,NEW.board) ON CONFLICT DO NOTHING;
 INSERT INTO platform.job_watch_index(watch_id,user_id,ats,board,company,keywords,locations,remote_only,paused,created_at)
  VALUES(NEW.id,NEW.user_id,NEW.ats,NEW.board,NEW.company,NEW.keywords,NEW.locations,NEW.remote_only,NEW.paused,NEW.created_at)
  ON CONFLICT(watch_id) DO UPDATE SET company=EXCLUDED.company,keywords=EXCLUDED.keywords,locations=EXCLUDED.locations,
   remote_only=EXCLUDED.remote_only,paused=EXCLUDED.paused;
 RETURN NEW; END $function$;
DROP FUNCTION platform.search_allowance(uuid);
DROP FUNCTION platform.save_plan(uuid,text,bigint,bigint,integer,integer);
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
CREATE OR REPLACE FUNCTION platform.plan_json(p platform.plan) RETURNS jsonb LANGUAGE sql STABLE
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT jsonb_build_object('id',p.id,'name',p.name,'cap_7d_usd',platform.money(p.cap_7d),'cap_30d_usd',platform.money(p.cap_30d),'version',p.version)
$function$;
CREATE OR REPLACE FUNCTION platform.usage_entries(p_before bigint, p_limit integer) RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
WITH charges AS (
 SELECT u.id, u.amount, u.accounted_at,
  CASE WHEN mo.operation_id IS NOT NULL THEN 'mail' ELSE 'jobs' END AS app,
  CASE WHEN c.kind='scrape' THEN 'scrape'
       WHEN mo.operation_id IS NOT NULL THEN 'mail'
       WHEN o.kind='summary' THEN 'summary'
       WHEN o.kind='extraction' THEN 'extraction'
       WHEN r.id IS NOT NULL THEN 'match'
       ELSE 'other' END AS step,
  COALESCE(cs.job_id, r.job_id, ms.job_id) AS job_id
 FROM platform.ledger_entry u
 JOIN platform.charge c ON c.id=u.charge_id
 LEFT JOIN platform.model_charge a ON a.charge_id=u.charge_id
 LEFT JOIN public.model_operation o ON o.user_id=a.user_id AND o.id=a.operation_id
 LEFT JOIN public.mail_operation mo ON mo.user_id=a.user_id AND mo.operation_id=a.operation_id
 LEFT JOIN public.mail_message ms ON ms.user_id=mo.user_id AND ms.id=mo.message_id
 LEFT JOIN public.cv_summary cs ON cs.user_id=a.user_id AND cs.operation_id=a.operation_id
 LEFT JOIN public.match_run_operation mro ON mro.user_id=a.user_id AND mro.operation_id=a.operation_id
 LEFT JOIN public.match_run r ON r.user_id=mro.user_id AND r.id=mro.run_id
 WHERE u.user_id=public.app_user_id() AND c.kind IN ('model','scrape')
),
groups AS (
 SELECT app, job_id, max(id) AS last_id, max(accounted_at) AS last_at, sum(amount)::bigint AS total,
  array_agg(DISTINCT step ORDER BY step) AS steps
 FROM charges GROUP BY app, job_id
 HAVING p_before IS NULL OR max(id) < p_before
 ORDER BY max(id) DESC
 LIMIT least(greatest(p_limit,1),100)
)
SELECT COALESCE(jsonb_agg(jsonb_build_object('id',g.last_id::text,'amount_usd',platform.money(g.total),'accounted_at',g.last_at,
 'app',g.app,'title',j.title,'company',NULLIF(j.company,''),'steps',g.steps) ORDER BY g.last_id DESC),'[]')
FROM groups g LEFT JOIN public.job j ON j.user_id=public.app_user_id() AND j.id=g.job_id
$function$;
REVOKE EXECUTE ON FUNCTION platform.save_plan(uuid,text,bigint,bigint,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.save_plan(uuid,text,bigint,bigint,integer) TO zapply_app;
RESET ROLE;
ALTER TABLE job_alert DROP CONSTRAINT job_alert_posting_key_check,
 ADD CONSTRAINT job_alert_posting_key_check CHECK(length(posting_key) BETWEEN 1 AND 200);
DROP TABLE job_alert_key;
DROP TABLE platform.search_charge,platform.search_watch;
ALTER TABLE job_watch DROP CONSTRAINT job_watch_shape;
ALTER TABLE job_watch ALTER COLUMN ats SET NOT NULL, ALTER COLUMN board SET NOT NULL,
 DROP COLUMN every_hours, DROP COLUMN country, DROP COLUMN kind;
DROP TABLE platform.search_settings;
ALTER TABLE platform.system_key DROP CONSTRAINT system_key_kind_check,
 ADD CONSTRAINT system_key_kind_check CHECK(kind IN ('openrouter','firecrawl','crawl4ai'));
ALTER TABLE platform.plan DROP COLUMN max_search_watchers;
-- +goose StatementEnd
