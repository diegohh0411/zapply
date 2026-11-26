-- The posting catalog: every posting the platform sees, stored once, with
-- labels a model reads off it (seniority, area, countries, ...), so alerts can
-- later match on what a posting is rather than on words in its title.
--
-- Nothing here is member data or member-visible. The board and search polls
-- add what they see (package catalog); delivering alerts is unchanged and
-- still reads only job_alert and job_alert_key. A backfill adds the postings
-- members were already alerted about.
--
-- A posting is one opening, whatever found it. platform.posting_identity maps
-- each of its identities to it, as job_alert_key does per member: its ATS
-- posting (ats:<ats>:<board>:<posting>), its canonical URL (url:<normalized>)
-- and its company, title and country (fp:<company|title|country>). Two ATS
-- board postings are the same opening only by their ATS key.
--
-- Labels come from two model operations per posting, each run, quoted and
-- charged by the worker like any other: a decision (seniority, area, minimum
-- years, employment type, remote) and an extraction (specialty, countries,
-- languages). They are charged to the catalog's own member, an internal
-- account on an internal plan, so classification spends no member's
-- allowance and its cost stays visible and capped (docs/parts/11-posting-catalog.md).
-- +goose Up
-- +goose StatementBegin
CREATE TABLE platform.posting (
 id uuid PRIMARY KEY,
 -- What first found it: a board poll, a search poll, or the alert backfill.
 source text NOT NULL CHECK(source IN ('ats','search','alert')),
 company text NOT NULL CHECK(length(company) <= 200),
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 500),
 location text NOT NULL DEFAULT '' CHECK(length(location) <= 500),
 url text NOT NULL CHECK(length(url) <= 2048 AND url ~ '^https://'),
 remote boolean NOT NULL DEFAULT false,
 -- The country the source gave or the location names, when known.
 country text CHECK(country ~ '^[A-Z]{2}$'),
 published_at timestamptz,
 description text CHECK(length(description) <= 20000),
 -- The source's own seniority (TheirStack's), a hint for the classifier.
 seniority_hint text CHECK(length(seniority_hint) <= 50),
 first_seen_at timestamptz NOT NULL DEFAULT now(),
 last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX posting_first_seen ON platform.posting(first_seen_at DESC);
