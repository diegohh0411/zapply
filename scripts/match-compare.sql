-- Compares two finished match runs of the same job, typically pipeline 1 and
-- pipeline 2. Read-only; run with the schema owner's URL, for example:
--
--   PGOPTIONS="-c default_transaction_read_only=on" psql "$MIGRATE_DATABASE_URL" \
--     -v a=<run uuid> -v b=<run uuid> -f scripts/match-compare.sql
--
-- Start the pipeline 1 run with POST /v1/jobs/{id}/match-runs?pipeline=1.
\set ON_ERROR_STOP on
\pset pager off

\echo 'Runs'
SELECT r.id, r.pipeline, r.status, round(r.score::numeric, 1) AS score, r.must_met || '/' || r.must_total AS must,
  r.total AS questions, r.cached,
  (SELECT count(*) FROM match_run_operation m WHERE m.run_id = r.id) AS requests,
  (SELECT round(COALESCE(sum(u.amount), 0) / 1e12, 6) FROM match_run_operation m
     JOIN platform.model_attempt t ON t.operation_id = m.operation_id
     JOIN platform.model_usage u ON u.attempt_id = t.id WHERE m.run_id = r.id) AS cost_usd,
  r.finished_at - r.created_at AS duration
FROM match_run r WHERE r.id IN (:'a', :'b') ORDER BY r.id = :'b';

\echo 'Requirements (both runs)'
WITH ra AS (SELECT c FROM match_run, jsonb_array_elements(result->'requirements') c WHERE id = :'a'),
     rb AS (SELECT c FROM match_run, jsonb_array_elements(result->'requirements') c WHERE id = :'b')
SELECT left(COALESCE(ra.c->>'text', rb.c->>'text'), 70) AS requirement,
  ra.c->>'class' AS class_a, rb.c->>'class' AS class_b,
  ra.c->>'status' AS status_a, rb.c->>'status' AS status_b,
  round((ra.c->>'strength')::numeric, 2) AS strength_a, round((rb.c->>'strength')::numeric, 2) AS strength_b,
  left(ra.c->>'evidence_title', 30) AS evidence_a, left(rb.c->>'evidence_title', 30) AS evidence_b
FROM ra FULL JOIN rb ON ra.c->>'text_hash' = rb.c->>'text_hash'
ORDER BY (ra.c->>'status') IS DISTINCT FROM (rb.c->>'status') DESC, requirement;

\echo 'Agreement'
WITH ra AS (SELECT c->>'text_hash' h, c->>'status' s, c->>'evidence_id' e FROM match_run, jsonb_array_elements(result->'requirements') c WHERE id = :'a'),
     rb AS (SELECT c->>'text_hash' h, c->>'status' s, c->>'evidence_id' e FROM match_run, jsonb_array_elements(result->'requirements') c WHERE id = :'b'),
     sa AS (SELECT DISTINCT x->>'evidence_id' e FROM match_run, jsonb_array_elements((result->'selection') || (result->'suggestions')) x WHERE id = :'a'),
     sb AS (SELECT DISTINCT x->>'evidence_id' e FROM match_run, jsonb_array_elements((result->'selection') || (result->'suggestions')) x WHERE id = :'b')
SELECT
  (SELECT count(*) FROM ra JOIN rb USING (h)) AS shared_requirements,
  (SELECT count(*) FROM ra JOIN rb USING (h) WHERE ra.s = rb.s) AS same_status,
  (SELECT count(*) FROM ra JOIN rb USING (h) WHERE ra.e IS NOT DISTINCT FROM rb.e) AS same_evidence,
  (SELECT count(*) FROM ra WHERE h NOT IN (SELECT h FROM rb)) AS only_a,
  (SELECT count(*) FROM rb WHERE h NOT IN (SELECT h FROM ra)) AS only_b,
  (SELECT count(*) FROM sa JOIN sb USING (e)) AS selected_both,
  (SELECT count(*) FROM (SELECT e FROM sa UNION SELECT e FROM sb) u) AS selected_either;

\echo 'Selection differences'
WITH sa AS (SELECT x->>'evidence_id' e, x->>'title' t, (x->>'relevance')::numeric r FROM match_run, jsonb_array_elements((result->'selection') || (result->'suggestions')) x WHERE id = :'a'),
     sb AS (SELECT x->>'evidence_id' e, x->>'title' t, (x->>'relevance')::numeric r FROM match_run, jsonb_array_elements((result->'selection') || (result->'suggestions')) x WHERE id = :'b')
SELECT left(COALESCE(sa.t, sb.t), 50) AS evidence, round(sa.r, 2) AS relevance_a, round(sb.r, 2) AS relevance_b
FROM sa FULL JOIN sb USING (e) WHERE sa.e IS NULL OR sb.e IS NULL ORDER BY evidence;
