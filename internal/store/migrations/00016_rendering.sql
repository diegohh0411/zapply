-- Part 7: rendering. A draft holds one job's CV settings; an export is an
-- immutable snapshot of a CV as it was sent. PDFs are never stored: the
-- browser compiles a snapshot's content with the template version it names.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE cv_draft (
 user_id uuid NOT NULL, job_id uuid NOT NULL,
 template text NOT NULL DEFAULT 'classic' CHECK(length(template) BETWEEN 1 AND 64),
 -- NULL follows the template's latest version.
 template_version int CHECK(template_version > 0),
 locale text NOT NULL DEFAULT 'en' CHECK(length(locale) BETWEEN 2 AND 16),
 page_budget smallint NOT NULL DEFAULT 1 CHECK(page_budget BETWEEN 1 AND 3),
 picks jsonb NOT NULL DEFAULT '{"include":[],"exclude":[]}' CHECK(jsonb_typeof(picks)='object'),
 version int NOT NULL DEFAULT 1, updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,job_id), FOREIGN KEY(user_id,job_id) REFERENCES job(user_id,id) ON DELETE CASCADE
);
CREATE TABLE cv_export (
 id uuid PRIMARY KEY, user_id uuid NOT NULL, job_id uuid NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 255),
 -- No foreign key: the snapshot outlives nothing but its job, and records
 -- which run ranked it.
 match_run_id uuid,
 template text NOT NULL, template_version int NOT NULL, locale text NOT NULL,
 document_date date NOT NULL, page_budget smallint NOT NULL, pages int NOT NULL CHECK(pages > 0), overflow boolean NOT NULL,
 content jsonb NOT NULL CHECK(jsonb_typeof(content)='object' AND octet_length(content::text) <= 262144),
 content_hash text NOT NULL, client_sha256 text CHECK(client_sha256 ~ '^[0-9a-fA-F]{64}$'),
 dropped jsonb NOT NULL CHECK(jsonb_typeof(dropped)='array'),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,id), UNIQUE(user_id,idempotency_key),
 FOREIGN KEY(user_id,job_id) REFERENCES job(user_id,id) ON DELETE CASCADE
);
CREATE INDEX cv_export_job ON cv_export(user_id,job_id,created_at DESC,id DESC);
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['cv_draft','cv_export'] LOOP
 EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant ON %I TO zapply_app USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id())',t);
 END LOOP;
END $$;
-- Snapshots are insert-only. Deleting the job still cascades: referential
-- actions run as the table owner.
REVOKE UPDATE,DELETE ON cv_export FROM zapply_app;
-- +goose StatementEnd

-- +goose Down
DROP TABLE cv_export, cv_draft;
