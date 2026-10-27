-- Platform administration is available only through explicitly granted functions.
-- Tenant operation results remain under forced RLS.
-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'zapply_worker') THEN
    CREATE ROLE zapply_worker NOLOGIN NOSUPERUSER NOBYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'zapply_platform') THEN
    CREATE ROLE zapply_platform NOLOGIN NOSUPERUSER NOBYPASSRLS;
  END IF;
END $$;
CREATE SCHEMA platform;
REVOKE ALL ON SCHEMA platform FROM PUBLIC;
GRANT USAGE ON SCHEMA public, platform TO zapply_app, zapply_worker;
GRANT EXECUTE ON FUNCTION public.app_user_id() TO zapply_worker;

CREATE TABLE platform.owner (user_id uuid PRIMARY KEY REFERENCES public.app_user(id));
CREATE TABLE platform.plan (
  id uuid PRIMARY KEY, name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
  cap_7d bigint NOT NULL CHECK (cap_7d > 0), cap_30d bigint NOT NULL CHECK (cap_30d > 0),
  version integer NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE platform.budget_account (user_id uuid PRIMARY KEY REFERENCES public.app_user(id));
CREATE TABLE platform.membership (
  user_id uuid PRIMARY KEY REFERENCES platform.budget_account(user_id), plan_id uuid NOT NULL REFERENCES platform.plan(id),
  status text NOT NULL CHECK (status IN ('active','suspended')), version integer NOT NULL DEFAULT 1,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE platform.audit (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, actor_id uuid, action text NOT NULL,
  subject_id uuid NOT NULL, detail jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE platform.system_key (
  id uuid PRIMARY KEY, label text NOT NULL CHECK (length(btrim(label)) BETWEEN 1 AND 100),
  ciphertext bytea NOT NULL, encryption_version text NOT NULL, fingerprint text NOT NULL,
  active boolean NOT NULL DEFAULT false, version integer NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX system_key_active ON platform.system_key ((true)) WHERE active;
-- A stable row serializes activation, including activating the first key.
CREATE TABLE platform.provider_lock (id boolean PRIMARY KEY CHECK(id));
INSERT INTO platform.provider_lock VALUES (true);

CREATE TABLE llm_operation (
  id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES app_user(id), kind text NOT NULL DEFAULT 'diagnostic',
  idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255), request_hash text NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','blocked','running','completed','failed','needs_review','cancelled')),
  blocked_reason text NOT NULL DEFAULT '', next_eligible_at timestamptz, result text NOT NULL DEFAULT '' CHECK (length(result) <= 16000),
  version integer NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(user_id,id), UNIQUE(user_id,kind,idempotency_key)
);
ALTER TABLE llm_operation ENABLE ROW LEVEL SECURITY;
ALTER TABLE llm_operation FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON llm_operation TO zapply_app, zapply_worker
  USING (user_id = app_user_id()) WITH CHECK (user_id = app_user_id());
REVOKE ALL ON llm_operation FROM PUBLIC, zapply_app;
GRANT SELECT, INSERT ON llm_operation TO zapply_app;
GRANT SELECT, UPDATE ON llm_operation TO zapply_worker;
GRANT INSERT ON outbox TO zapply_worker;
CREATE POLICY worker_insert ON outbox FOR INSERT TO zapply_worker WITH CHECK(user_id = app_user_id());

CREATE TABLE platform.llm_attempt (
  id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES platform.budget_account(user_id), operation_id uuid NOT NULL,
  attempt_number integer NOT NULL, request_hash text NOT NULL, model text NOT NULL, key_id uuid NOT NULL REFERENCES platform.system_key(id),
  quote jsonb NOT NULL, reserved bigint NOT NULL CHECK (reserved >= 0), dispatched_at timestamptz NOT NULL,
  deadline_at timestamptz NOT NULL, state text NOT NULL CHECK (state IN ('dispatching','uncertain','settled','uncharged')),
  generation_id text, error_code text NOT NULL DEFAULT '', version integer NOT NULL DEFAULT 1,
  FOREIGN KEY(user_id,operation_id) REFERENCES public.llm_operation(user_id,id), UNIQUE(user_id,operation_id,attempt_number)
);
CREATE UNIQUE INDEX attempt_generation ON platform.llm_attempt(generation_id) WHERE generation_id IS NOT NULL;
CREATE UNIQUE INDEX attempt_unresolved ON platform.llm_attempt(user_id,operation_id) WHERE state IN ('dispatching','uncertain');
CREATE INDEX attempt_holds ON platform.llm_attempt(user_id) WHERE state IN ('dispatching','uncertain');
CREATE TABLE platform.operation_index (
 operation_id uuid PRIMARY KEY REFERENCES public.llm_operation(id) ON DELETE CASCADE,
 user_id uuid NOT NULL, status text NOT NULL, next_eligible_at timestamptz, updated_at timestamptz NOT NULL
);
CREATE TABLE platform.llm_usage (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, attempt_id uuid NOT NULL REFERENCES platform.llm_attempt(id),
  user_id uuid NOT NULL REFERENCES platform.budget_account(user_id), amount bigint NOT NULL,
  accounted_at timestamptz NOT NULL, source text NOT NULL, dedup_key text NOT NULL,
  usage jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(attempt_id,dedup_key)
);
CREATE INDEX usage_window ON platform.llm_usage(user_id,accounted_at);
REVOKE ALL ON ALL TABLES IN SCHEMA platform FROM PUBLIC, zapply_app, zapply_worker;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA platform FROM PUBLIC, zapply_app, zapply_worker;

CREATE FUNCTION platform.initialize_account() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
  INSERT INTO platform.budget_account(user_id) VALUES (NEW.id) ON CONFLICT DO NOTHING;
  RETURN NEW;
END $$;
CREATE TRIGGER initialize_budget AFTER INSERT ON app_user FOR EACH ROW EXECUTE FUNCTION platform.initialize_account();
CREATE FUNCTION platform.index_operation() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 INSERT INTO platform.operation_index VALUES(NEW.id,NEW.user_id,NEW.status,NEW.next_eligible_at,NEW.updated_at)
 ON CONFLICT(operation_id) DO UPDATE SET status=EXCLUDED.status,next_eligible_at=EXCLUDED.next_eligible_at,updated_at=EXCLUDED.updated_at;
 RETURN NEW;
END $$;
CREATE TRIGGER index_model_operation AFTER INSERT OR UPDATE ON llm_operation FOR EACH ROW EXECUTE FUNCTION platform.index_operation();
ALTER TABLE app_user NO FORCE ROW LEVEL SECURITY;
INSERT INTO platform.budget_account SELECT id FROM public.app_user;
ALTER TABLE app_user FORCE ROW LEVEL SECURITY;

CREATE FUNCTION platform.assert_owner() RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
  IF NOT EXISTS (SELECT FROM platform.owner WHERE user_id = public.app_user_id()) THEN
    RAISE EXCEPTION 'platform owner access required' USING ERRCODE='ZA403';
  END IF;
END $$;
CREATE FUNCTION platform.role() RETURNS text LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
  SELECT CASE WHEN EXISTS (SELECT FROM platform.owner WHERE user_id=public.app_user_id()) THEN 'owner' ELSE 'member' END
$$;
CREATE FUNCTION platform.money(p bigint) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT (p::numeric/1000000000000)::numeric(24,12)::text $$;
CREATE FUNCTION platform.emit(p_user uuid, p_topic text, p_id uuid, p_version integer DEFAULT 0) RETURNS void LANGUAGE sql
SET search_path = pg_catalog, platform, pg_temp AS $$
  INSERT INTO public.outbox(id,user_id,topic,payload) VALUES(gen_random_uuid(),p_user,p_topic,jsonb_build_object('id',p_id,'version',p_version))
$$;
CREATE FUNCTION platform.audit_event(p_action text,p_id uuid,p_detail jsonb DEFAULT '{}') RETURNS void LANGUAGE sql
SET search_path = pg_catalog, platform, pg_temp AS $$
  INSERT INTO platform.audit(actor_id,action,subject_id,detail) VALUES(public.app_user_id(),p_action,p_id,p_detail)
$$;
CREATE FUNCTION platform.plan_json(p platform.plan) RETURNS jsonb LANGUAGE sql STABLE
SET search_path = pg_catalog, platform, pg_temp AS $$
 SELECT jsonb_build_object('id',p.id,'name',p.name,'cap_7d_usd',platform.money(p.cap_7d),'cap_30d_usd',platform.money(p.cap_30d),'version',p.version)
$$;
CREATE FUNCTION platform.plans() RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(platform.plan_json(p) ORDER BY p.name,p.id) FROM platform.plan p),'[]');
END $$;
CREATE FUNCTION platform.save_plan(p_id uuid,p_name text,p_week bigint,p_month bigint,p_version integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ DECLARE p platform.plan; u uuid; BEGIN
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
END $$;
CREATE FUNCTION platform.membership_json(p_user uuid) RETURNS jsonb LANGUAGE sql STABLE
SET search_path = pg_catalog, platform, pg_temp AS $$
 SELECT jsonb_build_object('user_id',m.user_id,'plan_id',m.plan_id,'plan_name',p.name,'status',m.status,'version',m.version,'updated_at',m.updated_at)
 FROM platform.membership m JOIN platform.plan p ON p.id=m.plan_id WHERE m.user_id=p_user
$$;
CREATE FUNCTION platform.save_membership(p_user uuid,p_plan uuid,p_status text,p_version integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 PERFORM 1 FROM platform.budget_account WHERE user_id=p_user FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'member not found' USING ERRCODE='ZA404'; END IF;
 PERFORM 1 FROM platform.plan WHERE id=p_plan FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'plan not found' USING ERRCODE='ZA404'; END IF;
 IF p_version=0 THEN
   INSERT INTO platform.membership(user_id,plan_id,status) VALUES(p_user,p_plan,p_status) ON CONFLICT DO NOTHING;
   IF NOT FOUND THEN RAISE EXCEPTION 'membership changed' USING ERRCODE='ZA412'; END IF;
 ELSE
   UPDATE platform.membership SET plan_id=p_plan,status=p_status,version=version+1,updated_at=now() WHERE user_id=p_user AND version=p_version;
   IF NOT FOUND THEN RAISE EXCEPTION 'membership changed' USING ERRCODE='ZA412'; END IF;
 END IF;
 PERFORM platform.audit_event('membership.saved',p_user,platform.membership_json(p_user));
 PERFORM platform.emit(p_user,'membership.changed',p_user,(SELECT version FROM platform.membership WHERE user_id=p_user));
 RETURN platform.membership_json(p_user);
END $$;

CREATE FUNCTION platform.usage_for(p_user uuid) RETURNS jsonb LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, platform, pg_temp AS $$
DECLARE t timestamptz:=statement_timestamp(); w bigint:=0; m bigint:=0; h bigint:=0; cw bigint:=0; cm bigint:=0; reason text:=''; next_at timestamptz; member jsonb;
BEGIN
 member:=platform.membership_json(p_user);
 IF member IS NULL THEN reason:='membership_required';
 ELSE
   SELECT p.cap_7d,p.cap_30d INTO cw,cm FROM platform.membership ms JOIN platform.plan p ON p.id=ms.plan_id WHERE ms.user_id=p_user;
   IF member->>'status'='suspended' THEN reason:='membership_suspended'; END IF;
 END IF;
 SELECT COALESCE(sum(amount) FILTER(WHERE accounted_at>t-interval '168 hours'),0),COALESCE(sum(amount),0)
 INTO w,m FROM platform.llm_usage WHERE user_id=p_user AND accounted_at>t-interval '720 hours' AND accounted_at<=t;
 SELECT COALESCE(sum(reserved),0) INTO h FROM platform.llm_attempt WHERE user_id=p_user AND state IN ('dispatching','uncertain');
 SELECT min(expiry) INTO next_at FROM (
   SELECT accounted_at+interval '168 hours' expiry FROM platform.llm_usage WHERE user_id=p_user AND amount>0 AND accounted_at>t-interval '168 hours'
   UNION ALL SELECT accounted_at+interval '720 hours' FROM platform.llm_usage WHERE user_id=p_user AND amount>0 AND accounted_at>t-interval '720 hours'
 ) x;
 IF reason='' AND EXISTS(SELECT FROM platform.llm_attempt WHERE user_id=p_user AND state='uncertain') THEN reason:='accounting_pending'; END IF;
 IF reason='' AND EXISTS(SELECT FROM platform.llm_attempt WHERE user_id=p_user AND state='dispatching') THEN reason:='in_flight'; END IF;
 IF reason='' AND (w::numeric+h>=cw OR m::numeric+h>=cm) THEN reason:='usage_limit'; END IF;
 RETURN jsonb_build_object('membership',member,'server_time',t,'cap_7d_usd',platform.money(cw),'cap_30d_usd',platform.money(cm),
   'spent_7d_usd',platform.money(w),'spent_30d_usd',platform.money(m),'reserved_usd',platform.money(h),
   'available_7d_usd',platform.money(greatest(0,cw-w-h)),'available_30d_usd',platform.money(greatest(0,cm-m-h)),
   'next_capacity_at',next_at,'blocked_reason',reason);
END $$;
CREATE FUNCTION platform.usage() RETURNS jsonb LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ SELECT platform.usage_for(public.app_user_id()) $$;
CREATE FUNCTION platform.eligible_at(p_cost bigint) RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
DECLARE u uuid:=public.app_user_id(); t timestamptz:=statement_timestamp(); cw bigint; cm bigint; result timestamptz;
BEGIN
 SELECT p.cap_7d,p.cap_30d INTO cw,cm FROM platform.membership m JOIN platform.plan p ON p.id=m.plan_id WHERE m.user_id=u AND m.status='active';
 IF NOT FOUND OR p_cost>cw OR p_cost>cm OR EXISTS(SELECT FROM platform.llm_attempt WHERE user_id=u AND state IN('dispatching','uncertain')) THEN RETURN NULL; END IF;
 SELECT min(candidate) INTO result FROM (
  SELECT t candidate UNION SELECT accounted_at+interval '168 hours' FROM platform.llm_usage WHERE user_id=u AND accounted_at>t-interval '168 hours'
  UNION SELECT accounted_at+interval '720 hours' FROM platform.llm_usage WHERE user_id=u AND accounted_at>t-interval '720 hours'
 ) times WHERE (SELECT COALESCE(sum(amount),0) FROM platform.llm_usage WHERE user_id=u AND accounted_at>candidate-interval '168 hours' AND accounted_at<=candidate)+p_cost<=cw
 AND (SELECT COALESCE(sum(amount),0) FROM platform.llm_usage WHERE user_id=u AND accounted_at>candidate-interval '720 hours' AND accounted_at<=candidate)+p_cost<=cm;
 RETURN result;
END $$;
CREATE FUNCTION platform.members(p_after uuid,p_limit integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',b.user_id,'label',COALESCE((SELECT min(subject) FROM public.identity i WHERE i.user_id=b.user_id),b.user_id::text),
   'membership',platform.membership_json(b.user_id),'usage',platform.usage_for(b.user_id)) ORDER BY b.user_id)
   FROM (SELECT user_id FROM platform.budget_account WHERE p_after IS NULL OR user_id>p_after ORDER BY user_id LIMIT least(greatest(p_limit,1),100)) b),'[]');
