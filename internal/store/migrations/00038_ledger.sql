-- One ledger for every dollar a member spends, whatever paid for it.
--
-- A charge is one billable provider call: it holds its most expensive outcome
-- (reserved) against the member's plan while it runs, and settles into
-- append-only ledger entries; corrections are new entries, never updates. What
-- the call was is evidence in a table of its kind, keyed by the charge:
-- model_charge for model calls today, scrape_charge and search_charge next.
-- Plan caps, the usage page and the activity list read only charge and
-- ledger_entry, so a new kind of spending needs only its evidence table.
--
-- model_attempt becomes model_charge (its accounting columns move to charge)
-- and model_usage becomes ledger_entry, keeping ids, history and grants.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE platform.charge (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES platform.budget_account(user_id),
 kind text NOT NULL CHECK(kind IN ('model','scrape','search')),
 reserved bigint NOT NULL CHECK(reserved >= 0),
 state text NOT NULL CHECK(state IN ('dispatching','uncertain','settled','uncharged')),
 dispatched_at timestamptz NOT NULL,
 deadline_at timestamptz NOT NULL,
 version integer NOT NULL DEFAULT 1,
 -- Evidence references (id,kind,user_id): a row can only describe a charge of
 -- its own kind and member.
 UNIQUE(id,kind,user_id)
);
CREATE INDEX charge_holds ON platform.charge(user_id) WHERE state IN ('dispatching','uncertain');
INSERT INTO platform.charge(id,user_id,kind,reserved,state,dispatched_at,deadline_at,version)
 SELECT id,user_id,'model',reserved,state,dispatched_at,deadline_at,version FROM platform.model_attempt;

ALTER TABLE platform.model_usage DROP CONSTRAINT model_usage_attempt_id_fkey;
ALTER TABLE platform.model_attempt RENAME TO model_charge;
ALTER TABLE platform.model_charge RENAME COLUMN id TO charge_id;
DROP INDEX platform.attempt_holds, platform.attempt_unresolved;
ALTER TABLE platform.model_charge DROP CONSTRAINT model_attempt_user_id_fkey,
 DROP COLUMN reserved, DROP COLUMN state, DROP COLUMN dispatched_at, DROP COLUMN deadline_at, DROP COLUMN version,
 ADD COLUMN kind text NOT NULL DEFAULT 'model' CHECK(kind='model');
ALTER TABLE platform.model_charge RENAME CONSTRAINT model_attempt_pkey TO model_charge_pkey;
ALTER TABLE platform.model_charge RENAME CONSTRAINT model_attempt_key_id_fkey TO model_charge_key_id_fkey;
ALTER TABLE platform.model_charge RENAME CONSTRAINT model_attempt_user_id_operation_id_fkey TO model_charge_user_id_operation_id_fkey;
ALTER TABLE platform.model_charge RENAME CONSTRAINT model_attempt_user_id_operation_id_attempt_number_key TO model_charge_user_id_operation_id_attempt_number_key;
ALTER INDEX platform.attempt_generation RENAME TO model_charge_generation;
ALTER TABLE platform.model_charge ADD CONSTRAINT model_charge_charge FOREIGN KEY(charge_id,kind,user_id) REFERENCES platform.charge(id,kind,user_id);

ALTER TABLE platform.model_usage RENAME TO ledger_entry;
ALTER SEQUENCE platform.model_usage_id_seq RENAME TO ledger_entry_id_seq;
ALTER TABLE platform.ledger_entry RENAME COLUMN attempt_id TO charge_id;
-- Provider-reported detail of the amount: token usage for a model call,
-- credits for a scrape or search.
ALTER TABLE platform.ledger_entry RENAME COLUMN usage TO detail;
ALTER TABLE platform.ledger_entry RENAME CONSTRAINT model_usage_pkey TO ledger_entry_pkey;
ALTER TABLE platform.ledger_entry RENAME CONSTRAINT model_usage_user_id_fkey TO ledger_entry_user_id_fkey;
ALTER TABLE platform.ledger_entry RENAME CONSTRAINT model_usage_attempt_id_dedup_key_key TO ledger_entry_charge_id_dedup_key_key;
ALTER TABLE platform.ledger_entry ADD CONSTRAINT ledger_entry_charge_id_fkey FOREIGN KEY(charge_id) REFERENCES platform.charge(id);
ALTER INDEX platform.usage_window RENAME TO ledger_entry_window;
GRANT SELECT,INSERT,UPDATE ON platform.charge TO zapply_platform;

SET LOCAL ROLE zapply_platform;

-- What a member spent in the 7- and 30-day windows ending at p_at, and what
-- their unsettled charges hold.
CREATE FUNCTION platform.spend(p_user uuid,p_at timestamptz,OUT week bigint,OUT month bigint,OUT held bigint) LANGUAGE sql STABLE
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT COALESCE(sum(amount) FILTER(WHERE accounted_at>p_at-interval '168 hours'),0)::bigint,COALESCE(sum(amount),0)::bigint,
  (SELECT COALESCE(sum(reserved),0)::bigint FROM platform.charge WHERE user_id=p_user AND state IN('dispatching','uncertain'))
 FROM platform.ledger_entry WHERE user_id=p_user AND accounted_at>p_at-interval '720 hours' AND accounted_at<=p_at $$;

-- Opens a charge that holds p_cost against the member's plan until it closes.
-- The caller holds the member's budget_account lock. A free charge (p_cost 0)
-- is only recorded: it needs no plan. Returns '' or why it cannot open.
CREATE FUNCTION platform.open_charge(p_id uuid,p_user uuid,p_kind text,p_cost bigint,p_deadline timestamptz) RETURNS text LANGUAGE plpgsql
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE ms platform.membership; p platform.plan; t timestamptz:=clock_timestamp(); s record;
BEGIN
 IF p_cost IS NULL OR p_cost<0 OR p_deadline<=t THEN RAISE EXCEPTION 'invalid reservation' USING ERRCODE='ZA422'; END IF;
 IF p_cost>0 THEN
  SELECT * INTO ms FROM platform.membership WHERE user_id=p_user;
  IF NOT FOUND THEN RETURN 'membership_required'; END IF;
  IF ms.status<>'active' THEN RETURN 'membership_suspended'; END IF;
  SELECT * INTO p FROM platform.plan WHERE id=ms.plan_id FOR SHARE;
  IF p_cost>p.cap_7d OR p_cost>p.cap_30d THEN RETURN 'request_exceeds_plan'; END IF;
  SELECT * INTO s FROM platform.spend(p_user,t);
  IF s.week+s.held+p_cost>p.cap_7d OR s.month+s.held+p_cost>p.cap_30d THEN RETURN 'usage_limit'; END IF;
 END IF;
 INSERT INTO platform.charge(id,user_id,kind,reserved,state,dispatched_at,deadline_at) VALUES(p_id,p_user,p_kind,p_cost,'dispatching',t,p_deadline);
 PERFORM platform.emit(p_user,'usage.changed',p_user);
 RETURN '';
