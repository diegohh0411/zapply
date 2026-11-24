-- +goose Up
-- +goose StatementBegin
-- Edge workers: LinkedIn blocks datacenter IPs, so a posting can also be read
-- by a worker on a residential machine. The worker pulls scrape jobs from the
-- API over HTTPS (outbound only), renders the page in an isolated headless
-- Chrome and posts the page back. The queue lives here, not in memory, because
-- the API may run as several replicas.
--
-- A worker authenticates with a bearer token of which only the SHA-256 is
-- stored. It serves any user's scrapes, so the owner limits it: the hosts it
-- may open (a host and its subdomains; none by default), jobs per hour and
-- jobs at once. Scrape results are kept only until the API reads them, and
-- never more than an hour.
--
-- Everything is reached through SECURITY DEFINER functions granted to the API
-- role only: the model worker and the relay do not scrape.

-- Bare lowercase hostnames, at most 50.
CREATE FUNCTION platform.edge_hosts_valid(p_hosts text[]) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT p_hosts IS NOT NULL AND cardinality(p_hosts)<=50 AND NOT EXISTS(
  SELECT FROM unnest(p_hosts) h WHERE h IS NULL OR length(h)>253 OR h !~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$')
$$;

CREATE TABLE platform.edge_worker(
 id uuid PRIMARY KEY,
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 80),
 token_hash bytea NOT NULL UNIQUE CHECK(length(token_hash)=32),
 enabled boolean NOT NULL DEFAULT true,
 allowed_hosts text[] NOT NULL DEFAULT '{}' CHECK(platform.edge_hosts_valid(allowed_hosts)),
 max_per_hour int NOT NULL DEFAULT 20 CHECK(max_per_hour BETWEEN 1 AND 600),
 max_concurrent int NOT NULL DEFAULT 1 CHECK(max_concurrent BETWEEN 1 AND 10),
 last_seen_at timestamptz,
 version int NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE platform.scrape_job(
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 url text NOT NULL CHECK(url ~* '^https?://' AND length(url)<=2048),
 host text NOT NULL CHECK(length(host) BETWEEN 1 AND 253),
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','claimed','done','failed','expired')),
 worker_id uuid REFERENCES platform.edge_worker(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 claimed_at timestamptz,
 finished_at timestamptz,
 deadline timestamptz NOT NULL,
 status_code int,
 error text CHECK(length(error)<=500),
 result_markdown text CHECK(octet_length(result_markdown)<=1048576),
 result_html text CHECK(octet_length(result_html)<=4194304),
 result_metadata jsonb CHECK(octet_length(result_metadata::text)<=65536),
 CHECK((status IN ('queued')) = (worker_id IS NULL AND claimed_at IS NULL) OR status='expired'));
-- The claim scan: oldest queued job first.
CREATE INDEX scrape_job_queued ON platform.scrape_job(created_at) WHERE status='queued';
-- Per-worker limits and the stats window.
CREATE INDEX scrape_job_worker ON platform.scrape_job(worker_id,claimed_at) WHERE worker_id IS NOT NULL;
CREATE INDEX scrape_job_finished ON platform.scrape_job(finished_at) WHERE finished_at IS NOT NULL;
GRANT SELECT,INSERT,UPDATE,DELETE ON platform.edge_worker,platform.scrape_job TO zapply_platform;

-- A saved edge provider has no address and no key: the queue is the transport.
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

-- The migration login can SET ROLE but does not inherit platform function ownership.
SET LOCAL ROLE zapply_platform;

-- Marks jobs nobody finished in time, drops results an hour after the job
-- finished (the API takes a result once, so this is only the backstop) and
-- deletes finished rows after the 24 hours the admin stats look back.
CREATE FUNCTION platform.expire_scrape_jobs() RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 UPDATE platform.scrape_job SET status='expired',finished_at=now() WHERE status IN ('queued','claimed') AND deadline<now();
 UPDATE platform.scrape_job SET result_markdown=NULL,result_html=NULL,result_metadata=NULL WHERE finished_at<now()-interval '1 hour' AND (result_markdown IS NOT NULL OR result_html IS NOT NULL OR result_metadata IS NOT NULL);
 DELETE FROM platform.scrape_job WHERE finished_at<now()-interval '25 hours';
$$;

-- Resolves a bearer token's hash to its enabled worker, locking the row so a
-- worker's claims are serialised and its limits cannot be raced.
CREATE FUNCTION platform.edge_authenticate(p_hash bytea) RETURNS platform.edge_worker LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE w platform.edge_worker; BEGIN
 SELECT * INTO w FROM platform.edge_worker WHERE token_hash=p_hash AND enabled FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'invalid edge worker token' USING ERRCODE='ZA401'; END IF;
 RETURN w;
END $$;

CREATE FUNCTION platform.edge_worker_json(w platform.edge_worker) RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('id',w.id,'name',w.name,'enabled',w.enabled,'allowed_hosts',to_jsonb(w.allowed_hosts),
  'max_per_hour',w.max_per_hour,'max_concurrent',w.max_concurrent,'last_seen_at',w.last_seen_at,'created_at',w.created_at,'version',w.version,
  'jobs_24h',(SELECT count(*) FROM platform.scrape_job j WHERE j.worker_id=w.id AND j.claimed_at>now()-interval '24 hours'),
  'done_24h',(SELECT count(*) FROM platform.scrape_job j WHERE j.worker_id=w.id AND j.claimed_at>now()-interval '24 hours' AND j.status='done'),
  'failed_24h',(SELECT count(*) FROM platform.scrape_job j WHERE j.worker_id=w.id AND j.claimed_at>now()-interval '24 hours' AND j.status IN ('failed','expired')))
$$;

CREATE FUNCTION platform.edge_workers() RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(platform.edge_worker_json(w) ORDER BY w.created_at DESC) FROM platform.edge_worker w),'[]');
END $$;

