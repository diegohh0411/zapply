-- Notifications: one inbox of things a member should look at, read or unread.
-- Today the only kind is `mail` (a message classified as about an application);
-- each new kind adds its own nullable reference column and CHECK.
--
-- Rows are made by the database, not by code: a trigger adds one when a message
-- becomes relevant, so every path that classifies mail produces one.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE notification (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK(kind IN ('mail')),
 mail_message_id uuid,
 -- When the thing happened (a message's received time), which orders the list.
 occurred_at timestamptz NOT NULL,
 read_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,id), UNIQUE(user_id,mail_message_id),
 FOREIGN KEY(user_id,mail_message_id) REFERENCES mail_message(user_id,id) ON DELETE CASCADE,
 CHECK((kind='mail') = (mail_message_id IS NOT NULL))
);
CREATE INDEX notification_list ON notification(user_id,occurred_at DESC,id DESC);
CREATE INDEX notification_unread ON notification(user_id) WHERE read_at IS NULL;
-- Mail found before notifications existed was already seen on the old Mail page.
INSERT INTO notification(user_id,kind,mail_message_id,occurred_at,read_at)
 SELECT user_id,'mail',id,received_at,now() FROM mail_message WHERE state='classified' AND category <> 'other';
ALTER TABLE notification ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON notification TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id());
-- Members only mark notifications read; the worker makes them (via the trigger).
REVOKE ALL ON notification FROM zapply_app;
GRANT SELECT,UPDATE(read_at) ON notification TO zapply_app;
GRANT SELECT,INSERT ON notification TO zapply_worker;

CREATE FUNCTION public.notify_mail_message() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,public AS $$ BEGIN
 IF NEW.state='classified' AND NEW.category IS DISTINCT FROM 'other' THEN
  INSERT INTO public.notification(user_id,kind,mail_message_id,occurred_at)
   VALUES(NEW.user_id,'mail',NEW.id,NEW.received_at) ON CONFLICT(user_id,mail_message_id) DO NOTHING;
 END IF;
 RETURN NULL; END $$;
CREATE TRIGGER notify_mail_message AFTER INSERT OR UPDATE OF state ON mail_message
 FOR EACH ROW WHEN (NEW.state='classified') EXECUTE FUNCTION public.notify_mail_message();

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DROP TRIGGER notify_mail_message ON mail_message;
DROP FUNCTION public.notify_mail_message();
DROP TABLE notification;
-- +goose StatementEnd