END $$;

-- Settles a charge that is not a model call (those settle through settle) at
-- what the provider charged. Settling again does nothing.
CREATE FUNCTION platform.close_charge(p_id uuid,p_user uuid,p_amount bigint,p_source text,p_detail jsonb) RETURNS void LANGUAGE plpgsql
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE c platform.charge; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=p_user FOR UPDATE;
 SELECT * INTO c FROM platform.charge WHERE id=p_id AND user_id=p_user AND kind<>'model' FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'charge not found' USING ERRCODE='ZA404'; END IF;
 IF p_amount IS NULL OR p_amount<0 THEN RAISE EXCEPTION 'invalid cost' USING ERRCODE='ZA422'; END IF;
 IF c.state<>'dispatching' THEN RETURN; END IF;
 IF p_amount>c.reserved THEN
  PERFORM platform.audit_event('quote.exceeded',p_id,jsonb_build_object('reserved',platform.money(c.reserved),'actual',platform.money(p_amount)));
 END IF;
 INSERT INTO platform.ledger_entry(charge_id,user_id,amount,accounted_at,source,dedup_key,detail)
 VALUES(c.id,c.user_id,p_amount,c.dispatched_at,p_source,'settlement',COALESCE(p_detail,'{}'));
 UPDATE platform.charge SET state=CASE WHEN p_amount=0 THEN 'uncharged' ELSE 'settled' END,version=version+1 WHERE id=p_id;
 PERFORM platform.emit(c.user_id,'usage.changed',c.user_id);
END $$;

-- Charges that are not model calls cannot be reconciled with their provider:
-- one still open past its deadline (its process died mid-call) settles at
-- what it reserved, the most it could have cost.
CREATE FUNCTION platform.expire_charges() RETURNS integer LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE c record; n integer:=0; BEGIN
 FOR c IN SELECT id,user_id,reserved FROM platform.charge WHERE kind<>'model' AND state='dispatching' AND deadline_at<clock_timestamp() ORDER BY id LIMIT 500 LOOP
  PERFORM platform.close_charge(c.id,c.user_id,c.reserved,'deadline','{}');
  n:=n+1;
 END LOOP;
 RETURN n;
END $$;

CREATE OR REPLACE FUNCTION platform.admin_attempts(p_before uuid, p_limit integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',c.id,'user_id',c.user_id,'model',m.model,'state',c.state,'reserved_usd',platform.money(c.reserved),
   'dispatched_at',c.dispatched_at,'generation_id',m.generation_id,'error_code',m.error_code,'version',c.version) ORDER BY c.id DESC)
 FROM (SELECT * FROM platform.charge WHERE kind='model' AND state='uncertain' AND (p_before IS NULL OR id<p_before) ORDER BY id DESC LIMIT least(greatest(p_limit,1),100)) c
 JOIN platform.model_charge m ON m.charge_id=c.id),'[]');
END $function$;

CREATE OR REPLACE FUNCTION platform.attempts_for_operation(p_operation uuid) RETURNS jsonb LANGUAGE sql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT COALESCE(jsonb_agg(to_jsonb(m)-'charge_id'-'kind'||jsonb_build_object('id',c.id,'reserved',c.reserved,'dispatched_at',c.dispatched_at,
   'deadline_at',c.deadline_at,'state',c.state,'version',c.version) ORDER BY m.attempt_number DESC),'[]')
 FROM platform.model_charge m JOIN platform.charge c ON c.id=m.charge_id
 WHERE m.operation_id=p_operation AND m.user_id=public.app_user_id()
$function$;

CREATE OR REPLACE FUNCTION platform.eligible_at(p_cost bigint) RETURNS timestamp with time zone LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
DECLARE u uuid:=public.app_user_id(); t timestamptz:=statement_timestamp(); cw bigint; cm bigint; h numeric; result timestamptz;
BEGIN
 SELECT p.cap_7d,p.cap_30d INTO cw,cm FROM platform.membership m JOIN platform.plan p ON p.id=m.plan_id WHERE m.user_id=u AND m.status='active';
 IF NOT FOUND OR p_cost>cw OR p_cost>cm OR EXISTS(SELECT FROM platform.charge WHERE user_id=u AND kind='model' AND state='dispatching') THEN RETURN NULL; END IF;
 SELECT COALESCE(sum(reserved),0) INTO h FROM platform.charge WHERE user_id=u AND (state='uncertain' OR (state='dispatching' AND kind<>'model'));
 SELECT min(candidate) INTO result FROM (
  SELECT t candidate UNION SELECT accounted_at+interval '168 hours' FROM platform.ledger_entry WHERE user_id=u AND accounted_at>t-interval '168 hours'
  UNION SELECT accounted_at+interval '720 hours' FROM platform.ledger_entry WHERE user_id=u AND accounted_at>t-interval '720 hours'
 ) times WHERE (SELECT COALESCE(sum(amount),0) FROM platform.ledger_entry WHERE user_id=u AND accounted_at>candidate-interval '168 hours' AND accounted_at<=candidate)+h+p_cost<=cw
 AND (SELECT COALESCE(sum(amount),0) FROM platform.ledger_entry WHERE user_id=u AND accounted_at>candidate-interval '720 hours' AND accounted_at<=candidate)+h+p_cost<=cm;
 RETURN result;
END $function$;

CREATE OR REPLACE FUNCTION platform.load_key(p_id uuid) RETURNS TABLE(ciphertext bytea, encryption_version text) LANGUAGE sql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT k.ciphertext,k.encryption_version FROM platform.system_key k WHERE k.id=p_id
 AND EXISTS(SELECT FROM platform.model_charge a WHERE a.key_id=k.id AND a.user_id=public.app_user_id())
$function$;

