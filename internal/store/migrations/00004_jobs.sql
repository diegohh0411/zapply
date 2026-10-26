-- Jobs and application tracking: statuses (board columns), jobs (a posting
-- and the application to it), the activity timeline and saved views.
-- Design: docs/parts/04-jobs.md.
--
-- Tenancy as in 00002: user_id on every row, forced RLS, composite FKs.

-- +goose Up
CREATE TYPE job_stage AS ENUM ('saved', 'active', 'offer', 'closed');
CREATE TYPE job_status_color AS ENUM ('gray', 'blue', 'yellow', 'orange', 'green', 'purple', 'pink', 'red', 'brown');
CREATE TYPE work_mode AS ENUM ('onsite', 'remote', 'hybrid');
CREATE TYPE employment_type AS ENUM ('full_time', 'part_time', 'contract', 'temporary', 'internship', 'freelance');
CREATE TYPE salary_period AS ENUM ('year', 'month', 'hour');
CREATE TYPE job_activity_type AS ENUM ('created', 'status_changed', 'note');
CREATE TYPE job_view_layout AS ENUM ('board', 'table');

CREATE TABLE job_status (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    name            text NOT NULL CHECK (name <> ''),
    normalized_name text NOT NULL,
    color           job_status_color NOT NULL DEFAULT 'gray',
    stage           job_stage NOT NULL,
    position        text COLLATE "C" NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id),
    UNIQUE (user_id, normalized_name)
);
CREATE INDEX job_status_order ON job_status (user_id, position);

CREATE TABLE job (
    id                uuid PRIMARY KEY,
    user_id           uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    status_id         uuid NOT NULL,
    title             text NOT NULL CHECK (title <> ''),
    company           text NOT NULL DEFAULT '',
    url               text NOT NULL DEFAULT '',
    normalized_url    text NOT NULL DEFAULT '',
    location          text NOT NULL DEFAULT '',
    work_mode         work_mode,
    employment_type   employment_type,
    salary_min        bigint CHECK (salary_min >= 0),
    salary_max        bigint CHECK (salary_max >= 0),
    salary_currency   text NOT NULL DEFAULT '' CHECK (salary_currency ~ '^([A-Z]{3})?$'),
    salary_period     salary_period,
    source            text NOT NULL DEFAULT '',
    description       text NOT NULL DEFAULT '',
    notes             text NOT NULL DEFAULT '',
    applied_on        date,
    position          text COLLATE "C" NOT NULL,
    version           integer NOT NULL DEFAULT 1,
    content_hash      bytea NOT NULL,
    status_changed_at timestamptz NOT NULL DEFAULT now(),
    archived_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id),
    FOREIGN KEY (user_id, status_id) REFERENCES job_status (user_id, id),
    CHECK (salary_min IS NULL OR salary_max IS NULL OR salary_min <= salary_max),
    CHECK ((salary_min IS NULL AND salary_max IS NULL) OR salary_currency <> '')
);
CREATE INDEX job_column ON job (user_id, status_id, position);
CREATE INDEX job_updated ON job (user_id, updated_at DESC, id DESC);
CREATE INDEX job_created ON job (user_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX job_url ON job (user_id, normalized_url) WHERE normalized_url <> '';

CREATE TABLE job_activity (
    id          uuid PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    job_id      uuid NOT NULL,
    type        job_activity_type NOT NULL,
    from_status text NOT NULL DEFAULT '',
    to_status   text NOT NULL DEFAULT '',
    body        text NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id),
    FOREIGN KEY (user_id, job_id) REFERENCES job (user_id, id) ON DELETE CASCADE,
    CHECK (type <> 'note' OR body <> '')
);
CREATE INDEX job_activity_timeline ON job_activity (user_id, job_id, occurred_at DESC, id DESC);

CREATE TABLE job_view (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    name       text NOT NULL CHECK (name <> ''),
    layout     job_view_layout NOT NULL DEFAULT 'board',
    filters    jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(filters) = 'object'),
    sort       text NOT NULL DEFAULT '-updated_at',
    position   text COLLATE "C" NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, id)
);
CREATE INDEX job_view_order ON job_view (user_id, position);

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['job_status', 'job', 'job_activity', 'job_view']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant ON %I TO zapply_app USING (user_id = app_user_id()) '
                       'WITH CHECK (user_id = app_user_id())', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE job_view, job_activity, job, job_status;
DROP TYPE job_view_layout, job_activity_type, salary_period, employment_type, work_mode,
          job_stage, job_status_color;
