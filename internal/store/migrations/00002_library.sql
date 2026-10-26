-- Library: profile, organizations, evidence (with highlights and skills),
-- the outbox and idempotency keys. Design: docs/parts/02-library.md.
--
-- Every tenant table carries user_id with forced RLS, and references between
-- tenant rows are composite (user_id, id) so nothing can point across users.

-- +goose Up
CREATE TYPE evidence_category AS ENUM ('education', 'paid', 'unpaid');
CREATE TYPE evidence_form AS ENUM ('engagement', 'credential', 'output', 'recognition');
CREATE TYPE date_precision AS ENUM ('year', 'month', 'day');
CREATE TYPE organization_type AS ENUM ('company', 'education', 'nonprofit', 'government', 'client', 'other');
CREATE TYPE skill_type AS ENUM ('hard', 'tool', 'soft', 'language');

-- Fixed catalog of kinds (global, read-only). Field schemas for `details` live
-- in Go (internal/library/kinds.go); a test keeps the two in sync.
CREATE TABLE evidence_kind (
    kind       text PRIMARY KEY,
    form       evidence_form NOT NULL,
    categories evidence_category[] NOT NULL,
    child_only boolean NOT NULL DEFAULT false,
    UNIQUE (kind, form)
);
REVOKE INSERT, UPDATE, DELETE ON evidence_kind FROM zapply_app;

INSERT INTO evidence_kind (kind, form, categories, child_only) VALUES
    ('degree_program',   'engagement',  '{education}',             false),
    ('course',           'engagement',  '{education}',             false),
    ('job',              'engagement',  '{paid}',                  false),
    ('freelance',        'engagement',  '{paid}',                  false),
    ('business',         'engagement',  '{paid}',                  false),
    ('internship',       'engagement',  '{paid,unpaid,education}', false),
    ('apprenticeship',   'engagement',  '{paid,education}',        false),
    ('military_service', 'engagement',  '{paid}',                  false),
    ('volunteering',     'engagement',  '{unpaid}',                false),
    ('membership',       'engagement',  '{unpaid,education}',      false),
    ('career_break',     'engagement',  '{unpaid}',                false),
    ('assignment',       'engagement',  '{education,paid,unpaid}', true),
    ('license',          'credential',  '{education}',             false),
    ('certification',    'credential',  '{education}',             false),
    ('clearance',        'credential',  '{education}',             false),
    ('project',          'output',      '{education,paid,unpaid}', false),
    ('publication',      'output',      '{education,paid,unpaid}', false),
    ('presentation',     'output',      '{education,paid,unpaid}', false),
    ('patent',           'output',      '{education,paid,unpaid}', false),
    ('creative_work',    'output',      '{education,paid,unpaid}', false),
    ('award',            'recognition', '{education,paid,unpaid}', false),
    ('funding',          'recognition', '{education,paid,unpaid}', false);

CREATE TABLE profile (
    user_id    uuid PRIMARY KEY REFERENCES app_user (id) ON DELETE CASCADE,
    full_name  text NOT NULL DEFAULT '',
    headline   text NOT NULL DEFAULT '',
    email      text NOT NULL DEFAULT '',
    phone      text NOT NULL DEFAULT '',
    location   text NOT NULL DEFAULT '',
    links      jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(links) = 'array'),
    summary    text NOT NULL DEFAULT '',
    version    integer NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE organization (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    name            text NOT NULL CHECK (name <> ''),
    normalized_name text NOT NULL,
    type            organization_type NOT NULL DEFAULT 'other',
    website         text NOT NULL DEFAULT '',
    location        text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id),
    UNIQUE (user_id, normalized_name)
);