CREATE OR REPLACE FUNCTION platform.match_operations_dispatched(p_ids uuid[]) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 RETURN EXISTS(SELECT FROM platform.model_charge WHERE user_id=public.app_user_id() AND operation_id=ANY(p_ids));END $function$;

CREATE OR REPLACE FUNCTION platform.model_calibration(p_model text) RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 WITH recent AS (
  SELECT c.dispatched_at,c.reserved,
   (a.quote->'shape'->>'request_bytes')::numeric AS bytes,
   (a.quote->'shape'->>'questions')::numeric AS questions,
   (a.quote->'prediction'->>'expected')::numeric AS expected,
   sum(u.amount) AS amount,
   -- Corrections append deltas with a fresh payload: sum the money, and read
   -- tokens from the latest payload that reports them.
   (array_agg(u.detail ORDER BY u.id DESC) FILTER (WHERE jsonb_typeof(COALESCE(u.detail->'input_tokens',u.detail->'prompt_tokens'))='number'))[1] AS usage
  FROM platform.model_charge a JOIN platform.charge c ON c.id=a.charge_id JOIN platform.ledger_entry u ON u.charge_id=a.charge_id
  WHERE a.model=p_model AND c.state='settled' AND jsonb_typeof(a.quote->'shape')='object'
   AND c.dispatched_at>now()-interval '30 days'
  GROUP BY a.charge_id,c.id ORDER BY c.dispatched_at DESC LIMIT 200
 ), samples AS (
  SELECT bytes,questions,expected,amount,COALESCE(usage->'input_tokens',usage->'prompt_tokens')::text::numeric AS input,
   CASE WHEN jsonb_typeof(COALESCE(usage->'output_tokens',usage->'completion_tokens'))='number'
    THEN COALESCE(usage->'output_tokens',usage->'completion_tokens')::text::numeric END AS output
  FROM recent
  WHERE bytes>0 AND questions>0 AND expected>0 AND amount>0 AND usage IS NOT NULL
 )
 SELECT jsonb_build_object(
  'samples',(SELECT count(*) FROM samples),
  'tokens_per_kb',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY input*1000/bytes)),0) FROM samples),
  'output_per_q',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY output/questions)),0) FROM samples WHERE output IS NOT NULL),
  'cost_permil',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY amount*1000/expected)),0) FROM samples),
  -- Any overrun among the latest 20 settlements forces the context ceiling.
  'exceeded',(SELECT COALESCE(bool_or(amount>reserved),false) FROM (SELECT amount,reserved FROM recent ORDER BY dispatched_at DESC LIMIT 20) latest))
$function$;

CREATE OR REPLACE FUNCTION platform.operation_costs(p_ids uuid[]) RETURNS text LANGUAGE sql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT platform.money(COALESCE(sum(u.amount),0)::bigint) FROM platform.ledger_entry u JOIN platform.model_charge a ON a.charge_id=u.charge_id
 WHERE a.user_id=public.app_user_id() AND a.operation_id=ANY(p_ids) $function$;

CREATE OR REPLACE FUNCTION platform.reserve(p_id uuid, p_operation uuid, p_model text, p_hash text, p_quote jsonb, p_cost bigint, p_deadline timestamp with time zone) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
DECLARE u uuid:=public.app_user_id(); ms platform.membership; p platform.plan; k uuid; existing record; t timestamptz; s record; n integer;
BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=u FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'member not found' USING ERRCODE='ZA404'; END IF;
 SELECT c.id,c.state INTO existing FROM platform.model_charge m JOIN platform.charge c ON c.id=m.charge_id
 WHERE m.operation_id=p_operation AND m.user_id=u AND c.state IN('dispatching','uncertain') LIMIT 1;
 IF FOUND THEN RETURN jsonb_build_object('reason','existing_attempt','attempt_id',existing.id,'state',existing.state); END IF;
 IF NOT EXISTS(SELECT FROM public.model_operation WHERE id=p_operation AND user_id=u AND status IN('pending','blocked')) THEN
   RETURN jsonb_build_object('reason','operation_not_pending');
 END IF;
 SELECT * INTO ms FROM platform.membership WHERE user_id=u;
 IF NOT FOUND THEN RETURN jsonb_build_object('reason','membership_required'); END IF;
 IF ms.status<>'active' THEN RETURN jsonb_build_object('reason','membership_suspended'); END IF;
 SELECT * INTO p FROM platform.plan WHERE id=ms.plan_id FOR SHARE;
 t:=clock_timestamp();
 IF p_cost<0 OR p_deadline<=t THEN RAISE EXCEPTION 'invalid reservation' USING ERRCODE='ZA422'; END IF;
 SELECT * INTO s FROM platform.spend(u,t);
 SELECT count(*) INTO n FROM platform.charge WHERE user_id=u AND kind='model' AND state='dispatching';
 IF p_cost>p.cap_7d OR p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','request_exceeds_plan'); END IF;
 IF n>=8 THEN RETURN jsonb_build_object('reason','in_flight'); END IF;
 IF s.week+s.held+p_cost>p.cap_7d OR s.month+s.held+p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','usage_limit'); END IF;
 SELECT id INTO k FROM platform.system_key WHERE active;
 IF k IS NULL THEN RETURN jsonb_build_object('reason','provider_key_missing'); END IF;
 SELECT COALESCE(max(attempt_number),0)+1 INTO n FROM platform.model_charge WHERE operation_id=p_operation AND user_id=u;
 INSERT INTO platform.charge(id,user_id,kind,reserved,state,dispatched_at,deadline_at) VALUES(p_id,u,'model',p_cost,'dispatching',t,p_deadline);
 INSERT INTO platform.model_charge(charge_id,user_id,operation_id,attempt_number,request_hash,model,key_id,quote)
 VALUES(p_id,u,p_operation,n,p_hash,p_model,k,p_quote);
 UPDATE public.model_operation SET status='running',blocked_reason='',next_eligible_at=NULL,version=version+1,updated_at=t WHERE id=p_operation AND user_id=u;
 PERFORM platform.emit(u,'usage.changed',u);
 RETURN jsonb_build_object('reason','','attempt_id',p_id,'key_id',k,'state','dispatching');
END $function$;

