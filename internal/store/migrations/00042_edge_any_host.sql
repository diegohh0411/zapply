-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform.edge_hosts_valid(p_hosts text[]) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT p_hosts IS NOT NULL AND cardinality(p_hosts)<=50 AND (NOT ('*'=ANY(p_hosts)) OR cardinality(p_hosts)=1) AND NOT EXISTS(
  SELECT FROM unnest(p_hosts) h WHERE h IS NULL OR h<>'*' AND (length(h)>253 OR h !~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$'))
$$;

-- The migration login can SET ROLE but does not inherit platform function ownership.
SET LOCAL ROLE zapply_platform;
-- An edge worker may list "*" alone to open any host. The owner still decides
-- per worker; the worker keeps refusing non-public addresses and ports other
-- than 80 and 443, and the hourly and concurrent limits still apply.
CREATE OR REPLACE FUNCTION platform.enqueue_scrape_job(p_url text,p_host text,p_ttl_seconds int) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE id uuid; BEGIN
 IF p_host IS NULL OR p_host<>lower(p_host) OR p_host !~ '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$' OR p_ttl_seconds NOT BETWEEN 5 AND 300 THEN
  RAISE EXCEPTION 'invalid scrape job' USING ERRCODE='ZA422';
 END IF;
 PERFORM platform.expire_scrape_jobs();
 IF NOT EXISTS(SELECT FROM platform.edge_worker w WHERE w.enabled AND w.last_seen_at>now()-interval '90 seconds'
   AND EXISTS(SELECT FROM unnest(w.allowed_hosts) h WHERE h='*' OR p_host=h OR right(p_host,length(h)+1)='.'||h)
   AND (SELECT count(*) FROM platform.scrape_job j WHERE j.worker_id=w.id AND j.claimed_at>now()-interval '1 hour')<w.max_per_hour) THEN
  RETURN NULL;
 END IF;
 INSERT INTO platform.scrape_job(url,host,deadline) VALUES(p_url,p_host,now()+make_interval(secs=>p_ttl_seconds)) RETURNING platform.scrape_job.id INTO id;
 RETURN id;
END $$;

CREATE OR REPLACE FUNCTION platform.claim_scrape_job(p_token_hash bytea) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE w platform.edge_worker; j platform.scrape_job; BEGIN
 w:=platform.edge_authenticate(p_token_hash);
 IF w.last_seen_at IS NULL OR w.last_seen_at<now()-interval '10 seconds' THEN
  UPDATE platform.edge_worker SET last_seen_at=now() WHERE id=w.id;
 END IF;
 IF (SELECT count(*) FROM platform.scrape_job x WHERE x.worker_id=w.id AND x.status='claimed' AND x.deadline>now())>=w.max_concurrent THEN RETURN NULL; END IF;
 IF (SELECT count(*) FROM platform.scrape_job x WHERE x.worker_id=w.id AND x.claimed_at>now()-interval '1 hour')>=w.max_per_hour THEN RETURN NULL; END IF;
 SELECT * INTO j FROM platform.scrape_job x
  WHERE x.status='queued' AND x.deadline>now()+interval '10 seconds'
   AND EXISTS(SELECT FROM unnest(w.allowed_hosts) h WHERE h='*' OR x.host=h OR right(x.host,length(h)+1)='.'||h)
  ORDER BY x.created_at,x.id LIMIT 1 FOR UPDATE SKIP LOCKED;
 IF NOT FOUND THEN RETURN NULL; END IF;
 UPDATE platform.scrape_job SET status='claimed',worker_id=w.id,claimed_at=now() WHERE id=j.id;
 RETURN jsonb_build_object('id',j.id,'url',j.url,'deadline',j.deadline);
END $$;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform.edge_hosts_valid(p_hosts text[]) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT p_hosts IS NOT NULL AND cardinality(p_hosts)<=50 AND NOT EXISTS(
  SELECT FROM unnest(p_hosts) h WHERE h IS NULL OR length(h)>253 OR h !~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$')
$$;

SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.enqueue_scrape_job(p_url text,p_host text,p_ttl_seconds int) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
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

CREATE OR REPLACE FUNCTION platform.claim_scrape_job(p_token_hash bytea) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
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
RESET ROLE;
-- +goose StatementEnd
