-- Early job alerts: a member watches an employer's public ATS board (Greenhouse,
-- Lever or Ashby) with optional filters, and hears about a new opening within
-- the poll interval of its publication.
--
-- Boards are shared: the worker polls each watched board once, whoever watches
-- it, and keeps the postings it has seen in platform tables no member can read.
-- A posting is announced once: the first successful poll of a board (or one
-- after a long gap) only records what is already open, and later polls hand
-- every posting not yet delivered to the watches it matches. Each match becomes
-- a job_alert in its member's tenant, and a trigger files it as a notification.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE job_watch (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
 ats text NOT NULL CHECK(ats IN ('greenhouse','lever','ashby')),
 -- The employer's board slug (Greenhouse calls it a board token).
 board text NOT NULL CHECK(board ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$'),
 company text NOT NULL CHECK(length(company) BETWEEN 1 AND 200),
 -- A posting matches when its title contains any keyword, its location any
 -- location (case-insensitive; empty lists match everything) and, with
 -- remote_only, it is remote.
 keywords text[] NOT NULL DEFAULT '{}' CHECK(cardinality(keywords) <= 20),
 locations text[] NOT NULL DEFAULT '{}' CHECK(cardinality(locations) <= 20),
 remote_only boolean NOT NULL DEFAULT false,
 paused boolean NOT NULL DEFAULT false,
 version int NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,id), UNIQUE(user_id,ats,board)
);
CREATE TABLE job_alert (
 id uuid PRIMARY KEY, user_id uuid NOT NULL, watch_id uuid NOT NULL,
 -- The ATS's own posting id, unique within the board.
 posting_key text NOT NULL CHECK(length(posting_key) BETWEEN 1 AND 200),
 company text NOT NULL CHECK(length(company) <= 200),
 title text NOT NULL CHECK(length(title) <= 500),
 location text NOT NULL DEFAULT '' CHECK(length(location) <= 500),
 url text NOT NULL CHECK(length(url) <= 2048 AND url ~ '^https://'),
 remote boolean NOT NULL DEFAULT false,
 published_at timestamptz,
 found_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,id), UNIQUE(watch_id,posting_key),
 FOREIGN KEY(user_id,watch_id) REFERENCES job_watch(user_id,id) ON DELETE CASCADE
);
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['job_watch','job_alert'] LOOP
 EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
 EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
 EXECUTE format('CREATE POLICY tenant ON %I TO zapply_app,zapply_worker USING(user_id=app_user_id()) WITH CHECK(user_id=app_user_id())',t);
 END LOOP;
END $$;
-- Members manage their watches; only the worker files alerts.
REVOKE ALL ON job_watch,job_alert FROM zapply_app;
GRANT SELECT,INSERT,UPDATE,DELETE ON job_watch TO zapply_app;
GRANT SELECT ON job_alert TO zapply_app;
GRANT SELECT ON job_watch TO zapply_worker;
GRANT SELECT,INSERT ON job_alert TO zapply_worker;

-- Notifications learn a second kind.
ALTER TABLE notification ADD COLUMN job_alert_id uuid;
ALTER TABLE notification DROP CONSTRAINT notification_kind_check, DROP CONSTRAINT notification_check;
ALTER TABLE notification ADD CONSTRAINT notification_kind_check CHECK(kind IN ('mail','job_alert')),
 ADD CONSTRAINT notification_reference CHECK((kind='mail') = (mail_message_id IS NOT NULL) AND (kind='job_alert') = (job_alert_id IS NOT NULL)),
 ADD CONSTRAINT notification_job_alert UNIQUE(user_id,job_alert_id),
 ADD CONSTRAINT notification_job_alert_fk FOREIGN KEY(user_id,job_alert_id) REFERENCES job_alert(user_id,id) ON DELETE CASCADE;
CREATE FUNCTION public.notify_job_alert() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,public AS $$ BEGIN
 INSERT INTO public.notification(user_id,kind,job_alert_id,occurred_at)
  VALUES(NEW.user_id,'job_alert',NEW.id,NEW.found_at) ON CONFLICT(user_id,job_alert_id) DO NOTHING;
 RETURN NULL; END $$;
CREATE TRIGGER notify_job_alert AFTER INSERT ON job_alert FOR EACH ROW EXECUTE FUNCTION public.notify_job_alert();