CREATE OR REPLACE FUNCTION platform.resolve_attempt(p_id uuid, p_amount bigint, p_generation text, p_reason text, p_version integer) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ DECLARE c platform.charge; m platform.model_charge; BEGIN
 PERFORM platform.assert_owner();
 SELECT * INTO c FROM platform.charge WHERE id=p_id AND kind='model';
 IF NOT FOUND THEN RAISE EXCEPTION 'attempt not found' USING ERRCODE='ZA404'; END IF;
 PERFORM 1 FROM platform.budget_account WHERE user_id=c.user_id FOR UPDATE;
 SELECT * INTO c FROM platform.charge WHERE id=p_id FOR UPDATE;
 SELECT * INTO m FROM platform.model_charge WHERE charge_id=p_id FOR UPDATE;
 IF c.version<>p_version THEN RAISE EXCEPTION 'attempt changed' USING ERRCODE='ZA412'; END IF;
 IF c.state<>'uncertain' OR c.deadline_at>clock_timestamp() THEN RAISE EXCEPTION 'attempt still active or already resolved' USING ERRCODE='ZA409'; END IF;
 IF p_generation IS NOT NULL AND m.generation_id IS NOT NULL AND p_generation<>m.generation_id THEN RAISE EXCEPTION 'generation does not match attempt' USING ERRCODE='ZA409'; END IF;
 IF p_amount<0 OR length(btrim(p_reason)) NOT BETWEEN 3 AND 1000 THEN RAISE EXCEPTION 'cost and evidence are required' USING ERRCODE='ZA422'; END IF;
 -- Do not switch the API's tenant context to access the member's operation.
 INSERT INTO platform.ledger_entry(charge_id,user_id,amount,accounted_at,source,dedup_key,detail)
 VALUES(c.id,c.user_id,p_amount,c.dispatched_at,'owner_resolution','settlement','{}');
 UPDATE platform.charge SET state='settled',version=version+1 WHERE id=p_id;
 UPDATE platform.model_charge SET generation_id=COALESCE(generation_id,p_generation) WHERE charge_id=p_id;
 PERFORM platform.audit_event('attempt.resolved',p_id,jsonb_build_object('amount',platform.money(p_amount),'reason',p_reason));
 -- The worker finishes the tenant operation on its next reconciliation pass.
 PERFORM platform.emit(c.user_id,'usage.changed',c.user_id);
END $function$;