END $$;
CREATE FUNCTION platform.usage_entries(p_before bigint,p_limit integer) RETURNS jsonb LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
 SELECT COALESCE(jsonb_agg(jsonb_build_object('id',u.id::text,'amount_usd',platform.money(u.amount),'accounted_at',u.accounted_at,'model',a.model,'source',u.source) ORDER BY u.id DESC),'[]')
 FROM (SELECT * FROM platform.llm_usage WHERE user_id=public.app_user_id() AND (p_before IS NULL OR id<p_before) ORDER BY id DESC LIMIT least(greatest(p_limit,1),100)) u
 JOIN platform.llm_attempt a ON a.id=u.attempt_id
$$;

CREATE FUNCTION platform.keys() RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'label',label,'fingerprint',fingerprint,'active',active,'version',version,'created_at',created_at) ORDER BY created_at DESC) FROM platform.system_key),'[]');
END $$;
CREATE FUNCTION platform.add_key(p_id uuid,p_label text,p_cipher bytea,p_enc text,p_fingerprint text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 INSERT INTO platform.system_key(id,label,ciphertext,encryption_version,fingerprint) VALUES(p_id,p_label,p_cipher,p_enc,p_fingerprint);
 PERFORM platform.audit_event('key.created',p_id,jsonb_build_object('label',p_label,'fingerprint',p_fingerprint));
END $$;
CREATE FUNCTION platform.activate_key(p_id uuid,p_active boolean,p_version integer) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ DECLARE v integer; BEGIN
 PERFORM platform.assert_owner();
 PERFORM 1 FROM platform.provider_lock WHERE id FOR UPDATE;
 SELECT version INTO v FROM platform.system_key WHERE id=p_id;
 IF v IS NULL THEN RAISE EXCEPTION 'key not found' USING ERRCODE='ZA404'; END IF;
 IF v<>p_version THEN RAISE EXCEPTION 'key changed' USING ERRCODE='ZA412'; END IF;
 IF p_active THEN UPDATE platform.system_key SET active=false,version=version+1 WHERE active AND id<>p_id; END IF;
 UPDATE platform.system_key SET active=p_active,version=version+1 WHERE id=p_id;
 PERFORM platform.audit_event('key.activation',p_id,jsonb_build_object('active',p_active));
END $$;

-- Internal worker functions run only under the dedicated worker role.
CREATE FUNCTION platform.reserve(p_id uuid,p_operation uuid,p_model text,p_hash text,p_quote jsonb,p_cost bigint,p_deadline timestamptz) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
DECLARE u uuid:=public.app_user_id(); ms platform.membership; p platform.plan; k uuid; a platform.llm_attempt; t timestamptz; w numeric; m numeric; h numeric; n integer;
BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=u FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'member not found' USING ERRCODE='ZA404'; END IF;
 SELECT * INTO a FROM platform.llm_attempt WHERE operation_id=p_operation AND user_id=u AND state IN('dispatching','uncertain') LIMIT 1;
 IF FOUND THEN RETURN jsonb_build_object('reason','existing_attempt','attempt_id',a.id,'state',a.state); END IF;
 IF NOT EXISTS(SELECT FROM public.llm_operation WHERE id=p_operation AND user_id=u AND status IN('pending','blocked')) THEN
   RETURN jsonb_build_object('reason','operation_not_pending');
 END IF;
 SELECT * INTO ms FROM platform.membership WHERE user_id=u;
 IF NOT FOUND THEN RETURN jsonb_build_object('reason','membership_required'); END IF;
 IF ms.status<>'active' THEN RETURN jsonb_build_object('reason','membership_suspended'); END IF;
 SELECT * INTO p FROM platform.plan WHERE id=ms.plan_id FOR SHARE;
 t:=clock_timestamp();
 IF p_cost<0 OR p_deadline<=t THEN RAISE EXCEPTION 'invalid reservation' USING ERRCODE='ZA422'; END IF;
 SELECT COALESCE(sum(amount) FILTER(WHERE accounted_at>t-interval '168 hours'),0),COALESCE(sum(amount),0) INTO w,m
 FROM platform.llm_usage WHERE user_id=u AND accounted_at>t-interval '720 hours' AND accounted_at<=t;
 SELECT COALESCE(sum(reserved),0),count(*) INTO h,n FROM platform.llm_attempt WHERE user_id=u AND state IN('dispatching','uncertain');
 IF p_cost>p.cap_7d OR p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','request_exceeds_plan'); END IF;
 IF n>0 THEN RETURN jsonb_build_object('reason','in_flight'); END IF;
 IF w+h+p_cost>p.cap_7d OR m+h+p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','usage_limit'); END IF;
 SELECT id INTO k FROM platform.system_key WHERE active;
 IF k IS NULL THEN RETURN jsonb_build_object('reason','provider_key_missing'); END IF;
 SELECT COALESCE(max(attempt_number),0)+1 INTO n FROM platform.llm_attempt WHERE operation_id=p_operation AND user_id=u;
 INSERT INTO platform.llm_attempt(id,user_id,operation_id,attempt_number,request_hash,model,key_id,quote,reserved,dispatched_at,deadline_at,state)
 VALUES(p_id,u,p_operation,n,p_hash,p_model,k,p_quote,p_cost,t,p_deadline,'dispatching');
 UPDATE public.llm_operation SET status='running',blocked_reason='',next_eligible_at=NULL,version=version+1,updated_at=t WHERE id=p_operation AND user_id=u;
 PERFORM platform.emit(u,'usage.changed',u);
 RETURN jsonb_build_object('reason','','attempt_id',p_id,'key_id',k,'state','dispatching');
END $$;
CREATE FUNCTION platform.load_key(p_id uuid) RETURNS TABLE(ciphertext bytea,encryption_version text) LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
 SELECT k.ciphertext,k.encryption_version FROM platform.system_key k WHERE k.id=p_id
 AND EXISTS(SELECT FROM platform.llm_attempt a WHERE a.key_id=k.id AND a.user_id=public.app_user_id())
$$;
CREATE FUNCTION platform.attempts_for_operation(p_operation uuid) RETURNS jsonb LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
 SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY attempt_number DESC),'[]') FROM platform.llm_attempt a
 WHERE a.operation_id=p_operation AND a.user_id=public.app_user_id()