CREATE FUNCTION platform.create_edge_worker(p_id uuid,p_name text,p_token_hash bytea,p_hosts text[],p_max_per_hour int,p_max_concurrent int) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE w platform.edge_worker; BEGIN
 PERFORM platform.assert_owner();
 INSERT INTO platform.edge_worker(id,name,token_hash,allowed_hosts,max_per_hour,max_concurrent) VALUES(p_id,p_name,p_token_hash,COALESCE(p_hosts,'{}'),p_max_per_hour,p_max_concurrent) RETURNING * INTO w;
 PERFORM platform.audit_event('edge_worker.created',p_id,jsonb_build_object('name',p_name,'allowed_hosts',to_jsonb(p_hosts),'max_per_hour',p_max_per_hour,'max_concurrent',p_max_concurrent));
 RETURN platform.edge_worker_json(w);
END $$;

CREATE FUNCTION platform.update_edge_worker(p_id uuid,p_name text,p_enabled boolean,p_hosts text[],p_max_per_hour int,p_max_concurrent int,p_version int) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE w platform.edge_worker; BEGIN
 PERFORM platform.assert_owner();
 PERFORM 1 FROM platform.edge_worker WHERE id=p_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'edge worker not found' USING ERRCODE='ZA404'; END IF;
 UPDATE platform.edge_worker SET name=COALESCE(p_name,name),enabled=COALESCE(p_enabled,enabled),allowed_hosts=COALESCE(p_hosts,allowed_hosts),max_per_hour=COALESCE(p_max_per_hour,max_per_hour),max_concurrent=COALESCE(p_max_concurrent,max_concurrent),version=version+1,updated_at=now()
  WHERE id=p_id AND version=p_version RETURNING * INTO w;
 IF NOT FOUND THEN RAISE EXCEPTION 'edge worker changed' USING ERRCODE='ZA412'; END IF;
 PERFORM platform.audit_event('edge_worker.updated',p_id,jsonb_build_object('name',w.name,'enabled',w.enabled,'allowed_hosts',to_jsonb(w.allowed_hosts),'max_per_hour',w.max_per_hour,'max_concurrent',w.max_concurrent));
 RETURN platform.edge_worker_json(w);
END $$;

-- Replaces the token; the old one stops working at once.
CREATE FUNCTION platform.rotate_edge_worker_token(p_id uuid,p_token_hash bytea) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE w platform.edge_worker; BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.edge_worker SET token_hash=p_token_hash,version=version+1,updated_at=now() WHERE id=p_id RETURNING * INTO w;
 IF NOT FOUND THEN RAISE EXCEPTION 'edge worker not found' USING ERRCODE='ZA404'; END IF;
 PERFORM platform.audit_event('edge_worker.token_rotated',p_id,jsonb_build_object('name',w.name));
 RETURN platform.edge_worker_json(w);
END $$;

