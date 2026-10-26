-- Events: the relay publishes outbox rows to NATS and stamps each with a
-- delivery sequence. Design: docs/parts/03-events.md.
--
-- seq is assigned when the relay publishes a row, not when it is inserted, so
-- it follows publish order even when transactions commit out of id order.
-- SSE clients resume with seq > Last-Event-ID.
--
-- zapply_relay is the relay's role. Like zapply_app it is provisioned by ops
-- with LOGIN and a password; this only guarantees it exists so grants succeed.
-- It can touch the outbox and nothing else.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'zapply_relay') THEN
        CREATE ROLE zapply_relay NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE outbox
    ADD COLUMN seq          bigint UNIQUE,
    ADD COLUMN published_at timestamptz,
    ADD CONSTRAINT outbox_published CHECK ((seq IS NULL) = (published_at IS NULL));
CREATE INDEX outbox_pending ON outbox (created_at, id) WHERE published_at IS NULL;
CREATE INDEX outbox_user_seq ON outbox (user_id, seq) WHERE seq IS NOT NULL;
CREATE INDEX outbox_published_at ON outbox (published_at) WHERE published_at IS NOT NULL;

CREATE SEQUENCE outbox_seq AS bigint;

GRANT USAGE ON SCHEMA public TO zapply_relay;
GRANT SELECT, UPDATE, DELETE ON outbox TO zapply_relay;
GRANT USAGE ON SEQUENCE outbox_seq TO zapply_relay;
REVOKE ALL ON SEQUENCE outbox_seq FROM zapply_app;

-- The API reads its own events back to replay missed ones on reconnect. RLS
-- keeps it to the tenant's rows; it still cannot update or delete them.
GRANT SELECT ON outbox TO zapply_app;

-- The tenant policy calls app_user_id(), which only zapply_app may execute, so
-- scope it to that role. The relay sees every row through its own policy.
ALTER POLICY tenant ON outbox TO zapply_app;
CREATE POLICY relay ON outbox TO zapply_relay USING (true) WITH CHECK (true);

-- +goose Down
DROP POLICY relay ON outbox;
ALTER POLICY tenant ON outbox TO PUBLIC;
REVOKE SELECT ON outbox FROM zapply_app;
REVOKE ALL ON outbox FROM zapply_relay;
REVOKE USAGE ON SCHEMA public FROM zapply_relay;
DROP SEQUENCE outbox_seq;
DROP INDEX outbox_published_at, outbox_user_seq, outbox_pending;
ALTER TABLE outbox DROP CONSTRAINT outbox_published, DROP COLUMN published_at, DROP COLUMN seq;