$$;
CREATE FUNCTION platform.uncertain(p_id uuid,p_generation text,p_reason text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ DECLARE a platform.llm_attempt; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 UPDATE platform.llm_attempt SET state='uncertain',generation_id=COALESCE(generation_id,p_generation),error_code=p_reason,version=version+1
 WHERE id=p_id AND user_id=public.app_user_id() AND state IN('dispatching','uncertain') RETURNING * INTO a;
 IF FOUND THEN
   UPDATE public.llm_operation SET status='needs_review',blocked_reason='accounting_pending',version=version+1,updated_at=now() WHERE id=a.operation_id AND user_id=a.user_id;
   PERFORM platform.emit(a.user_id,'usage.changed',a.user_id);
   PERFORM platform.emit(a.user_id,'llm_operation.changed',a.operation_id);
 END IF;
END $$;
CREATE FUNCTION platform.settle(p_id uuid,p_amount bigint,p_generation text,p_usage jsonb,p_result text,p_source text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ DECLARE a platform.llm_attempt; charged bigint; last_entry bigint; BEGIN
 PERFORM 1 FROM platform.budget_account WHERE user_id=public.app_user_id() FOR UPDATE;
 SELECT * INTO a FROM platform.llm_attempt WHERE id=p_id AND user_id=public.app_user_id() FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'attempt not found' USING ERRCODE='ZA404'; END IF;
 IF p_amount<0 THEN RAISE EXCEPTION 'invalid cost' USING ERRCODE='ZA422'; END IF;
 IF p_amount>a.reserved THEN
   PERFORM platform.audit_event('quote.exceeded',p_id,jsonb_build_object('reserved',platform.money(a.reserved),'actual',platform.money(p_amount)));
 END IF;
 IF p_generation IS NOT NULL AND a.generation_id IS NOT NULL AND p_generation<>a.generation_id THEN
   RAISE EXCEPTION 'generation does not match attempt' USING ERRCODE='ZA409';
 END IF;
 IF a.state IN('settled','uncharged') THEN
   SELECT COALESCE(sum(amount),0),max(id) INTO charged,last_entry FROM platform.llm_usage WHERE attempt_id=p_id;
   IF charged=p_amount OR p_source='confirmed_uncharged' THEN RETURN; END IF;
   INSERT INTO platform.llm_usage(attempt_id,user_id,amount,accounted_at,source,dedup_key,usage)
   VALUES(a.id,a.user_id,p_amount-charged,a.dispatched_at,p_source,'correction:'||last_entry::text||':'||p_amount::text,p_usage);
   PERFORM platform.audit_event('usage.corrected',p_id,jsonb_build_object('previous',platform.money(charged),'confirmed',platform.money(p_amount)));
   PERFORM platform.emit(a.user_id,'usage.changed',a.user_id);
   RETURN;
 END IF;
 INSERT INTO platform.llm_usage(attempt_id,user_id,amount,accounted_at,source,dedup_key,usage)
 VALUES(a.id,a.user_id,p_amount,a.dispatched_at,p_source,'settlement',p_usage);
 UPDATE platform.llm_attempt SET state=CASE WHEN p_source='confirmed_uncharged' THEN 'uncharged' ELSE 'settled' END,
 generation_id=COALESCE(generation_id,p_generation),version=version+1 WHERE id=p_id;
 UPDATE public.llm_operation SET status=CASE WHEN p_result<>'' THEN 'completed' ELSE 'failed' END,
 result=p_result,blocked_reason=CASE WHEN p_result<>'' THEN '' ELSE 'result_unavailable' END,version=version+1,updated_at=now()
 WHERE id=a.operation_id AND user_id=a.user_id;
 PERFORM platform.emit(a.user_id,'usage.changed',a.user_id);
 PERFORM platform.emit(a.user_id,'llm_operation.changed',a.operation_id);
END $$;
CREATE FUNCTION platform.pending_operations(p_limit integer) RETURNS jsonb LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
 SELECT COALESCE(jsonb_agg(jsonb_build_object('user_id',user_id,'operation_id',operation_id)),'[]')
 FROM (SELECT user_id,operation_id FROM platform.operation_index
 WHERE status IN('pending','blocked','running','needs_review') AND (next_eligible_at IS NULL OR next_eligible_at<=now())
 ORDER BY updated_at,operation_id LIMIT least(greatest(p_limit,1),1000)) x
$$;
CREATE FUNCTION platform.admin_attempts(p_before uuid,p_limit integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'user_id',user_id,'model',model,'state',state,'reserved_usd',platform.money(reserved),
   'dispatched_at',dispatched_at,'generation_id',generation_id,'error_code',error_code,'version',version) ORDER BY id DESC)
 FROM (SELECT * FROM platform.llm_attempt WHERE state='uncertain' AND (p_before IS NULL OR id<p_before) ORDER BY id DESC LIMIT least(greatest(p_limit,1),100)) a),'[]');
