-- Shared accounting covers text and decision models. Preserve applied migration
-- history and rename the existing objects without copying or replacing data.
-- +goose Up
-- +goose StatementBegin
ALTER TABLE public.llm_operation RENAME TO model_operation;
ALTER TABLE public.llm_request RENAME TO model_request;
ALTER TABLE platform.llm_attempt RENAME TO model_attempt;
ALTER TABLE platform.llm_usage RENAME TO model_usage;
ALTER SEQUENCE platform.llm_usage_id_seq RENAME TO model_usage_id_seq;
DO $$ DECLARE obj record; BEGIN
 FOR obj IN SELECT n.nspname,t.relname,c.conname FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname IN ('public','platform') AND c.conname LIKE 'llm\_%' ESCAPE '\' LOOP
 EXECUTE format('ALTER TABLE %I.%I RENAME CONSTRAINT %I TO %I',obj.nspname,obj.relname,obj.conname,replace(obj.conname,'llm_','model_'));
 END LOOP;
END $$;
-- SQL/PLpgSQL source strings are not rewritten automatically by table renames.
-- Use the existing function owner, including on non-superuser deployments.
SET LOCAL ROLE zapply_platform;
DO $$ DECLARE f record; BEGIN
 FOR f IN SELECT pg_get_functiondef(p.oid) body FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='platform' AND p.prokind='f' AND pg_get_functiondef(p.oid) LIKE '%llm\_%' ESCAPE '\' LOOP
 EXECUTE replace(f.body,'llm_','model_');
 END LOOP;
END $$;
RESET ROLE;
-- Old queue kinds remain readable by the compatibility worker. New jobs and
-- events use model_operation; historical outbox entries retain their names.
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;
DO $$ DECLARE f record; BEGIN
 FOR f IN SELECT pg_get_functiondef(p.oid) body FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='platform' AND p.prokind='f' AND (pg_get_functiondef(p.oid) LIKE '%model_operation%' OR pg_get_functiondef(p.oid) LIKE '%model_request%' OR pg_get_functiondef(p.oid) LIKE '%model_attempt%' OR pg_get_functiondef(p.oid) LIKE '%model_usage%') LOOP
 EXECUTE replace(replace(replace(replace(f.body,'model_operation','llm_operation'),'model_request','llm_request'),'model_attempt','llm_attempt'),'model_usage','llm_usage');
 END LOOP;
END $$;
RESET ROLE;
DO $$ DECLARE obj record; BEGIN
 FOR obj IN SELECT n.nspname,t.relname,c.conname FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace WHERE n.nspname IN ('public','platform') AND (c.conname LIKE 'model_operation%' OR c.conname LIKE 'model_request%' OR c.conname LIKE 'model_attempt%' OR c.conname LIKE 'model_usage%') LOOP
 EXECUTE format('ALTER TABLE %I.%I RENAME CONSTRAINT %I TO %I',obj.nspname,obj.relname,obj.conname,replace(obj.conname,'model_','llm_'));
 END LOOP;
END $$;
ALTER SEQUENCE platform.model_usage_id_seq RENAME TO llm_usage_id_seq;
ALTER TABLE platform.model_usage RENAME TO llm_usage;
ALTER TABLE platform.model_attempt RENAME TO llm_attempt;
ALTER TABLE public.model_request RENAME TO llm_request;
ALTER TABLE public.model_operation RENAME TO llm_operation;
-- +goose StatementEnd