-- Platform tables: the shared boards, what each has published, and an index
-- of the watches so the poller can match across members.
CREATE TABLE platform.ats_board (
 ats text NOT NULL, board text NOT NULL,
 next_poll_at timestamptz NOT NULL DEFAULT now(),
 last_ok_at timestamptz,
 failures int NOT NULL DEFAULT 0,
 last_error text NOT NULL DEFAULT '' CHECK(length(last_error) <= 500),
 PRIMARY KEY(ats,board)
);
CREATE TABLE platform.ats_posting (
 ats text NOT NULL, board text NOT NULL, posting_key text NOT NULL,
 title text NOT NULL, location text NOT NULL, url text NOT NULL, remote boolean NOT NULL,
 published_at timestamptz,
 first_seen_at timestamptz NOT NULL DEFAULT now(), last_seen_at timestamptz NOT NULL DEFAULT now(),
 delivered boolean NOT NULL DEFAULT false,
 PRIMARY KEY(ats,board,posting_key),
 FOREIGN KEY(ats,board) REFERENCES platform.ats_board(ats,board) ON DELETE CASCADE
);
CREATE INDEX ats_posting_pending ON platform.ats_posting(ats,board) WHERE NOT delivered;
CREATE TABLE platform.job_watch_index (
 watch_id uuid PRIMARY KEY, user_id uuid NOT NULL, ats text NOT NULL, board text NOT NULL, company text NOT NULL,
 keywords text[] NOT NULL, locations text[] NOT NULL, remote_only boolean NOT NULL, paused boolean NOT NULL,
 created_at timestamptz NOT NULL
);
CREATE INDEX job_watch_index_board ON platform.job_watch_index(ats,board) WHERE NOT paused;
GRANT SELECT,INSERT,UPDATE,DELETE ON platform.ats_board,platform.ats_posting,platform.job_watch_index TO zapply_platform;

CREATE FUNCTION platform.index_job_watch() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM platform.job_watch_index WHERE watch_id=OLD.id;
  RETURN OLD;
 END IF;
 INSERT INTO platform.ats_board(ats,board) VALUES(NEW.ats,NEW.board) ON CONFLICT DO NOTHING;
 INSERT INTO platform.job_watch_index(watch_id,user_id,ats,board,company,keywords,locations,remote_only,paused,created_at)
  VALUES(NEW.id,NEW.user_id,NEW.ats,NEW.board,NEW.company,NEW.keywords,NEW.locations,NEW.remote_only,NEW.paused,NEW.created_at)
  ON CONFLICT(watch_id) DO UPDATE SET company=EXCLUDED.company,keywords=EXCLUDED.keywords,locations=EXCLUDED.locations,
   remote_only=EXCLUDED.remote_only,paused=EXCLUDED.paused;
 RETURN NEW; END $$;
CREATE TRIGGER index_job_watch AFTER INSERT OR UPDATE OR DELETE ON job_watch FOR EACH ROW EXECUTE FUNCTION platform.index_job_watch();

-- Hands out boards someone actively watches whose poll is due, each at most
-- once per interval: picking one moves its next poll forward.
CREATE FUNCTION platform.ats_boards_due(p_limit int,p_interval interval) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 WITH picked AS (
  UPDATE platform.ats_board b SET next_poll_at=now()+p_interval
  WHERE (b.ats,b.board) IN (SELECT ats,board FROM platform.ats_board d WHERE d.next_poll_at <= now()
    AND EXISTS(SELECT FROM platform.job_watch_index w WHERE w.ats=d.ats AND w.board=d.board AND NOT w.paused)
    ORDER BY d.next_poll_at LIMIT least(greatest(p_limit,1),1000) FOR UPDATE SKIP LOCKED)
  RETURNING b.ats,b.board)
 SELECT COALESCE(jsonb_agg(jsonb_build_object('ats',ats,'board',board)),'[]') FROM picked $$;

-- Records one poll. On failure it backs off (the interval doubles per failure,
-- up to a day). On success it stores the postings and returns every posting
-- not yet delivered with the watches it matches; the caller files the alerts
-- and then calls ats_postings_delivered with the keys in `pending`.
-- A board polled for the first time, or not successfully in two hours (nobody
-- watched it, or it was failing), only records its postings: they were open
-- before anyone was waiting for them.
CREATE FUNCTION platform.record_ats_poll(p_ats text,p_board text,p_postings jsonb,p_error text,p_interval interval) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE
 baseline boolean;
 out jsonb;