CREATE OR REPLACE FUNCTION platform.settle(p_id uuid, p_amount bigint, p_generation text, p_usage jsonb, p_result text, p_source text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ DECLARE c platform.charge; m platform.model_charge; charged bigint; last_entry bigint; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 SELECT * INTO c FROM platform.charge WHERE id=p_id AND user_id=public.app_user_id() AND kind='model' FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'attempt not found' USING ERRCODE='ZA404'; END IF;
 SELECT * INTO m FROM platform.model_charge WHERE charge_id=p_id FOR UPDATE;
 IF p_amount<0 THEN RAISE EXCEPTION 'invalid cost' USING ERRCODE='ZA422'; END IF;
 IF p_amount>c.reserved THEN
   PERFORM platform.audit_event('quote.exceeded',p_id,jsonb_build_object('reserved',platform.money(c.reserved),'actual',platform.money(p_amount)));
 END IF;
 IF p_generation IS NOT NULL AND m.generation_id IS NOT NULL AND p_generation<>m.generation_id THEN
   RAISE EXCEPTION 'generation does not match attempt' USING ERRCODE='ZA409';
 END IF;
 IF c.state IN('settled','uncharged') THEN
   SELECT COALESCE(sum(amount),0),max(id) INTO charged,last_entry FROM platform.ledger_entry WHERE charge_id=p_id;
   IF charged=p_amount OR p_source='confirmed_uncharged' THEN RETURN; END IF;
   INSERT INTO platform.ledger_entry(charge_id,user_id,amount,accounted_at,source,dedup_key,detail)
   VALUES(c.id,c.user_id,p_amount-charged,c.dispatched_at,p_source,'correction:'||last_entry::text||':'||p_amount::text,p_usage);
   PERFORM platform.audit_event('usage.corrected',p_id,jsonb_build_object('previous',platform.money(charged),'confirmed',platform.money(p_amount)));
   PERFORM platform.emit(c.user_id,'usage.changed',c.user_id);
   RETURN;
 END IF;
 INSERT INTO platform.ledger_entry(charge_id,user_id,amount,accounted_at,source,dedup_key,detail)
 VALUES(c.id,c.user_id,p_amount,c.dispatched_at,p_source,'settlement',p_usage);
 UPDATE platform.charge SET state=CASE WHEN p_source='confirmed_uncharged' THEN 'uncharged' ELSE 'settled' END,version=version+1 WHERE id=p_id;
 UPDATE platform.model_charge SET generation_id=COALESCE(generation_id,p_generation) WHERE charge_id=p_id;
 UPDATE public.model_operation SET status=CASE WHEN p_result<>'' THEN 'completed' ELSE 'failed' END,
 result=p_result,blocked_reason=CASE WHEN p_result<>'' THEN '' ELSE 'result_unavailable' END,version=version+1,updated_at=now()
 WHERE id=m.operation_id AND user_id=c.user_id;
 PERFORM platform.emit(c.user_id,'usage.changed',c.user_id);
 PERFORM platform.emit(c.user_id,'model_operation.changed',m.operation_id);
END $function$;

CREATE OR REPLACE FUNCTION platform.uncertain(p_id uuid, p_generation text, p_reason text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ DECLARE c platform.charge; m platform.model_charge; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 SELECT * INTO c FROM platform.charge WHERE id=p_id AND user_id=public.app_user_id() AND kind='model' FOR UPDATE;
 IF NOT FOUND THEN RETURN; END IF;
 SELECT * INTO m FROM platform.model_charge WHERE charge_id=p_id FOR UPDATE;
 -- Only a real change bumps versions: reconciliation must not invalidate an
 -- owner's pending resolution or replace the original error class.
 IF NOT (c.state='dispatching' OR (c.state='uncertain' AND m.generation_id IS NULL AND p_generation IS NOT NULL)) THEN RETURN; END IF;
 UPDATE platform.model_charge SET generation_id=COALESCE(generation_id,p_generation),
   error_code=CASE WHEN c.state='dispatching' THEN p_reason ELSE error_code END WHERE charge_id=p_id;
 UPDATE platform.charge SET state='uncertain',version=version+1 WHERE id=p_id;
 UPDATE public.model_operation SET status='needs_review',blocked_reason='accounting_pending',version=version+1,updated_at=now() WHERE id=m.operation_id AND user_id=c.user_id;
 PERFORM platform.emit(c.user_id,'usage.changed',c.user_id);
 PERFORM platform.emit(c.user_id,'model_operation.changed',m.operation_id);
END $function$;

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

CREATE OR REPLACE FUNCTION platform.usage_for(p_user uuid) RETURNS jsonb LANGUAGE plpgsql STABLE
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
DECLARE t timestamptz:=statement_timestamp(); s record; cw bigint:=0; cm bigint:=0; reason text:=''; next_at timestamptz; member jsonb;
BEGIN
 member:=platform.membership_json(p_user);
 IF member IS NULL THEN reason:='membership_required';
 ELSE
   SELECT p.cap_7d,p.cap_30d INTO cw,cm FROM platform.membership ms JOIN platform.plan p ON p.id=ms.plan_id WHERE ms.user_id=p_user;
   IF member->>'status'='suspended' THEN reason:='membership_suspended'; END IF;
 END IF;
 SELECT * INTO s FROM platform.spend(p_user,t);
 SELECT min(expiry) INTO next_at FROM (
   SELECT accounted_at+interval '168 hours' expiry FROM platform.ledger_entry WHERE user_id=p_user AND amount>0 AND accounted_at>t-interval '168 hours'
   UNION ALL SELECT accounted_at+interval '720 hours' FROM platform.ledger_entry WHERE user_id=p_user AND amount>0 AND accounted_at>t-interval '720 hours'
 ) x;
 IF reason='' AND EXISTS(SELECT FROM platform.charge WHERE user_id=p_user AND kind='model' AND state='dispatching') THEN reason:='in_flight'; END IF;
 IF reason='' AND (s.week::numeric+s.held>=cw OR s.month::numeric+s.held>=cm) THEN reason:='usage_limit'; END IF;
 RETURN jsonb_build_object('membership',member,'server_time',t,'cap_7d_usd',platform.money(cw),'cap_30d_usd',platform.money(cm),
   'spent_7d_usd',platform.money(s.week),'spent_30d_usd',platform.money(s.month),'reserved_usd',platform.money(s.held),
   'available_7d_usd',platform.money(greatest(0,cw-s.week-s.held)),'available_30d_usd',platform.money(greatest(0,cm-s.month-s.held)),
   'next_capacity_at',next_at,'blocked_reason',reason);
END $function$;

-- Grants come from the owner: the migration login does not inherit it.
REVOKE EXECUTE ON FUNCTION platform.spend(uuid,timestamptz),platform.open_charge(uuid,uuid,text,bigint,timestamptz),
 platform.close_charge(uuid,uuid,bigint,text,jsonb),platform.expire_charges() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.expire_charges() TO zapply_worker;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT FROM platform.charge WHERE kind<>'model') THEN
  RAISE EXCEPTION 'the ledger holds scrape or search charges; roll back their migrations first';
 END IF;
END $$;
SET LOCAL ROLE zapply_platform;
DROP FUNCTION platform.expire_charges(),platform.close_charge(uuid,uuid,bigint,text,jsonb),platform.open_charge(uuid,uuid,text,bigint,timestamptz);
RESET ROLE;
ALTER TABLE platform.ledger_entry DROP CONSTRAINT ledger_entry_charge_id_fkey;
ALTER INDEX platform.ledger_entry_window RENAME TO usage_window;
ALTER TABLE platform.ledger_entry RENAME CONSTRAINT ledger_entry_charge_id_dedup_key_key TO model_usage_attempt_id_dedup_key_key;
ALTER TABLE platform.ledger_entry RENAME CONSTRAINT ledger_entry_user_id_fkey TO model_usage_user_id_fkey;
ALTER TABLE platform.ledger_entry RENAME CONSTRAINT ledger_entry_pkey TO model_usage_pkey;
ALTER TABLE platform.ledger_entry RENAME COLUMN detail TO usage;
ALTER TABLE platform.ledger_entry RENAME COLUMN charge_id TO attempt_id;
ALTER SEQUENCE platform.ledger_entry_id_seq RENAME TO model_usage_id_seq;
ALTER TABLE platform.ledger_entry RENAME TO model_usage;

ALTER TABLE platform.model_charge DROP CONSTRAINT model_charge_charge;
ALTER TABLE platform.model_charge ADD COLUMN reserved bigint, ADD COLUMN state text, ADD COLUMN dispatched_at timestamptz,
 ADD COLUMN deadline_at timestamptz, ADD COLUMN version integer NOT NULL DEFAULT 1;
UPDATE platform.model_charge m SET reserved=c.reserved,state=c.state,dispatched_at=c.dispatched_at,deadline_at=c.deadline_at,version=c.version
 FROM platform.charge c WHERE c.id=m.charge_id;
ALTER TABLE platform.model_charge DROP COLUMN kind,
 ALTER COLUMN reserved SET NOT NULL, ALTER COLUMN state SET NOT NULL, ALTER COLUMN dispatched_at SET NOT NULL, ALTER COLUMN deadline_at SET NOT NULL,
 ADD CONSTRAINT model_attempt_reserved_check CHECK(reserved >= 0),
 ADD CONSTRAINT model_attempt_state_check CHECK(state IN ('dispatching','uncertain','settled','uncharged')),
 ADD CONSTRAINT model_attempt_user_id_fkey FOREIGN KEY(user_id) REFERENCES platform.budget_account(user_id);
ALTER INDEX platform.model_charge_generation RENAME TO attempt_generation;
ALTER TABLE platform.model_charge RENAME CONSTRAINT model_charge_user_id_operation_id_attempt_number_key TO model_attempt_user_id_operation_id_attempt_number_key;
ALTER TABLE platform.model_charge RENAME CONSTRAINT model_charge_user_id_operation_id_fkey TO model_attempt_user_id_operation_id_fkey;
ALTER TABLE platform.model_charge RENAME CONSTRAINT model_charge_key_id_fkey TO model_attempt_key_id_fkey;
ALTER TABLE platform.model_charge RENAME CONSTRAINT model_charge_pkey TO model_attempt_pkey;
ALTER TABLE platform.model_charge RENAME COLUMN charge_id TO id;
ALTER TABLE platform.model_charge RENAME TO model_attempt;
CREATE UNIQUE INDEX attempt_unresolved ON platform.model_attempt(user_id,operation_id) WHERE state IN ('dispatching','uncertain');
CREATE INDEX attempt_holds ON platform.model_attempt(user_id) WHERE state IN ('dispatching','uncertain');
ALTER TABLE platform.model_usage ADD CONSTRAINT model_usage_attempt_id_fkey FOREIGN KEY(attempt_id) REFERENCES platform.model_attempt(id);

SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.admin_attempts(p_before uuid, p_limit integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'user_id',user_id,'model',model,'state',state,'reserved_usd',platform.money(reserved),
   'dispatched_at',dispatched_at,'generation_id',generation_id,'error_code',error_code,'version',version) ORDER BY id DESC)
 FROM (SELECT * FROM platform.model_attempt WHERE state='uncertain' AND (p_before IS NULL OR id<p_before) ORDER BY id DESC LIMIT least(greatest(p_limit,1),100)) a),'[]');