END $$;
CREATE FUNCTION platform.resolve_attempt(p_id uuid,p_amount bigint,p_generation text,p_reason text,p_version integer) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ DECLARE a platform.llm_attempt; BEGIN
 PERFORM platform.assert_owner();
 SELECT * INTO a FROM platform.llm_attempt WHERE id=p_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'attempt not found' USING ERRCODE='ZA404'; END IF;
 PERFORM 1 FROM platform.budget_account WHERE user_id=a.user_id FOR UPDATE;
 SELECT * INTO a FROM platform.llm_attempt WHERE id=p_id FOR UPDATE;
 IF a.version<>p_version THEN RAISE EXCEPTION 'attempt changed' USING ERRCODE='ZA412'; END IF;
 IF a.state<>'uncertain' OR a.deadline_at>clock_timestamp() THEN RAISE EXCEPTION 'attempt still active or already resolved' USING ERRCODE='ZA409'; END IF;
 IF p_generation IS NOT NULL AND a.generation_id IS NOT NULL AND p_generation<>a.generation_id THEN RAISE EXCEPTION 'generation does not match attempt' USING ERRCODE='ZA409'; END IF;
 IF p_amount<0 OR length(btrim(p_reason)) NOT BETWEEN 3 AND 1000 THEN RAISE EXCEPTION 'cost and evidence are required' USING ERRCODE='ZA422'; END IF;
 -- Do not switch the API's tenant context to access the member's operation.
 INSERT INTO platform.llm_usage(attempt_id,user_id,amount,accounted_at,source,dedup_key,usage)
 VALUES(a.id,a.user_id,p_amount,a.dispatched_at,'owner_resolution','settlement','{}');
 UPDATE platform.llm_attempt SET state='settled',generation_id=COALESCE(generation_id,p_generation),version=version+1 WHERE id=p_id;
 PERFORM platform.audit_event('attempt.resolved',p_id,jsonb_build_object('amount',platform.money(p_amount),'reason',p_reason));
 -- The worker finishes the tenant operation on its next reconciliation pass.
 PERFORM platform.emit(a.user_id,'usage.changed',a.user_id);