CREATE TABLE evidence (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    parent_id       uuid,
    organization_id uuid,
    category        evidence_category NOT NULL,
    form            evidence_form NOT NULL,
    kind            text NOT NULL,
    title           text NOT NULL CHECK (title <> ''),
    summary         text NOT NULL DEFAULT '',
    location        text NOT NULL DEFAULT '',
    url             text NOT NULL DEFAULT '',
    start_date      date,
    start_precision date_precision,
    end_date        date,
    end_precision   date_precision,
    is_current      boolean NOT NULL DEFAULT false,
    details         jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(details) = 'object'),
    auto_include    boolean NOT NULL DEFAULT true,
    position        text COLLATE "C" NOT NULL,
    version         integer NOT NULL DEFAULT 1,
    content_hash    bytea NOT NULL,
    archived_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id),
    FOREIGN KEY (kind, form) REFERENCES evidence_kind (kind, form),
    FOREIGN KEY (user_id, parent_id) REFERENCES evidence (user_id, id) ON DELETE CASCADE,
    FOREIGN KEY (user_id, organization_id) REFERENCES organization (user_id, id),
    CHECK ((start_date IS NULL) = (start_precision IS NULL)),
    CHECK ((end_date IS NULL) = (end_precision IS NULL)),
    CHECK (end_date IS NULL OR start_date IS NOT NULL),
    CHECK (NOT is_current OR end_date IS NULL),
    CHECK (NOT is_current OR form = 'engagement'),
    CHECK (form <> 'engagement' OR start_date IS NOT NULL)
);
CREATE INDEX evidence_list ON evidence (user_id, category, position);
CREATE INDEX evidence_children ON evidence (user_id, parent_id, position) WHERE parent_id IS NOT NULL;
CREATE INDEX evidence_organization ON evidence (user_id, organization_id) WHERE organization_id IS NOT NULL;

-- Backstop for rules the API also validates (with friendlier errors):
-- category allowed for the kind, and nesting (depth 2, engagement parents,
-- output/recognition/assignment children, same category as the parent).
-- Errors use SQLSTATE ZA001 so the API can map them to 422.
-- +goose StatementBegin
CREATE FUNCTION evidence_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    k evidence_kind;
    p evidence;
BEGIN
    SELECT * INTO k FROM evidence_kind WHERE kind = NEW.kind;
    IF NOT NEW.category = ANY (k.categories) THEN
        RAISE EXCEPTION 'kind % does not allow category %', NEW.kind, NEW.category
            USING ERRCODE = 'ZA001';
    END IF;

    IF NEW.parent_id IS NULL THEN
        IF k.child_only THEN
            RAISE EXCEPTION 'kind % must have a parent', NEW.kind USING ERRCODE = 'ZA001';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.parent_id = NEW.id THEN
        RAISE EXCEPTION 'evidence cannot be its own parent' USING ERRCODE = 'ZA001';
    END IF;
    SELECT * INTO p FROM evidence WHERE user_id = NEW.user_id AND id = NEW.parent_id;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key reports it
    END IF;
    IF p.parent_id IS NOT NULL THEN
        RAISE EXCEPTION 'evidence nests at most two levels' USING ERRCODE = 'ZA001';
    END IF;
    IF p.form <> 'engagement' THEN
        RAISE EXCEPTION 'only engagements can have children' USING ERRCODE = 'ZA001';
    END IF;
    IF NOT (NEW.form IN ('output', 'recognition') OR NEW.kind = 'assignment') THEN
        RAISE EXCEPTION 'kind % cannot be nested', NEW.kind USING ERRCODE = 'ZA001';
    END IF;
    IF NEW.category <> p.category THEN
        RAISE EXCEPTION 'a child must have its parent''s category (%)', p.category
            USING ERRCODE = 'ZA001';
    END IF;
    IF EXISTS (SELECT 1 FROM evidence WHERE user_id = NEW.user_id AND parent_id = NEW.id) THEN
        RAISE EXCEPTION 'evidence with children cannot be nested' USING ERRCODE = 'ZA001';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION evidence_check() FROM PUBLIC;

CREATE TRIGGER evidence_check
    BEFORE INSERT OR UPDATE OF kind, form, category, parent_id ON evidence
    FOR EACH ROW EXECUTE FUNCTION evidence_check();

