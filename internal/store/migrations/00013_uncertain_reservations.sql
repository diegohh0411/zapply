-- Uncertain calls retain their reservations without blocking unrelated requests.
-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform.usage_for(p_user uuid) RETURNS jsonb LANGUAGE plpgsql STABLE
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
END $$;
CREATE OR REPLACE FUNCTION platform.eligible_at(p_cost bigint) RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
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
END $$;
CREATE OR REPLACE FUNCTION platform.reserve(p_id uuid,p_operation uuid,p_model text,p_hash text,p_quote jsonb,p_cost bigint,p_deadline timestamptz) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
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
 IF n>0 THEN RETURN jsonb_build_object('reason','in_flight'); END IF;
 IF w+h+p_cost>p.cap_7d OR m+h+p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','usage_limit'); END IF;
 SELECT id INTO k FROM platform.system_key WHERE active;
 IF k IS NULL THEN RETURN jsonb_build_object('reason','provider_key_missing'); END IF;
 SELECT COALESCE(max(attempt_number),0)+1 INTO n FROM platform.model_attempt WHERE operation_id=p_operation AND user_id=u;
 INSERT INTO platform.model_attempt(id,user_id,operation_id,attempt_number,request_hash,model,key_id,quote,reserved,dispatched_at,deadline_at,state)
 VALUES(p_id,u,p_operation,n,p_hash,p_model,k,p_quote,p_cost,t,p_deadline,'dispatching');
 UPDATE public.model_operation SET status='running',blocked_reason='',next_eligible_at=NULL,version=version+1,updated_at=t WHERE id=p_operation AND user_id=u;
 PERFORM platform.emit(u,'usage.changed',u);
 RETURN jsonb_build_object('reason','','attempt_id',p_id,'key_id',k,'state','dispatching');
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform.usage_for(p_user uuid) RETURNS jsonb LANGUAGE plpgsql STABLE
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
 INTO w,m FROM platform.model_usage WHERE user_id=p_user AND accounted_at>t-interval '720 hours' AND accounted_at<=t;
 SELECT COALESCE(sum(reserved),0) INTO h FROM platform.model_attempt WHERE user_id=p_user AND state IN ('dispatching','uncertain');
 SELECT min(expiry) INTO next_at FROM (
   SELECT accounted_at+interval '168 hours' expiry FROM platform.model_usage WHERE user_id=p_user AND amount>0 AND accounted_at>t-interval '168 hours'
   UNION ALL SELECT accounted_at+interval '720 hours' FROM platform.model_usage WHERE user_id=p_user AND amount>0 AND accounted_at>t-interval '720 hours'
 ) x;
 IF reason='' AND EXISTS(SELECT FROM platform.model_attempt WHERE user_id=p_user AND state='uncertain') THEN reason:='accounting_pending'; END IF;
 IF reason='' AND EXISTS(SELECT FROM platform.model_attempt WHERE user_id=p_user AND state='dispatching') THEN reason:='in_flight'; END IF;
 IF reason='' AND (w::numeric+h>=cw OR m::numeric+h>=cm) THEN reason:='usage_limit'; END IF;
 RETURN jsonb_build_object('membership',member,'server_time',t,'cap_7d_usd',platform.money(cw),'cap_30d_usd',platform.money(cm),
   'spent_7d_usd',platform.money(w),'spent_30d_usd',platform.money(m),'reserved_usd',platform.money(h),
   'available_7d_usd',platform.money(greatest(0,cw-w-h)),'available_30d_usd',platform.money(greatest(0,cm-m-h)),
   'next_capacity_at',next_at,'blocked_reason',reason);
END $$;
CREATE OR REPLACE FUNCTION platform.eligible_at(p_cost bigint) RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
DECLARE u uuid:=public.app_user_id(); t timestamptz:=statement_timestamp(); cw bigint; cm bigint; result timestamptz;
BEGIN
 SELECT p.cap_7d,p.cap_30d INTO cw,cm FROM platform.membership m JOIN platform.plan p ON p.id=m.plan_id WHERE m.user_id=u AND m.status='active';
 IF NOT FOUND OR p_cost>cw OR p_cost>cm OR EXISTS(SELECT FROM platform.model_attempt WHERE user_id=u AND state IN('dispatching','uncertain')) THEN RETURN NULL; END IF;
 SELECT min(candidate) INTO result FROM (
  SELECT t candidate UNION SELECT accounted_at+interval '168 hours' FROM platform.model_usage WHERE user_id=u AND accounted_at>t-interval '168 hours'
  UNION SELECT accounted_at+interval '720 hours' FROM platform.model_usage WHERE user_id=u AND accounted_at>t-interval '720 hours'
 ) times WHERE (SELECT COALESCE(sum(amount),0) FROM platform.model_usage WHERE user_id=u AND accounted_at>candidate-interval '168 hours' AND accounted_at<=candidate)+p_cost<=cw
 AND (SELECT COALESCE(sum(amount),0) FROM platform.model_usage WHERE user_id=u AND accounted_at>candidate-interval '720 hours' AND accounted_at<=candidate)+p_cost<=cm;
 RETURN result;
END $$;
CREATE OR REPLACE FUNCTION platform.reserve(p_id uuid,p_operation uuid,p_model text,p_hash text,p_quote jsonb,p_cost bigint,p_deadline timestamptz) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
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
 SELECT COALESCE(sum(reserved),0),count(*) INTO h,n FROM platform.model_attempt WHERE user_id=u AND state IN('dispatching','uncertain');
 IF p_cost>p.cap_7d OR p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','request_exceeds_plan'); END IF;
 IF n>0 THEN RETURN jsonb_build_object('reason','in_flight'); END IF;
 IF w+h+p_cost>p.cap_7d OR m+h+p_cost>p.cap_30d THEN RETURN jsonb_build_object('reason','usage_limit'); END IF;
 SELECT id INTO k FROM platform.system_key WHERE active;
 IF k IS NULL THEN RETURN jsonb_build_object('reason','provider_key_missing'); END IF;
 SELECT COALESCE(max(attempt_number),0)+1 INTO n FROM platform.model_attempt WHERE operation_id=p_operation AND user_id=u;
 INSERT INTO platform.model_attempt(id,user_id,operation_id,attempt_number,request_hash,model,key_id,quote,reserved,dispatched_at,deadline_at,state)
 VALUES(p_id,u,p_operation,n,p_hash,p_model,k,p_quote,p_cost,t,p_deadline,'dispatching');
 UPDATE public.model_operation SET status='running',blocked_reason='',next_eligible_at=NULL,version=version+1,updated_at=t WHERE id=p_operation AND user_id=u;
 PERFORM platform.emit(u,'usage.changed',u);
 RETURN jsonb_build_object('reason','','attempt_id',p_id,'key_id',k,'state','dispatching');
END $$;
-- +goose StatementEnd