BEGIN
 IF p_error IS NOT NULL THEN
  UPDATE platform.ats_board SET failures=failures+1,last_error=left(p_error,500),
   next_poll_at=now()+least(p_interval*power(2,least(failures,6)),interval '1 day')
  WHERE ats=p_ats AND board=p_board;
  RETURN jsonb_build_object('matches','[]'::jsonb,'pending','[]'::jsonb);
 END IF;
 SELECT last_ok_at IS NULL OR last_ok_at < now()-interval '2 hours' INTO baseline FROM platform.ats_board WHERE ats=p_ats AND board=p_board FOR UPDATE;
 IF NOT FOUND THEN
  RETURN jsonb_build_object('matches','[]'::jsonb,'pending','[]'::jsonb);
 END IF;
 INSERT INTO platform.ats_posting(ats,board,posting_key,title,location,url,remote,published_at,delivered)
  SELECT p_ats,p_board,left(p->>'key',200),left(p->>'title',500),left(COALESCE(p->>'location',''),500),p->>'url',COALESCE((p->>'remote')::boolean,false),(p->>'published_at')::timestamptz,baseline
  FROM jsonb_array_elements(p_postings) p
  WHERE length(p->>'key') > 0 AND p->>'url' ~ '^https://' AND length(p->>'url') <= 2048
  ON CONFLICT(ats,board,posting_key) DO UPDATE SET title=EXCLUDED.title,location=EXCLUDED.location,url=EXCLUDED.url,
   remote=EXCLUDED.remote,last_seen_at=now();
 -- Closed postings are forgotten after a month; one that reopens later is news again.
 DELETE FROM platform.ats_posting WHERE ats=p_ats AND board=p_board AND last_seen_at < now()-interval '30 days';
 UPDATE platform.ats_board SET last_ok_at=now(),failures=0,last_error='' WHERE ats=p_ats AND board=p_board;
 SELECT jsonb_build_object(
  'matches',COALESCE((SELECT jsonb_agg(jsonb_build_object('user_id',w.user_id,'watch_id',w.watch_id,'company',w.company,
    'key',p.posting_key,'title',p.title,'location',p.location,'url',p.url,'remote',p.remote,'published_at',p.published_at))
   FROM platform.ats_posting p JOIN platform.job_watch_index w ON w.ats=p.ats AND w.board=p.board
   WHERE p.ats=p_ats AND p.board=p_board AND NOT p.delivered AND NOT w.paused AND w.created_at <= p.first_seen_at
    AND (cardinality(w.keywords)=0 OR EXISTS(SELECT FROM unnest(w.keywords) k WHERE strpos(lower(p.title),lower(k)) > 0))
    AND (cardinality(w.locations)=0 OR EXISTS(SELECT FROM unnest(w.locations) l WHERE strpos(lower(p.location),lower(l)) > 0))
    AND (NOT w.remote_only OR p.remote)),'[]'::jsonb),
  'pending',COALESCE((SELECT jsonb_agg(posting_key) FROM platform.ats_posting WHERE ats=p_ats AND board=p_board AND NOT delivered),'[]'::jsonb))
 INTO out;
 RETURN out;
END $$;
CREATE FUNCTION platform.ats_postings_delivered(p_ats text,p_board text,p_keys text[]) RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 UPDATE platform.ats_posting SET delivered=true WHERE ats=p_ats AND board=p_board AND posting_key=ANY(p_keys) AND NOT delivered $$;
-- How the current member's boards are doing, for their watch list.
CREATE FUNCTION platform.my_ats_boards() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT COALESCE(jsonb_agg(jsonb_build_object('ats',b.ats,'board',b.board,'last_ok_at',b.last_ok_at,'last_error',b.last_error)),'[]')
 FROM platform.ats_board b WHERE EXISTS(SELECT FROM platform.job_watch_index w WHERE w.ats=b.ats AND w.board=b.board AND w.user_id=public.app_user_id()) $$;

GRANT EXECUTE ON FUNCTION platform.ats_boards_due(int,interval),platform.record_ats_poll(text,text,jsonb,text,interval),platform.ats_postings_delivered(text,text,text[]) TO zapply_worker;
GRANT EXECUTE ON FUNCTION platform.my_ats_boards() TO zapply_app;
DO $$ DECLARE f text; BEGIN
 FOREACH f IN ARRAY ARRAY['platform.index_job_watch()','platform.ats_boards_due(int,interval)','platform.record_ats_poll(text,text,jsonb,text,interval)',
  'platform.ats_postings_delivered(text,text,text[])','platform.my_ats_boards()'] LOOP
  EXECUTE format('REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC',f);
  EXECUTE format('ALTER FUNCTION %s OWNER TO zapply_platform',f);
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.my_ats_boards(),platform.ats_postings_delivered(text,text,text[]),platform.record_ats_poll(text,text,jsonb,text,interval),platform.ats_boards_due(int,interval);
DROP TRIGGER index_job_watch ON job_watch;
DROP FUNCTION platform.index_job_watch();
DROP TABLE platform.job_watch_index,platform.ats_posting,platform.ats_board;
DROP TRIGGER notify_job_alert ON job_alert;
DROP FUNCTION public.notify_job_alert();
DELETE FROM notification WHERE kind='job_alert';
ALTER TABLE notification DROP CONSTRAINT notification_job_alert_fk, DROP CONSTRAINT notification_job_alert,
 DROP CONSTRAINT notification_reference, DROP CONSTRAINT notification_kind_check;
ALTER TABLE notification DROP COLUMN job_alert_id;
ALTER TABLE notification ADD CONSTRAINT notification_kind_check CHECK(kind IN ('mail')),
 ADD CONSTRAINT notification_check CHECK((kind='mail') = (mail_message_id IS NOT NULL));
DROP TABLE job_alert,job_watch;
-- +goose StatementEnd