END $function$;
CREATE OR REPLACE FUNCTION platform.attempts_for_operation(p_operation uuid) RETURNS jsonb LANGUAGE sql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY attempt_number DESC),'[]') FROM platform.model_attempt a
 WHERE a.operation_id=p_operation AND a.user_id=public.app_user_id()
$function$;
CREATE OR REPLACE FUNCTION platform.eligible_at(p_cost bigint) RETURNS timestamp with time zone LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
DECLARE u uuid:=public.app_user_id(); t timestamptz:=statement_timestamp(); cw bigint; cm bigint; h numeric; result timestamptz;
BEGIN
 SELECT p.cap_7d,p.cap_30d INTO cw,cm FROM platform.membership m JOIN platform.plan p ON p.id=m.plan_id WHERE m.user_id=u AND m.status='active';
 IF NOT FOUND OR p_cost>cw OR p_cost>cm OR EXISTS(SELECT FROM platform.model_attempt WHERE user_id=u AND state='dispatching') THEN RETURN NULL; END IF;
 SELECT COALESCE(sum(reserved),0) INTO h FROM platform.model_attempt WHERE user_id=u AND state='uncertain';
 SELECT min(candidate) INTO result FROM (
  SELECT t candidate UNION SELECT accounted_at+interval '168 hours' FROM platform.model_usage WHERE user_id=u AND accounted_at>t-interval '168 hours'
  UNION SELECT accounted_at+interval '720 hours' FROM platform.model_usage WHERE user_id=u AND accounted_at>t-interval '720 hours'
 ) times WHERE (SELECT COALESCE(sum(amount),0) FROM platform.model_usage WHERE user_id=u AND accounted_at>candidate-interval '168 hours' AND accounted_at<=candidate)+h+p_cost<=cw
 AND (SELECT COALESCE(sum(amount),0) FROM platform.model_usage WHERE user_id=u AND accounted_at>candidate-interval '720 hours' AND accounted_at<=candidate)+h+p_cost<=cm;
 RETURN result;
END $function$;
CREATE OR REPLACE FUNCTION platform.load_key(p_id uuid) RETURNS TABLE(ciphertext bytea, encryption_version text) LANGUAGE sql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT k.ciphertext,k.encryption_version FROM platform.system_key k WHERE k.id=p_id
 AND EXISTS(SELECT FROM platform.model_attempt a WHERE a.key_id=k.id AND a.user_id=public.app_user_id())
$function$;
CREATE OR REPLACE FUNCTION platform.match_operations_dispatched(p_ids uuid[]) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 RETURN EXISTS(SELECT FROM platform.model_attempt WHERE user_id=public.app_user_id() AND operation_id=ANY(p_ids));END $function$;
CREATE OR REPLACE FUNCTION platform.model_calibration(p_model text)
 RETURNS jsonb
 LANGUAGE sql
 STABLE SECURITY DEFINER
 SET search_path TO 'pg_catalog', 'platform', 'pg_temp'
AS $function$
 WITH recent AS (
  SELECT a.dispatched_at,a.reserved,
   (a.quote->'shape'->>'request_bytes')::numeric AS bytes,
   (a.quote->'shape'->>'questions')::numeric AS questions,
   (a.quote->'prediction'->>'expected')::numeric AS expected,
   sum(u.amount) AS amount,
   -- Corrections append deltas with a fresh payload: sum the money, and read
   -- tokens from the latest payload that reports them.
   (array_agg(u.usage ORDER BY u.id DESC) FILTER (WHERE jsonb_typeof(COALESCE(u.usage->'input_tokens',u.usage->'prompt_tokens'))='number'))[1] AS usage
  FROM platform.model_attempt a JOIN platform.model_usage u ON u.attempt_id=a.id
  WHERE a.model=p_model AND a.state='settled' AND jsonb_typeof(a.quote->'shape')='object'
   AND a.dispatched_at>now()-interval '30 days'
  GROUP BY a.id ORDER BY a.dispatched_at DESC LIMIT 200
 ), samples AS (
  SELECT bytes,questions,expected,amount,COALESCE(usage->'input_tokens',usage->'prompt_tokens')::text::numeric AS input,
   CASE WHEN jsonb_typeof(COALESCE(usage->'output_tokens',usage->'completion_tokens'))='number'
    THEN COALESCE(usage->'output_tokens',usage->'completion_tokens')::text::numeric END AS output
  FROM recent
  WHERE bytes>0 AND questions>0 AND expected>0 AND amount>0 AND usage IS NOT NULL
 )
 SELECT jsonb_build_object(
  'samples',(SELECT count(*) FROM samples),
  'tokens_per_kb',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY input*1000/bytes)),0) FROM samples),
  'output_per_q',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY output/questions)),0) FROM samples WHERE output IS NOT NULL),
  'cost_permil',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY amount*1000/expected)),0) FROM samples),
  -- Any overrun among the latest 20 settlements forces the context ceiling.
  'exceeded',(SELECT COALESCE(bool_or(amount>reserved),false) FROM (SELECT amount,reserved FROM recent ORDER BY dispatched_at DESC LIMIT 20) latest))
$function$;
CREATE OR REPLACE FUNCTION platform.operation_costs(p_ids uuid[]) RETURNS text LANGUAGE sql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
 SELECT platform.money(COALESCE(sum(u.amount),0)::bigint) FROM platform.model_usage u JOIN platform.model_attempt a ON a.id=u.attempt_id WHERE a.user_id=public.app_user_id() AND a.operation_id=ANY(p_ids) $function$;
