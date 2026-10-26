-- Foundation: runtime role, tenant context, users and identities.
--
-- Cluster roles are normally provisioned by ops; this only guarantees the
-- runtime role exists so grants below succeed. Give it LOGIN and a password
-- out of band. It must never be superuser or BYPASSRLS (the API checks).

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'zapply_app') THEN
        CREATE ROLE zapply_app NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO zapply_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO zapply_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

-- The tenant for the current transaction, set by store.InTenant with
-- set_config('app.user_id', …, true). NULL outside a tenant transaction, so
-- policies match nothing.
-- +goose StatementBegin
CREATE FUNCTION app_user_id() RETURNS uuid
LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('app.user_id', true), '')::uuid
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION app_user_id() TO zapply_app;

CREATE TABLE app_user (
    id         uuid PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE app_user ENABLE ROW LEVEL SECURITY;
ALTER TABLE app_user FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON app_user
    USING (id = app_user_id()) WITH CHECK (id = app_user_id());

-- Maps an identity provider principal to a user. Not tenant data and not
-- readable by the runtime role; access goes through resolve_identity().
CREATE TABLE identity (
    issuer     text NOT NULL,
    subject    text NOT NULL,
    user_id    uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE
                   DEFERRABLE INITIALLY DEFERRED,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX identity_user_id ON identity (user_id);
REVOKE ALL ON identity FROM zapply_app;

-- Returns the user for (issuer, subject), creating it with p_new_id if this is
-- the first time the principal is seen. Safe under concurrent first requests:
-- the losing insert waits on the winner and reads its row.
-- +goose StatementBegin
CREATE FUNCTION resolve_identity(p_issuer text, p_subject text, p_new_id uuid)
RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    uid uuid;
BEGIN
    SELECT user_id INTO uid FROM identity
     WHERE issuer = p_issuer AND subject = p_subject;
    IF uid IS NOT NULL THEN
        RETURN uid;
    END IF;

    INSERT INTO identity (issuer, subject, user_id)
    VALUES (p_issuer, p_subject, p_new_id)
    ON CONFLICT (issuer, subject) DO NOTHING
    RETURNING user_id INTO uid;

    IF uid IS NULL THEN
        SELECT user_id INTO uid FROM identity
         WHERE issuer = p_issuer AND subject = p_subject;
        RETURN uid;
    END IF;

    PERFORM set_config('app.user_id', uid::text, true);
    INSERT INTO app_user (id) VALUES (uid);
    PERFORM set_config('app.user_id', '', true);
    RETURN uid;
END
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION resolve_identity(text, text, uuid) TO zapply_app;

-- +goose Down
DROP FUNCTION resolve_identity(text, text, uuid);
DROP TABLE identity;
DROP TABLE app_user;
DROP FUNCTION app_user_id();
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT EXECUTE ON FUNCTIONS TO PUBLIC;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM zapply_app;
REVOKE USAGE ON SCHEMA public FROM zapply_app;