CREATE TABLE platform.posting_identity (
 key text PRIMARY KEY CHECK(length(key) BETWEEN 1 AND 2100),
 posting_id uuid NOT NULL REFERENCES platform.posting(id) ON DELETE CASCADE,
 -- The posting holding the key came from an ATS board.
 from_ats boolean NOT NULL DEFAULT false,
 -- A fingerprint holds for 60 days from its last sighting; past that the same
 -- role posted again is a new opening and takes the key over.
 seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX posting_identity_posting ON platform.posting_identity(posting_id);
CREATE TABLE platform.posting_label (
 posting_id uuid PRIMARY KEY REFERENCES platform.posting(id) ON DELETE CASCADE,
 -- The classifier's prompt version (catalog.LabelVersion).
 version integer NOT NULL CHECK(version > 0),
 seniority text CHECK(seniority IN ('intern','entry','mid','senior','staff','manager')),
 min_years integer CHECK(min_years BETWEEN 0 AND 50),
 -- One or two areas, the primary first.
 areas text[] CHECK(cardinality(areas) BETWEEN 1 AND 2 AND areas <@ ARRAY['engineering_software','engineering_other','it','data_analytics',
  'product_design','finance_accounting','sales','marketing_comms','customer_support','operations_supply','hr','legal_compliance','healthcare',
  'education','research_science','admin','consulting_pm','skilled_trades','hospitality_retail','media_arts','ai_training','other']),
 specialty text CHECK(length(specialty) BETWEEN 1 AND 200),
 countries text[] CHECK(array_to_string(countries,',') ~ '^([A-Z]{2}(,[A-Z]{2})*)?$' AND cardinality(countries) <= 50),
 remote boolean,
 employment_type text CHECK(employment_type IN ('full_time','contract','internship','part_time')),
 languages text[] CHECK(array_to_string(languages,',') ~ '^([a-z]{2}(,[a-z]{2})*)?$' AND cardinality(languages) <= 20),
 -- Per field, how sure the model was: {"seniority":0.91,"remote":0.98,...}.
 confidence jsonb NOT NULL DEFAULT '{}',
 decision_model text, decided_at timestamptz,
 extraction_model text, extracted_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now()
);
-- Each posting's two classification stages. A stage holds its open operation
-- while one runs, counts its attempts, and is done once answered or out of
-- attempts (failed).
CREATE TABLE platform.posting_stage (
 posting_id uuid NOT NULL REFERENCES platform.posting(id) ON DELETE CASCADE,
 stage text NOT NULL CHECK(stage IN ('decision','extraction')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts >= 0),
 operation_id uuid UNIQUE,
 done boolean NOT NULL DEFAULT false,
 failed boolean NOT NULL DEFAULT false,
 PRIMARY KEY(posting_id,stage)
);
CREATE INDEX posting_stage_todo ON platform.posting_stage(stage) WHERE NOT done AND operation_id IS NULL;
-- How far the alert backfill has read each member's alerts.
CREATE TABLE platform.posting_backfill (
 user_id uuid PRIMARY KEY,
 after_id uuid,
 done boolean NOT NULL DEFAULT false,
 updated_at timestamptz NOT NULL DEFAULT now()
);
-- Board postings are cataloged once each, after the poll that first saw them.
ALTER TABLE platform.ats_posting ADD COLUMN cataloged boolean NOT NULL DEFAULT false;
CREATE INDEX ats_posting_uncataloged ON platform.ats_posting(ats,board) WHERE NOT cataloged;
-- The models that classify postings, set apart from the member-facing ones so
-- each stage can use its own. Classification is off until they are set.
ALTER TABLE platform.model_settings
 ADD COLUMN posting_decision_model text CHECK(posting_decision_model IS NULL OR length(btrim(posting_decision_model)) BETWEEN 1 AND 255),
 ADD COLUMN posting_extraction_model text CHECK(posting_extraction_model IS NULL OR length(btrim(posting_extraction_model)) BETWEEN 1 AND 255);

GRANT SELECT,INSERT,UPDATE,DELETE ON platform.posting,platform.posting_identity,platform.posting_label,platform.posting_stage,platform.posting_backfill TO zapply_platform;
GRANT SELECT,INSERT,UPDATE ON platform.posting,platform.posting_identity,platform.posting_label,platform.posting_stage,platform.posting_backfill TO zapply_worker;

-- The catalog's member: an internal account on an internal plan, which pays
-- for classification. The owner can see and change its plan's caps.
ALTER TABLE app_user NO FORCE ROW LEVEL SECURITY;
INSERT INTO app_user(id) VALUES('00000000-0000-7000-8000-0000000ca7a1') ON CONFLICT DO NOTHING;
ALTER TABLE app_user FORCE ROW LEVEL SECURITY;
INSERT INTO platform.budget_account(user_id) VALUES('00000000-0000-7000-8000-0000000ca7a1') ON CONFLICT DO NOTHING;
-- $10 a week, $30 a month (picodollars).
INSERT INTO platform.plan(id,name,cap_7d,cap_30d) VALUES('00000000-0000-7000-8000-0000000ca7a2','Internal: posting catalog',10000000000000,30000000000000)
 ON CONFLICT DO NOTHING;
INSERT INTO platform.membership(user_id,plan_id,status) VALUES('00000000-0000-7000-8000-0000000ca7a1','00000000-0000-7000-8000-0000000ca7a2','active')
 ON CONFLICT DO NOTHING;

-- The migration login can SET ROLE but does not inherit platform function ownership.
SET LOCAL ROLE zapply_platform;
CREATE FUNCTION platform.posting_models() RETURNS TABLE(decision text,extraction text) LANGUAGE sql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT posting_decision_model,posting_extraction_model FROM platform.model_settings WHERE id
$$;
CREATE FUNCTION platform.save_posting_models(p_decision text,p_extraction text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.model_settings SET posting_decision_model=p_decision,posting_extraction_model=p_extraction WHERE id;
 PERFORM platform.audit_event('model_settings.saved',public.app_user_id(),
  jsonb_build_object('posting_decision_model',p_decision,'posting_extraction_model',p_extraction));
END $$;
-- A board's postings not yet in the catalog, oldest first, with the company
-- its watchers know the board by.
CREATE FUNCTION platform.ats_postings_uncataloged(p_ats text,p_board text,p_limit int) RETURNS jsonb LANGUAGE sql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object(
  'company',COALESCE((SELECT min(company) FROM platform.job_watch_index WHERE ats=p_ats AND board=p_board),''),
  'postings',COALESCE((SELECT jsonb_agg(jsonb_build_object('key',posting_key,'title',title,'location',location,'url',url,'remote',remote,'published_at',published_at))
   FROM (SELECT * FROM platform.ats_posting WHERE ats=p_ats AND board=p_board AND NOT cataloged ORDER BY first_seen_at,posting_key
    LIMIT least(greatest(p_limit,1),5000)) p),'[]'::jsonb))
$$;
CREATE FUNCTION platform.ats_postings_cataloged(p_ats text,p_board text,p_keys text[]) RETURNS void LANGUAGE sql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
 UPDATE platform.ats_posting SET cataloged=true WHERE ats=p_ats AND board=p_board AND posting_key=ANY(p_keys) AND NOT cataloged
$$;
-- Members who watch anything, in id order, for the alert backfill. Every
-- alert belongs to a watch, so this covers every member with alerts.
CREATE FUNCTION platform.watching_members(p_after uuid,p_limit int) RETURNS uuid[] LANGUAGE sql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT COALESCE(array_agg(user_id ORDER BY user_id),'{}') FROM (
  SELECT user_id FROM (SELECT user_id FROM platform.job_watch_index UNION SELECT user_id FROM platform.search_watch) w
  WHERE p_after IS NULL OR user_id > p_after ORDER BY user_id LIMIT least(greatest(p_limit,1),1000)) m
$$;
REVOKE EXECUTE ON FUNCTION platform.posting_models(),platform.save_posting_models(text,text),platform.ats_postings_uncataloged(text,text,int),
 platform.ats_postings_cataloged(text,text,text[]),platform.watching_members(uuid,int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.posting_models(),platform.ats_postings_uncataloged(text,text,int),platform.ats_postings_cataloged(text,text,text[]),
 platform.watching_members(uuid,int) TO zapply_worker;
GRANT EXECUTE ON FUNCTION platform.posting_models(),platform.save_posting_models(text,text) TO zapply_app;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.watching_members(uuid,int),platform.ats_postings_cataloged(text,text,text[]),platform.ats_postings_uncataloged(text,text,int),
 platform.save_posting_models(text,text),platform.posting_models();
ALTER TABLE platform.model_settings DROP COLUMN posting_extraction_model,DROP COLUMN posting_decision_model;
DROP INDEX platform.ats_posting_uncataloged;
ALTER TABLE platform.ats_posting DROP COLUMN cataloged;
DROP TABLE platform.posting_backfill,platform.posting_stage,platform.posting_label,platform.posting_identity,platform.posting;
-- The catalog's member stays: its charges and ledger entries reference it.
-- +goose StatementEnd
