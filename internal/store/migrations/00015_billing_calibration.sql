-- Decision reservations predict cost from request size. This summarises how a
-- model's recent settlements compared with their quotes so the prediction
-- learns per model. Settlements are cross-tenant; only aggregates leave here.
-- Attempts quoted before shapes were recorded cannot calibrate and are skipped.
-- +goose Up
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;
CREATE FUNCTION platform.decision_calibration(p_model text) RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
 WITH recent AS (
  SELECT a.dispatched_at,a.reserved,
   (a.quote->'shape'->>'request_bytes')::numeric AS bytes,
   (a.quote->'shape'->>'questions')::numeric AS questions,
   (a.quote->'prediction'->>'expected')::numeric AS expected,
   sum(u.amount) AS amount,
   (array_agg(u.usage ORDER BY u.id))[1] AS usage
  FROM platform.model_attempt a JOIN platform.model_usage u ON u.attempt_id=a.id
  WHERE a.model=p_model AND a.state='settled' AND jsonb_typeof(a.quote->'shape')='object'
   AND a.dispatched_at>now()-interval '30 days'
  GROUP BY a.id ORDER BY a.dispatched_at DESC LIMIT 200
 ), samples AS (
  SELECT bytes,questions,expected,amount,(usage->>'input_tokens')::numeric AS input,
   COALESCE((usage->>'output_tokens')::numeric,0) AS output
  FROM recent
  WHERE bytes>0 AND questions>0 AND expected>0 AND amount>0
   AND jsonb_typeof(usage->'input_tokens')='number'
   AND COALESCE(jsonb_typeof(usage->'output_tokens'),'number')='number'
 )
 SELECT jsonb_build_object(
  'samples',(SELECT count(*) FROM samples),
  'tokens_per_kb',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY input*1000/bytes)),0) FROM samples),
  'output_per_q',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY output/questions)),0) FROM samples),
  'cost_permil',(SELECT COALESCE(ceil(percentile_disc(0.95) WITHIN GROUP (ORDER BY amount*1000/expected)),0) FROM samples),
  -- Any overrun among the latest 20 settlements forces the context ceiling.
  'exceeded',(SELECT COALESCE(bool_or(amount>reserved),false) FROM (SELECT amount,reserved FROM recent ORDER BY dispatched_at DESC LIMIT 20) latest))
$$;
REVOKE EXECUTE ON FUNCTION platform.decision_calibration(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.decision_calibration(text) TO zapply_app,zapply_worker;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.decision_calibration(text);
-- +goose StatementEnd