CREATE OR REPLACE FUNCTION platform.reserve(p_id uuid, p_operation uuid, p_model text, p_hash text, p_quote jsonb, p_cost bigint, p_deadline timestamp with time zone) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
DECLARE u uuid:=public.app_user_id(); ms platform.membership; p platform.plan; k uuid; a platform.model_attempt; t timestamptz; w numeric; m numeric; h numeric; n integer;
BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=u FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'member not found' USING ERRCODE='ZA404'; END IF;
 SELECT * INTO a FROM platform.model_attempt WHERE operation_id=p_operation AND user_id=u AND state IN('dispatching','uncertain') LIMIT 1;
 IF FOUND THEN RETURN jsonb_build_object('reason','existing_attempt','attempt_id',a.id,'state',a.state); END IF;
 IF NOT EXISTS(SELECT FROM public.model_operation WHERE id=p_operation AND user_id=u AND status IN('pending','blocked')) THEN
   RETURN jsonb_build_object('reason','operation_not_pending');
 END IF;
 SELECT * INTO ms FROM platform.membership WHERE user_id=u;
 IF NOT FOUND THEN RETURN jsonb_build_object('reason','membership_required'); END IF;
 IF ms.status<>'active' THEN RETURN jsonb_build_object('reason','membership_suspended'); END IF;
 SELECT * INTO p FROM platform.plan WHERE id=ms.plan_id FOR SHARE;
 t:=clock_timestamp();
 IF p_cost<0 OR p_deadline<=t THEN RAISE EXCEPTION 'invalid reservation' USING ERRCODE='ZA422'; END IF;
 SELECT COALESCE(sum(amount) FILTER(WHERE accounted_at>t-interval '168 hours'),0),COALESCE(sum(amount),0) INTO w,m
 FROM platform.model_usage WHERE user_id=u AND accounted_at>t-interval '720 hours' AND accounted_at<=t;
 SELECT COALESCE(sum(reserved),0),count(*) FILTER (WHERE state='dispatching') INTO h,n FROM platform.model_attempt WHERE user_id=u AND state IN('dispatching','uncertain');
 IF p_cost>p.cap_7d OR p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','request_exceeds_plan'); END IF;
 IF n>=8 THEN RETURN jsonb_build_object('reason','in_flight'); END IF;
 IF w+h+p_cost>p.cap_7d OR m+h+p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','usage_limit'); END IF;
 SELECT id INTO k FROM platform.system_key WHERE active;
 IF k IS NULL THEN RETURN jsonb_build_object('reason','provider_key_missing'); END IF;
 SELECT COALESCE(max(attempt_number),0)+1 INTO n FROM platform.model_attempt WHERE operation_id=p_operation AND user_id=u;
 INSERT INTO platform.model_attempt(id,user_id,operation_id,attempt_number,request_hash,model,key_id,quote,reserved,dispatched_at,deadline_at,state)
 VALUES(p_id,u,p_operation,n,p_hash,p_model,k,p_quote,p_cost,t,p_deadline,'dispatching');
 UPDATE public.model_operation SET status='running',blocked_reason='',next_eligible_at=NULL,version=version+1,updated_at=t WHERE id=p_operation AND user_id=u;
 PERFORM platform.emit(u,'usage.changed',u);
 RETURN jsonb_build_object('reason','','attempt_id',p_id,'key_id',k,'state','dispatching');
END $function$;
CREATE OR REPLACE FUNCTION platform.resolve_attempt(p_id uuid, p_amount bigint, p_generation text, p_reason text, p_version integer)
 RETURNS void
 LANGUAGE plpgsql
 SECURITY DEFINER
 SET search_path TO 'pg_catalog', 'platform', 'pg_temp'
AS $function$ DECLARE a platform.model_attempt; BEGIN
 PERFORM platform.assert_owner();
 SELECT * INTO a FROM platform.model_attempt WHERE id=p_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'attempt not found' USING ERRCODE='ZA404'; END IF;
 PERFORM 1 FROM platform.budget_account WHERE user_id=a.user_id FOR UPDATE;
 SELECT * INTO a FROM platform.model_attempt WHERE id=p_id FOR UPDATE;
 IF a.version<>p_version THEN RAISE EXCEPTION 'attempt changed' USING ERRCODE='ZA412'; END IF;
 IF a.state<>'uncertain' OR a.deadline_at>clock_timestamp() THEN RAISE EXCEPTION 'attempt still active or already resolved' USING ERRCODE='ZA409'; END IF;
 IF p_generation IS NOT NULL AND a.generation_id IS NOT NULL AND p_generation<>a.generation_id THEN RAISE EXCEPTION 'generation does not match attempt' USING ERRCODE='ZA409'; END IF;
 IF p_amount<0 OR length(btrim(p_reason)) NOT BETWEEN 3 AND 1000 THEN RAISE EXCEPTION 'cost and evidence are required' USING ERRCODE='ZA422'; END IF;
 -- Do not switch the API's tenant context to access the member's operation.
 INSERT INTO platform.model_usage(attempt_id,user_id,amount,accounted_at,source,dedup_key,usage)
 VALUES(a.id,a.user_id,p_amount,a.dispatched_at,'owner_resolution','settlement','{}');
 UPDATE platform.model_attempt SET state='settled',generation_id=COALESCE(generation_id,p_generation),version=version+1 WHERE id=p_id;
 PERFORM platform.audit_event('attempt.resolved',p_id,jsonb_build_object('amount',platform.money(p_amount),'reason',p_reason));
 -- The worker finishes the tenant operation on its next reconciliation pass.
 PERFORM platform.emit(a.user_id,'usage.changed',a.user_id);