-- A parent's category change must carry its children along (the API does
-- this in the same transaction); this rejects any write that would not.
-- +goose StatementBegin
CREATE FUNCTION evidence_children_category() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM evidence
               WHERE user_id = NEW.user_id AND parent_id = NEW.id AND category <> NEW.category) THEN
        RAISE EXCEPTION 'children must have their parent''s category (%)', NEW.category
            USING ERRCODE = 'ZA001';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION evidence_children_category() FROM PUBLIC;

CREATE CONSTRAINT TRIGGER evidence_children_category
    AFTER UPDATE OF category ON evidence
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION evidence_children_category();

CREATE TABLE highlight (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    evidence_id   uuid NOT NULL,
    text          text NOT NULL CHECK (text <> ''),
    metric_value  numeric,
    metric_unit   text NOT NULL DEFAULT '',
    auto_include  boolean NOT NULL DEFAULT true,
    position      text COLLATE "C" NOT NULL,
    content_hash  bytea NOT NULL,
    UNIQUE (user_id, id),
    FOREIGN KEY (user_id, evidence_id) REFERENCES evidence (user_id, id) ON DELETE CASCADE,
    CHECK (metric_unit = '' OR metric_value IS NOT NULL)
);
CREATE INDEX highlight_evidence ON highlight (user_id, evidence_id, position);

CREATE TABLE skill (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    name            text NOT NULL CHECK (name <> ''),
    normalized_name text NOT NULL,
    type            skill_type NOT NULL DEFAULT 'hard',
    proficiency     text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id),
    UNIQUE (user_id, normalized_name),
    CHECK (proficiency = ''
           OR (type = 'language' AND proficiency IN ('A1', 'A2', 'B1', 'B2', 'C1', 'C2', 'native'))
           OR (type <> 'language' AND proficiency IN ('beginner', 'intermediate', 'advanced', 'expert')))
);

CREATE TABLE evidence_skill (
    user_id     uuid NOT NULL,
    evidence_id uuid NOT NULL,
    skill_id    uuid NOT NULL,
    PRIMARY KEY (user_id, evidence_id, skill_id),
    FOREIGN KEY (user_id, evidence_id) REFERENCES evidence (user_id, id) ON DELETE CASCADE,
    FOREIGN KEY (user_id, skill_id) REFERENCES skill (user_id, id) ON DELETE CASCADE
);
CREATE INDEX evidence_skill_skill ON evidence_skill (user_id, skill_id);

CREATE TABLE highlight_skill (
    user_id      uuid NOT NULL,
    highlight_id uuid NOT NULL,
    skill_id     uuid NOT NULL,
    PRIMARY KEY (user_id, highlight_id, skill_id),
    FOREIGN KEY (user_id, highlight_id) REFERENCES highlight (user_id, id) ON DELETE CASCADE,
    FOREIGN KEY (user_id, skill_id) REFERENCES skill (user_id, id) ON DELETE CASCADE
);
CREATE INDEX highlight_skill_skill ON highlight_skill (user_id, skill_id);

-- Domain events written in the same transaction as the change. The API may
-- only insert; the relay (part 3) reads and deletes with its own role.
CREATE TABLE outbox (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    topic      text NOT NULL,
    payload    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
REVOKE SELECT, UPDATE, DELETE ON outbox FROM zapply_app;

-- Stored responses for retried creates (Idempotency-Key).
CREATE TABLE idempotency_key (
    user_id      uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    key          text NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_hash bytea NOT NULL,
    status       integer NOT NULL,
    body         bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['profile', 'organization', 'evidence', 'highlight', 'skill',
                             'evidence_skill', 'highlight_skill', 'outbox', 'idempotency_key']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant ON %I USING (user_id = app_user_id()) '
                       'WITH CHECK (user_id = app_user_id())', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE idempotency_key, outbox, highlight_skill, evidence_skill, skill, highlight,
           evidence, organization, profile, evidence_kind;
DROP FUNCTION evidence_children_category();
DROP FUNCTION evidence_check();
DROP TYPE skill_type, organization_type, date_precision, evidence_form, evidence_category;
