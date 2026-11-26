-- The posting catalog's state for the owner's admin page: how many postings
-- there are, where each classification stage stands, what the catalog's
-- member has spent against its plan, and the newest labels to spot-check
-- (docs/parts/11-posting-catalog.md).
-- +goose Up
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;
CREATE FUNCTION platform.posting_catalog(p_limit int) RETURNS jsonb LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE result jsonb;
BEGIN
 PERFORM platform.assert_owner();
 SELECT jsonb_build_object(
  'postings',(SELECT count(*) FROM platform.posting),
  'postings_24h',(SELECT count(*) FROM platform.posting WHERE first_seen_at>statement_timestamp()-interval '24 hours'),
  -- Per posting: both stages answered, one still to do, one given up on.
  'labeled',(SELECT count(*) FROM (SELECT posting_id FROM platform.posting_stage GROUP BY posting_id
    HAVING bool_and(done AND NOT failed)) l),
  'waiting',(SELECT count(DISTINCT posting_id) FROM platform.posting_stage WHERE NOT done),
  'failed',(SELECT count(DISTINCT posting_id) FROM platform.posting_stage WHERE failed),
  'stages',(SELECT jsonb_object_agg(stage,counts) FROM (SELECT st.stage,jsonb_build_object(
    'done',count(s.posting_id) FILTER(WHERE s.done AND NOT s.failed),
    'waiting',count(s.posting_id) FILTER(WHERE NOT s.done AND s.operation_id IS NULL),
    'running',count(s.posting_id) FILTER(WHERE NOT s.done AND s.operation_id IS NOT NULL),
    'failed',count(s.posting_id) FILTER(WHERE s.failed)) counts
   FROM (VALUES('decision'),('extraction')) st(stage) LEFT JOIN platform.posting_stage s ON s.stage=st.stage GROUP BY st.stage) c),
  'usage',platform.usage_for('00000000-0000-7000-8000-0000000ca7a1'),
  'recent',COALESCE((SELECT jsonb_agg(jsonb_build_object(
    'id',p.id,'title',p.title,'company',p.company,'location',p.location,
    'seniority',l.seniority,'min_years',l.min_years,'areas',COALESCE(to_jsonb(l.areas),'[]'),'specialty',l.specialty,
    'countries',COALESCE(to_jsonb(l.countries),'[]'),'remote',l.remote,'employment_type',l.employment_type,
    'languages',COALESCE(to_jsonb(l.languages),'[]'),'confidence',l.confidence,
    'decision_model',l.decision_model,'extraction_model',l.extraction_model,'updated_at',l.updated_at) ORDER BY l.updated_at DESC,p.id)
   FROM (SELECT * FROM platform.posting_label ORDER BY updated_at DESC,posting_id LIMIT least(greatest(p_limit,1),50)) l
   JOIN platform.posting p ON p.id=l.posting_id),'[]'))
 INTO result;
 RETURN result;
END $$;
REVOKE EXECUTE ON FUNCTION platform.posting_catalog(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.posting_catalog(int) TO zapply_app;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.posting_catalog(int);
-- +goose StatementEnd
