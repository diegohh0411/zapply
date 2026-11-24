-- Reading a posting's page is billed to the member who asked, like a model
-- call: each provider the chain tries is its own charge, priced at the
-- owner's credit_usd for that provider. A provider without a price (Firecrawl
-- Local, Crawl4AI, edge workers) is recorded as a free charge, which needs no
-- plan; a priced one needs an active plan with room for its most expensive
-- outcome, or the chain moves on to the next provider.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE platform.scrape_charge (
 charge_id uuid PRIMARY KEY,
 kind text NOT NULL DEFAULT 'scrape' CHECK(kind='scrape'),
 user_id uuid NOT NULL,
 provider text NOT NULL CHECK(provider IN ('firecrawl','crawl4ai','edge')),
 host text NOT NULL CHECK(length(host) BETWEEN 1 AND 253),
 credit_price bigint NOT NULL CHECK(credit_price >= 0), -- picodollars per credit
 credits integer CHECK(credits >= 0),
 FOREIGN KEY(charge_id,kind,user_id) REFERENCES platform.charge(id,kind,user_id)
);
GRANT SELECT,INSERT,UPDATE ON platform.scrape_charge TO zapply_platform;

-- Providers may carry credit_usd: USD per provider credit, as a decimal
-- string. Edge workers run on the owner's machines and cannot be priced.
CREATE OR REPLACE FUNCTION platform.scraper_settings_valid(p_providers jsonb,p_rules jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE p jsonb; r jsonb; e jsonb; n int; k text;
BEGIN
 IF jsonb_typeof(p_providers) IS DISTINCT FROM 'array' OR jsonb_typeof(p_rules) IS DISTINCT FROM 'array' THEN RETURN false; END IF;
 n:=jsonb_array_length(p_providers);
 IF n>10 OR jsonb_array_length(p_rules)>100 THEN RETURN false; END IF;
 FOR p IN SELECT * FROM jsonb_array_elements(p_providers) LOOP
  IF jsonb_typeof(p) IS DISTINCT FROM 'object' OR COALESCE(p->>'kind','') NOT IN ('firecrawl','crawl4ai','edge') THEN RETURN false; END IF;
  FOR k IN SELECT jsonb_object_keys(p) LOOP
   IF k NOT IN ('kind','base_url','key_id','credit_usd') THEN RETURN false; END IF;
  END LOOP;
  IF jsonb_typeof(p->'base_url') NOT IN ('string','null') OR jsonb_typeof(p->'key_id') NOT IN ('string','null') OR jsonb_typeof(p->'credit_usd') NOT IN ('string','null') THEN RETURN false; END IF;
  IF (p->>'base_url') IS NOT NULL AND ((p->>'base_url') !~ '^https?://[^[:space:]]+$' OR length(p->>'base_url')>2048) THEN RETURN false; END IF;
  IF p->>'kind'='crawl4ai' AND (p->>'base_url') IS NULL THEN RETURN false; END IF;
  IF p->>'kind'='edge' AND ((p->>'base_url') IS NOT NULL OR (p->>'key_id') IS NOT NULL OR (p->>'credit_usd') IS NOT NULL) THEN RETURN false; END IF;
  IF (p->>'key_id') IS NOT NULL AND (p->>'key_id') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN RETURN false; END IF;
  IF (p->>'credit_usd') IS NOT NULL AND ((p->>'credit_usd') !~ '^[0-9]{1,3}(\.[0-9]{1,12})?$') THEN RETURN false; END IF;
 END LOOP;
 FOR r IN SELECT * FROM jsonb_array_elements(p_rules) LOOP
  IF jsonb_typeof(r) IS DISTINCT FROM 'object' THEN RETURN false; END IF;
  FOR k IN SELECT jsonb_object_keys(r) LOOP
   IF k NOT IN ('host','providers') THEN RETURN false; END IF;
  END LOOP;
  IF jsonb_typeof(r->'host') IS DISTINCT FROM 'string' OR length(r->>'host')>253
   OR (r->>'host') !~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$' THEN RETURN false; END IF;
  IF jsonb_typeof(r->'providers') IS DISTINCT FROM 'array' OR jsonb_array_length(r->'providers') NOT BETWEEN 1 AND 10 THEN RETURN false; END IF;
  FOR e IN SELECT * FROM jsonb_array_elements(r->'providers') LOOP
   IF jsonb_typeof(e) IS DISTINCT FROM 'number' THEN RETURN false; END IF;
   IF e::text !~ '^[0-9]{1,2}$' THEN RETURN false; END IF;
   IF (e::text)::int>=n THEN RETURN false; END IF;
  END LOOP;
 END LOOP;
 RETURN true;
END $$;

SET LOCAL ROLE zapply_platform;

-- Opens the charge for one provider call made for the signed-in member.
-- p_cost is the most the call can cost; p_credit_price its picodollars per credit.
-- Returns '' or why the member cannot pay for it.
CREATE FUNCTION platform.open_scrape_charge(p_id uuid,p_provider text,p_host text,p_credit_price bigint,p_cost bigint,p_deadline timestamptz) RETURNS text LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE u uuid:=public.app_user_id(); reason text; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=u FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'member not found' USING ERRCODE='ZA404'; END IF;
 reason:=platform.open_charge(p_id,u,'scrape',p_cost,p_deadline);
 IF reason='' THEN
  INSERT INTO platform.scrape_charge(charge_id,user_id,provider,host,credit_price) VALUES(p_id,u,p_provider,p_host,p_credit_price);
 END IF;
 RETURN reason;
END $$;

-- Settles a scrape at the credits the provider used. A charge already
-- settled (expire_charges got there first) keeps its amount and evidence.
CREATE FUNCTION platform.close_scrape_charge(p_id uuid,p_credits integer) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE u uuid:=public.app_user_id(); s platform.scrape_charge; open boolean; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=u FOR UPDATE;
 SELECT * INTO s FROM platform.scrape_charge WHERE charge_id=p_id AND user_id=u;
 IF NOT FOUND THEN RAISE EXCEPTION 'charge not found' USING ERRCODE='ZA404'; END IF;
 IF p_credits IS NULL OR p_credits<0 THEN RAISE EXCEPTION 'invalid credits' USING ERRCODE='ZA422'; END IF;
 SELECT state='dispatching' INTO open FROM platform.charge WHERE id=p_id;
 IF NOT open THEN RETURN; END IF;
 PERFORM platform.close_charge(p_id,u,p_credits::bigint*s.credit_price,'provider_response',jsonb_build_object('credits',p_credits));
 UPDATE platform.scrape_charge SET credits=p_credits WHERE charge_id=p_id;
END $$;

-- Activity lists scrapes as posting work not linked to a job.
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
  -- Free scrapes are on the ledger, not in the activity list.
  AND NOT (c.kind='scrape' AND u.amount=0)
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
REVOKE EXECUTE ON FUNCTION platform.open_scrape_charge(uuid,text,text,bigint,bigint,timestamptz),platform.close_scrape_charge(uuid,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.open_scrape_charge(uuid,text,text,bigint,bigint,timestamptz),platform.close_scrape_charge(uuid,integer) TO zapply_app;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT FROM platform.charge WHERE kind='scrape') THEN
  RAISE EXCEPTION 'the ledger holds scrape charges, which cannot be rolled back';
 END IF;
 IF EXISTS(SELECT FROM platform.scraper_settings s, jsonb_array_elements(s.providers) p WHERE p ? 'credit_usd') THEN
  RAISE EXCEPTION 'a scraper provider has a credit price; remove it before rolling back';
 END IF;
END $$;
SET LOCAL ROLE zapply_platform;
DROP FUNCTION platform.close_scrape_charge(uuid,integer),platform.open_scrape_charge(uuid,text,text,bigint,bigint,timestamptz);
CREATE OR REPLACE FUNCTION platform.usage_entries(p_before bigint, p_limit integer) RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
WITH charges AS (
 SELECT u.id, u.amount, u.accounted_at,
  CASE WHEN mo.operation_id IS NOT NULL THEN 'mail' ELSE 'jobs' END AS app,
  CASE WHEN mo.operation_id IS NOT NULL THEN 'mail'
       WHEN o.kind='summary' THEN 'summary'
       WHEN o.kind='extraction' THEN 'extraction'
       WHEN r.id IS NOT NULL THEN 'match'
       ELSE 'other' END AS step,
  COALESCE(cs.job_id, r.job_id, ms.job_id) AS job_id
 FROM platform.ledger_entry u
 JOIN platform.model_charge a ON a.charge_id=u.charge_id
 LEFT JOIN public.model_operation o ON o.user_id=a.user_id AND o.id=a.operation_id
 LEFT JOIN public.mail_operation mo ON mo.user_id=a.user_id AND mo.operation_id=a.operation_id
 LEFT JOIN public.mail_message ms ON ms.user_id=mo.user_id AND ms.id=mo.message_id
 LEFT JOIN public.cv_summary cs ON cs.user_id=a.user_id AND cs.operation_id=a.operation_id
 LEFT JOIN public.match_run_operation mro ON mro.user_id=a.user_id AND mro.operation_id=a.operation_id
 LEFT JOIN public.match_run r ON r.user_id=mro.user_id AND r.id=mro.run_id
 WHERE u.user_id=public.app_user_id()
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
RESET ROLE;
CREATE OR REPLACE FUNCTION platform.scraper_settings_valid(p_providers jsonb,p_rules jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE p jsonb; r jsonb; e jsonb; n int; k text;
BEGIN
 IF jsonb_typeof(p_providers) IS DISTINCT FROM 'array' OR jsonb_typeof(p_rules) IS DISTINCT FROM 'array' THEN RETURN false; END IF;
 n:=jsonb_array_length(p_providers);
 IF n>10 OR jsonb_array_length(p_rules)>100 THEN RETURN false; END IF;
 FOR p IN SELECT * FROM jsonb_array_elements(p_providers) LOOP
  IF jsonb_typeof(p) IS DISTINCT FROM 'object' OR COALESCE(p->>'kind','') NOT IN ('firecrawl','crawl4ai','edge') THEN RETURN false; END IF;
  FOR k IN SELECT jsonb_object_keys(p) LOOP
   IF k NOT IN ('kind','base_url','key_id') THEN RETURN false; END IF;
  END LOOP;
  IF jsonb_typeof(p->'base_url') NOT IN ('string','null') OR jsonb_typeof(p->'key_id') NOT IN ('string','null') THEN RETURN false; END IF;
  IF (p->>'base_url') IS NOT NULL AND ((p->>'base_url') !~ '^https?://[^[:space:]]+$' OR length(p->>'base_url')>2048) THEN RETURN false; END IF;
  IF p->>'kind'='crawl4ai' AND (p->>'base_url') IS NULL THEN RETURN false; END IF;
  IF p->>'kind'='edge' AND ((p->>'base_url') IS NOT NULL OR (p->>'key_id') IS NOT NULL) THEN RETURN false; END IF;
  IF (p->>'key_id') IS NOT NULL AND (p->>'key_id') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN RETURN false; END IF;
 END LOOP;
 FOR r IN SELECT * FROM jsonb_array_elements(p_rules) LOOP
  IF jsonb_typeof(r) IS DISTINCT FROM 'object' THEN RETURN false; END IF;
  FOR k IN SELECT jsonb_object_keys(r) LOOP
   IF k NOT IN ('host','providers') THEN RETURN false; END IF;
  END LOOP;
  IF jsonb_typeof(r->'host') IS DISTINCT FROM 'string' OR length(r->>'host')>253
   OR (r->>'host') !~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$' THEN RETURN false; END IF;
  IF jsonb_typeof(r->'providers') IS DISTINCT FROM 'array' OR jsonb_array_length(r->'providers') NOT BETWEEN 1 AND 10 THEN RETURN false; END IF;
  FOR e IN SELECT * FROM jsonb_array_elements(r->'providers') LOOP
   IF jsonb_typeof(e) IS DISTINCT FROM 'number' THEN RETURN false; END IF;
   IF e::text !~ '^[0-9]{1,2}$' THEN RETURN false; END IF;
   IF (e::text)::int>=n THEN RETURN false; END IF;
  END LOOP;
 END LOOP;
 RETURN true;
END $$;
DROP TABLE platform.scrape_charge;
-- +goose StatementEnd