-- Queues a page for edge workers. It returns null, and queues nothing, when no
-- enabled worker that was seen in the last 90 seconds may open the host and
-- has hourly capacity left, so the scraper chain falls back at once instead of
-- waiting for a laptop that is asleep.
CREATE FUNCTION platform.enqueue_scrape_job(p_url text,p_host text,p_ttl_seconds int) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE id uuid; BEGIN
 IF p_host IS NULL OR p_host<>lower(p_host) OR p_host !~ '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$' OR p_ttl_seconds NOT BETWEEN 5 AND 300 THEN
  RAISE EXCEPTION 'invalid scrape job' USING ERRCODE='ZA422';
 END IF;
 PERFORM platform.expire_scrape_jobs();
 IF NOT EXISTS(SELECT FROM platform.edge_worker w WHERE w.enabled AND w.last_seen_at>now()-interval '90 seconds'
   AND EXISTS(SELECT FROM unnest(w.allowed_hosts) h WHERE p_host=h OR right(p_host,length(h)+1)='.'||h)
   AND (SELECT count(*) FROM platform.scrape_job j WHERE j.worker_id=w.id AND j.claimed_at>now()-interval '1 hour')<w.max_per_hour) THEN
  RETURN NULL;
 END IF;
 INSERT INTO platform.scrape_job(url,host,deadline) VALUES(p_url,p_host,now()+make_interval(secs=>p_ttl_seconds)) RETURNING platform.scrape_job.id INTO id;
 RETURN id;
END $$;

-- Hands the worker its next job, or null. The oldest queued job whose host the
-- worker may open goes first; SKIP LOCKED lets workers claim concurrently
-- without waiting on or double-taking a row.
CREATE FUNCTION platform.claim_scrape_job(p_token_hash bytea) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE w platform.edge_worker; j platform.scrape_job; BEGIN
 w:=platform.edge_authenticate(p_token_hash);
 IF w.last_seen_at IS NULL OR w.last_seen_at<now()-interval '10 seconds' THEN
  UPDATE platform.edge_worker SET last_seen_at=now() WHERE id=w.id;
 END IF;
 IF (SELECT count(*) FROM platform.scrape_job x WHERE x.worker_id=w.id AND x.status='claimed' AND x.deadline>now())>=w.max_concurrent THEN RETURN NULL; END IF;
 IF (SELECT count(*) FROM platform.scrape_job x WHERE x.worker_id=w.id AND x.claimed_at>now()-interval '1 hour')>=w.max_per_hour THEN RETURN NULL; END IF;
 SELECT * INTO j FROM platform.scrape_job x
  WHERE x.status='queued' AND x.deadline>now()+interval '10 seconds'
   AND EXISTS(SELECT FROM unnest(w.allowed_hosts) h WHERE x.host=h OR right(x.host,length(h)+1)='.'||h)
  ORDER BY x.created_at,x.id LIMIT 1 FOR UPDATE SKIP LOCKED;
 IF NOT FOUND THEN RETURN NULL; END IF;
 UPDATE platform.scrape_job SET status='claimed',worker_id=w.id,claimed_at=now() WHERE id=j.id;
 RETURN jsonb_build_object('id',j.id,'url',j.url,'deadline',j.deadline);
END $$;