END $function$;
CREATE OR REPLACE FUNCTION platform.settle(p_id uuid, p_amount bigint, p_generation text, p_usage jsonb, p_result text, p_source text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$ DECLARE a platform.model_attempt; charged bigint; last_entry bigint; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 SELECT * INTO a FROM platform.model_attempt WHERE id=p_id AND user_id=public.app_user_id() FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'attempt not found' USING ERRCODE='ZA404'; END IF;
 IF p_amount<0 THEN RAISE EXCEPTION 'invalid cost' USING ERRCODE='ZA422'; END IF;
 IF p_amount>a.reserved THEN
   PERFORM platform.audit_event('quote.exceeded',p_id,jsonb_build_object('reserved',platform.money(a.reserved),'actual',platform.money(p_amount)));
 END IF;
 IF p_generation IS NOT NULL AND a.generation_id IS NOT NULL AND p_generation<>a.generation_id THEN
   RAISE EXCEPTION 'generation does not match attempt' USING ERRCODE='ZA409';
 END IF;
 IF a.state IN('settled','uncharged') THEN
   SELECT COALESCE(sum(amount),0),max(id) INTO charged,last_entry FROM platform.model_usage WHERE attempt_id=p_id;
   IF charged=p_amount OR p_source='confirmed_uncharged' THEN RETURN; END IF;
   INSERT INTO platform.model_usage(attempt_id,user_id,amount,accounted_at,source,dedup_key,usage)
   VALUES(a.id,a.user_id,p_amount-charged,a.dispatched_at,p_source,'correction:'||last_entry::text||':'||p_amount::text,p_usage);
   PERFORM platform.audit_event('usage.corrected',p_id,jsonb_build_object('previous',platform.money(charged),'confirmed',platform.money(p_amount)));
   PERFORM platform.emit(a.user_id,'usage.changed',a.user_id);
   RETURN;
 END IF;
 INSERT INTO platform.model_usage(attempt_id,user_id,amount,accounted_at,source,dedup_key,usage)
 VALUES(a.id,a.user_id,p_amount,a.dispatched_at,p_source,'settlement',p_usage);
 UPDATE platform.model_attempt SET state=CASE WHEN p_source='confirmed_uncharged' THEN 'uncharged' ELSE 'settled' END,
 generation_id=COALESCE(generation_id,p_generation),version=version+1 WHERE id=p_id;
 UPDATE public.model_operation SET status=CASE WHEN p_result<>'' THEN 'completed' ELSE 'failed' END,
 result=p_result,blocked_reason=CASE WHEN p_result<>'' THEN '' ELSE 'result_unavailable' END,version=version+1,updated_at=now()
 WHERE id=a.operation_id AND user_id=a.user_id;
 PERFORM platform.emit(a.user_id,'usage.changed',a.user_id);
 PERFORM platform.emit(a.user_id,'model_operation.changed',a.operation_id);
END $function$;
CREATE OR REPLACE FUNCTION platform.uncertain(p_id uuid, p_generation text, p_reason text)
 RETURNS void
 LANGUAGE plpgsql
 SECURITY DEFINER
 SET search_path TO 'pg_catalog', 'platform', 'pg_temp'
AS $function$ DECLARE a platform.model_attempt; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 -- Only a real change bumps versions: reconciliation must not invalidate an
 -- owner's pending resolution or replace the original error class.
 UPDATE platform.model_attempt SET state='uncertain',generation_id=COALESCE(generation_id,p_generation),
   error_code=CASE WHEN state='dispatching' THEN p_reason ELSE error_code END,version=version+1
 WHERE id=p_id AND user_id=public.app_user_id()
   AND (state='dispatching' OR (state='uncertain' AND generation_id IS NULL AND p_generation IS NOT NULL))
 RETURNING * INTO a;
 IF FOUND THEN
   UPDATE public.model_operation SET status='needs_review',blocked_reason='accounting_pending',version=version+1,updated_at=now() WHERE id=a.operation_id AND user_id=a.user_id;
   PERFORM platform.emit(a.user_id,'usage.changed',a.user_id);
   PERFORM platform.emit(a.user_id,'model_operation.changed',a.operation_id);
 END IF;
END $function$;
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
 FROM platform.model_usage u
 JOIN platform.model_attempt a ON a.id=u.attempt_id
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
CREATE OR REPLACE FUNCTION platform.usage_for(p_user uuid) RETURNS jsonb LANGUAGE plpgsql STABLE
SET search_path TO 'pg_catalog', 'platform', 'pg_temp' AS $function$
DECLARE t timestamptz:=statement_timestamp(); w bigint:=0; m bigint:=0; h bigint:=0; cw bigint:=0; cm bigint:=0; reason text:=''; next_at timestamptz; member jsonb;
BEGIN
 member:=platform.membership_json(p_user);
 IF member IS NULL THEN reason:='membership_required';
 ELSE
   SELECT p.cap_7d,p.cap_30d INTO cw,cm FROM platform.membership ms JOIN platform.plan p ON p.id=ms.plan_id WHERE ms.user_id=p_user;
   IF member->>'status'='suspended' THEN reason:='membership_suspended'; END IF;
 END IF;
 SELECT COALESCE(sum(amount) FILTER(WHERE accounted_at>t-interval '168 hours'),0),COALESCE(sum(amount),0)
 INTO w,m FROM platform.model_usage WHERE user_id=p_user AND accounted_at>t-interval '720 hours' AND accounted_at<=t;
 SELECT COALESCE(sum(reserved),0) INTO h FROM platform.model_attempt WHERE user_id=p_user AND state IN ('dispatching','uncertain');
 SELECT min(expiry) INTO next_at FROM (
   SELECT accounted_at+interval '168 hours' expiry FROM platform.model_usage WHERE user_id=p_user AND amount>0 AND accounted_at>t-interval '168 hours'
   UNION ALL SELECT accounted_at+interval '720 hours' FROM platform.model_usage WHERE user_id=p_user AND amount>0 AND accounted_at>t-interval '720 hours'
 ) x;
 IF reason='' AND EXISTS(SELECT FROM platform.model_attempt WHERE user_id=p_user AND state='dispatching') THEN reason:='in_flight'; END IF;
 IF reason='' AND (w::numeric+h>=cw OR m::numeric+h>=cm) THEN reason:='usage_limit'; END IF;
 RETURN jsonb_build_object('membership',member,'server_time',t,'cap_7d_usd',platform.money(cw),'cap_30d_usd',platform.money(cm),
   'spent_7d_usd',platform.money(w),'spent_30d_usd',platform.money(m),'reserved_usd',platform.money(h),
   'available_7d_usd',platform.money(greatest(0,cw-w-h)),'available_30d_usd',platform.money(greatest(0,cm-m-h)),
   'next_capacity_at',next_at,'blocked_reason',reason);
END $function$;
DROP FUNCTION platform.spend(uuid,timestamptz);
RESET ROLE;
DROP TABLE platform.charge;
-- +goose StatementEnd