END $$;

REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA platform FROM PUBLIC;
-- Definers use a non-login role with explicit policies, including on hosts where
-- the migration owner cannot bypass forced RLS.
GRANT USAGE ON SCHEMA public TO zapply_platform;
GRANT USAGE, CREATE ON SCHEMA platform TO zapply_platform;
GRANT EXECUTE ON FUNCTION public.app_user_id() TO zapply_platform;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA platform TO zapply_platform;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA platform TO zapply_platform;
GRANT SELECT ON public.identity TO zapply_platform;
CREATE POLICY platform_identity ON public.identity FOR SELECT TO zapply_platform USING(true);
GRANT SELECT, UPDATE ON public.llm_operation TO zapply_platform;
CREATE POLICY platform_operation ON public.llm_operation TO zapply_platform
 USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());
GRANT INSERT ON public.outbox TO zapply_platform;
CREATE POLICY platform_events ON public.outbox FOR INSERT TO zapply_platform WITH CHECK(true);
DO $$ DECLARE f record; BEGIN
 FOR f IN SELECT p.oid::regprocedure AS signature FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='platform' LOOP
   EXECUTE format('ALTER FUNCTION %s OWNER TO zapply_platform',f.signature);
 END LOOP;
END $$;
GRANT EXECUTE ON FUNCTION platform.role(),platform.plans(),platform.save_plan(uuid,text,bigint,bigint,integer),
 platform.save_membership(uuid,uuid,text,integer),platform.usage(),platform.members(uuid,integer),platform.usage_entries(bigint,integer),
 platform.keys(),platform.add_key(uuid,text,bytea,text,text),platform.activate_key(uuid,boolean,integer),
 platform.admin_attempts(uuid,integer),platform.resolve_attempt(uuid,bigint,text,text,integer),platform.assert_owner() TO zapply_app;
GRANT EXECUTE ON FUNCTION platform.reserve(uuid,uuid,text,text,jsonb,bigint,timestamptz),platform.load_key(uuid),
 platform.attempts_for_operation(uuid),platform.uncertain(uuid,text,text),platform.settle(uuid,bigint,text,jsonb,text,text),platform.usage(),platform.eligible_at(bigint),platform.pending_operations(integer) TO zapply_worker;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER initialize_budget ON app_user;
DROP POLICY platform_identity ON identity;
DROP POLICY platform_events ON outbox;
DROP POLICY worker_insert ON outbox;
DROP TABLE llm_operation CASCADE;
DROP SCHEMA platform CASCADE;
