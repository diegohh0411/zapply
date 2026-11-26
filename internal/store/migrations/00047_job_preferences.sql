-- Member job preferences (docs/parts/12-job-preferences.md): what each member
-- is looking for, derived from their Experience library and editable. Each
-- field group records whether it follows the derivation ('derived') or the
-- member set it ('member'); re-deriving only writes 'derived' groups.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE job_preference (
    user_id             uuid PRIMARY KEY REFERENCES app_user (id) ON DELETE CASCADE,
    seniority_min       text CHECK (seniority_min IN ('intern','entry','mid','senior','staff','manager')),
    seniority_max       text CHECK (seniority_max IN ('intern','entry','mid','senior','staff','manager')),
    seniority_source    text NOT NULL DEFAULT 'derived' CHECK (seniority_source IN ('derived','member')),
    areas               text[] NOT NULL DEFAULT '{}' CHECK (cardinality(areas) <= 22),
    areas_source        text NOT NULL DEFAULT 'derived' CHECK (areas_source IN ('derived','member')),
    countries           text[] NOT NULL DEFAULT '{}' CHECK (cardinality(countries) <= 50),
    remote_ok           boolean NOT NULL DEFAULT true,
    location_source     text NOT NULL DEFAULT 'derived' CHECK (location_source IN ('derived','member')),
    excluded_employment text[] NOT NULL DEFAULT '{}'
        CHECK (excluded_employment <@ ARRAY['full_time','contract','internship','part_time']::text[]),
    employment_source   text NOT NULL DEFAULT 'derived' CHECK (employment_source IN ('derived','member')),
    -- The last derivation, whatever the sources: work years and each field's suggestion.
    derived             jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(derived) = 'object'),
    derived_at          timestamptz,
    -- The area question's input, so an unchanged library asks no model again.
    area_input_hash     text NOT NULL DEFAULT '',
    area_operation      uuid,
    stale               boolean NOT NULL DEFAULT true,
    changed_at          timestamptz NOT NULL DEFAULT now(),
    version             integer NOT NULL DEFAULT 1,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK ((seniority_min IS NULL) = (seniority_max IS NULL))
);
CREATE INDEX job_preference_stale ON job_preference (changed_at) WHERE stale;
ALTER TABLE job_preference ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_preference FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON job_preference TO zapply_app, zapply_worker
    USING (user_id = app_user_id()) WITH CHECK (user_id = app_user_id());
GRANT SELECT, INSERT, UPDATE ON job_preference TO zapply_app, zapply_worker;
GRANT SELECT, UPDATE ON job_preference TO zapply_platform;
CREATE POLICY platform_scan ON job_preference TO zapply_platform USING (true);

SET LOCAL ROLE zapply_platform;
-- A library change marks the member's preferences for re-derivation.
CREATE FUNCTION platform.invalidate_job_preference() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $$ BEGIN
 IF NEW.topic LIKE 'evidence.%' OR NEW.topic LIKE 'skill.%' OR NEW.topic LIKE 'organization.%' OR NEW.topic LIKE 'profile.%' THEN
  UPDATE job_preference SET stale=true,changed_at=now() WHERE user_id=NEW.user_id;
 END IF;
 RETURN NEW;
END $$;
-- Members whose preferences wait for re-derivation, after a quiet period so
-- a burst of edits derives once.
CREATE FUNCTION platform.stale_job_preferences(p_limit int) RETURNS uuid[] LANGUAGE sql SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT COALESCE(array_agg(user_id),'{}') FROM (SELECT user_id FROM job_preference
  WHERE stale AND changed_at < now()-interval '5 seconds' ORDER BY changed_at LIMIT least(greatest(p_limit,1),500)) s
$$;
REVOKE EXECUTE ON FUNCTION platform.invalidate_job_preference(),platform.stale_job_preferences(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.stale_job_preferences(int) TO zapply_worker;
RESET ROLE;
CREATE TRIGGER invalidate_job_preference AFTER INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION platform.invalidate_job_preference();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER invalidate_job_preference ON outbox;
DROP FUNCTION platform.stale_job_preferences(int),platform.invalidate_job_preference();
DROP TABLE job_preference;
-- +goose StatementEnd