-- Records the page a worker read. Only the worker that claimed the job can
-- finish it, and only while it is claimed and unexpired.
CREATE FUNCTION platform.finish_scrape_job(p_token_hash bytea,p_id uuid,p_status text,p_markdown text,p_html text,p_metadata jsonb,p_status_code int,p_error text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE w platform.edge_worker; BEGIN
 w:=platform.edge_authenticate(p_token_hash);
 IF p_status NOT IN ('done','failed') THEN RAISE EXCEPTION 'invalid scrape result' USING ERRCODE='ZA422'; END IF;
 UPDATE platform.edge_worker SET last_seen_at=now() WHERE id=w.id;
 UPDATE platform.scrape_job SET status=p_status,finished_at=now(),result_markdown=p_markdown,result_html=p_html,result_metadata=p_metadata,status_code=p_status_code,error=p_error
  WHERE id=p_id AND worker_id=w.id AND status='claimed' AND deadline>now();
 IF NOT FOUND THEN RAISE EXCEPTION 'scrape job not found' USING ERRCODE='ZA404'; END IF;
END $$;

-- The API's view of a job. A finished job's result is returned once and then
-- cleared. With p_cancel, a job still pending is expired, so a result that
-- arrives after the caller gave up is never kept.
CREATE FUNCTION platform.scrape_job_result(p_id uuid,p_cancel boolean DEFAULT false) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE j platform.scrape_job; res jsonb; BEGIN
 SELECT * INTO j FROM platform.scrape_job WHERE id=p_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'scrape job not found' USING ERRCODE='ZA404'; END IF;
 IF j.status IN ('queued','claimed') AND (p_cancel OR j.deadline<now()) THEN
  UPDATE platform.scrape_job SET status='expired',finished_at=now() WHERE id=p_id;
  j.status:='expired';
 END IF;
 res:=jsonb_build_object('status',j.status,'status_code',j.status_code,'error',j.error);
 IF j.status IN ('done','failed') THEN
  res:=res||jsonb_build_object('markdown',j.result_markdown,'raw_html',j.result_html,'metadata',j.result_metadata);
  UPDATE platform.scrape_job SET result_markdown=NULL,result_html=NULL,result_metadata=NULL WHERE id=p_id;
 END IF;
 RETURN res;
END $$;

REVOKE EXECUTE ON FUNCTION platform.expire_scrape_jobs(),platform.edge_authenticate(bytea),platform.edge_worker_json(platform.edge_worker),
 platform.edge_workers(),platform.create_edge_worker(uuid,text,bytea,text[],int,int),platform.update_edge_worker(uuid,text,boolean,text[],int,int,int),platform.rotate_edge_worker_token(uuid,bytea),
 platform.enqueue_scrape_job(text,text,int),platform.claim_scrape_job(bytea),platform.finish_scrape_job(bytea,uuid,text,text,text,jsonb,int,text),platform.scrape_job_result(uuid,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.edge_workers(),platform.create_edge_worker(uuid,text,bytea,text[],int,int),platform.update_edge_worker(uuid,text,boolean,text[],int,int,int),platform.rotate_edge_worker_token(uuid,bytea),
 platform.enqueue_scrape_job(text,text,int),platform.claim_scrape_job(bytea),platform.finish_scrape_job(bytea,uuid,text,text,text,jsonb,int,text),platform.scrape_job_result(uuid,boolean) TO zapply_app;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- A saved edge provider cannot be expressed by the older validator, and
-- dropping it would renumber the positions that rules use: remove it first.
DO $$ BEGIN
 IF EXISTS(SELECT FROM platform.scraper_settings, jsonb_array_elements(providers) p WHERE p->>'kind'='edge') THEN
  RAISE EXCEPTION 'remove the edge scraper providers in Administration before rolling back';
 END IF;
END $$;
SET LOCAL ROLE zapply_platform;
DROP FUNCTION platform.scrape_job_result(uuid,boolean),platform.finish_scrape_job(bytea,uuid,text,text,text,jsonb,int,text),platform.claim_scrape_job(bytea),platform.enqueue_scrape_job(text,text,int),
 platform.rotate_edge_worker_token(uuid,bytea),platform.update_edge_worker(uuid,text,boolean,text[],int,int,int),platform.create_edge_worker(uuid,text,bytea,text[],int,int),platform.edge_workers(),
 platform.edge_worker_json(platform.edge_worker),platform.edge_authenticate(bytea),platform.expire_scrape_jobs();
RESET ROLE;
DROP TABLE platform.scrape_job;
DROP TABLE platform.edge_worker;
DROP FUNCTION platform.edge_hosts_valid(text[]);
CREATE OR REPLACE FUNCTION platform.scraper_settings_valid(p_providers jsonb,p_rules jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE p jsonb; r jsonb; e jsonb; n int; k text;
BEGIN
 IF jsonb_typeof(p_providers) IS DISTINCT FROM 'array' OR jsonb_typeof(p_rules) IS DISTINCT FROM 'array' THEN RETURN false; END IF;
 n:=jsonb_array_length(p_providers);
 IF n>10 OR jsonb_array_length(p_rules)>100 THEN RETURN false; END IF;
 FOR p IN SELECT * FROM jsonb_array_elements(p_providers) LOOP
  IF jsonb_typeof(p) IS DISTINCT FROM 'object' OR COALESCE(p->>'kind','') NOT IN ('firecrawl','crawl4ai') THEN RETURN false; END IF;
  FOR k IN SELECT jsonb_object_keys(p) LOOP
   IF k NOT IN ('kind','base_url','key_id') THEN RETURN false; END IF;
  END LOOP;
  IF jsonb_typeof(p->'base_url') NOT IN ('string','null') OR jsonb_typeof(p->'key_id') NOT IN ('string','null') THEN RETURN false; END IF;
  IF (p->>'base_url') IS NOT NULL AND ((p->>'base_url') !~ '^https?://[^[:space:]]+$' OR length(p->>'base_url')>2048) THEN RETURN false; END IF;
  IF p->>'kind'='crawl4ai' AND (p->>'base_url') IS NULL THEN RETURN false; END IF;
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
-- +goose StatementEnd
