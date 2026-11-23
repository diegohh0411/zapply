-- +goose Up
-- +goose StatementBegin
-- Wake the relay and the summary refresher instead of having them poll. A
-- notification is only a hint: both still poll slowly, so one lost while a
-- listener reconnects (or behind a transaction pooler) only delays work.
-- Identical notifications in one transaction are delivered once.
CREATE FUNCTION public.notify_outbox() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN
 PERFORM pg_notify('zapply_outbox', '');
 RETURN NULL; END $$;
CREATE TRIGGER notify_outbox AFTER INSERT ON outbox FOR EACH STATEMENT EXECUTE FUNCTION public.notify_outbox();
CREATE FUNCTION public.notify_summary() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN
 PERFORM pg_notify('zapply_summary', '');
 RETURN NULL; END $$;
CREATE TRIGGER notify_summary AFTER INSERT OR UPDATE ON cv_summary FOR EACH ROW WHEN (NEW.dirty) EXECUTE FUNCTION public.notify_summary();
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DROP TRIGGER notify_summary ON cv_summary;
DROP TRIGGER notify_outbox ON outbox;
DROP FUNCTION public.notify_summary(), public.notify_outbox();
-- +goose StatementEnd
