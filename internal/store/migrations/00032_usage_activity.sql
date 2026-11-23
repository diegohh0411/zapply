-- Usage entries group charges by the work they paid for, not by each model call:
-- a CV summary with its revisions, a match run, or an email's classification is
-- one line, labelled with its job (or the email's app when no job is linked).
-- +goose Up
-- +goose StatementBegin
-- The function runs as zapply_platform, which reads the job-side tables through
-- read policies scoped to the caller, like the other platform functions do.
GRANT SELECT ON public.model_operation, public.cv_summary, public.match_run, public.match_run_operation, public.mail_operation, public.mail_message, public.job TO zapply_platform;
CREATE POLICY platform_usage_match_run ON public.match_run FOR SELECT TO zapply_platform USING (user_id = public.app_user_id());
CREATE POLICY platform_usage_match_run_operation ON public.match_run_operation FOR SELECT TO zapply_platform USING (user_id = public.app_user_id());
CREATE POLICY platform_usage_mail_message ON public.mail_message FOR SELECT TO zapply_platform USING (user_id = public.app_user_id());
SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.usage_entries(p_before bigint,p_limit integer) RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
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
$$;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY platform_usage_mail_message ON public.mail_message;
DROP POLICY platform_usage_match_run_operation ON public.match_run_operation;
DROP POLICY platform_usage_match_run ON public.match_run;
SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.usage_entries(p_before bigint,p_limit integer) RETURNS jsonb LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$
 SELECT COALESCE(jsonb_agg(jsonb_build_object('id',u.id::text,'amount_usd',platform.money(u.amount),'accounted_at',u.accounted_at,'model',a.model,'source',u.source) ORDER BY u.id DESC),'[]')
 FROM (SELECT * FROM platform.model_usage WHERE user_id=public.app_user_id() AND (p_before IS NULL OR id<p_before) ORDER BY id DESC LIMIT least(greatest(p_limit,1),100)) u
 JOIN platform.model_attempt a ON a.id=u.attempt_id
$$;
RESET ROLE;
-- +goose StatementEnd
