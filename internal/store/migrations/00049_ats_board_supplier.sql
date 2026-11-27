-- ATS boards as a free supplier (docs/parts/14-ats-board-supplier.md). The
-- platform polls Greenhouse, Lever and Ashby boards the owner seeds or
-- approves, and adds their postings to the catalog like TheirStack's.
-- Suppliers' intervals become settings.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE platform.supplier ADD COLUMN interval_minutes integer NOT NULL DEFAULT 30
 CHECK(interval_minutes IN (5,10,15,30,60,360,1440));
-- The board supplier's row holds only its interval: each board keeps its own
-- schedule and failures.
INSERT INTO platform.supplier(name,interval_minutes) VALUES('ats:boards',15) ON CONFLICT(name) DO NOTHING;

-- One public job board. A seed is active from the start; a discovered board
-- (named by a posting's link) waits for the owner. Disabled covers rejected
-- boards, so discovery never offers them again.
CREATE TABLE platform.ats_board (
 ats text NOT NULL CHECK(ats IN ('greenhouse','lever','ashby')),
 board text NOT NULL CHECK(board ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$'),
 company text NOT NULL DEFAULT '' CHECK(length(company) <= 200),
 source text NOT NULL CHECK(source IN ('seed','discovered')),
 status text NOT NULL CHECK(status IN ('pending','active','disabled')),
 sightings integer NOT NULL DEFAULT 1 CHECK(sightings >= 0),
 next_poll_at timestamptz NOT NULL DEFAULT now(),
 activated_at timestamptz,
 last_polled_at timestamptz,
 last_ok_at timestamptz,
 last_new_at timestamptz,
 not_found_since timestamptz,
 failures integer NOT NULL DEFAULT 0,
 last_error text NOT NULL DEFAULT '' CHECK(length(last_error) <= 500),
 open_count integer NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(ats,board)
);
-- Slugs are matched without case: a link may spell one either way.
CREATE UNIQUE INDEX ats_board_slug ON platform.ats_board(ats,lower(board));
CREATE INDEX ats_board_due ON platform.ats_board(next_poll_at) WHERE status='active';
GRANT SELECT,INSERT,UPDATE ON platform.ats_board TO zapply_worker;
GRANT SELECT,INSERT,UPDATE,DELETE ON platform.ats_board TO zapply_platform;

SET LOCAL ROLE zapply_platform;
CREATE FUNCTION platform.ats_board_json(b platform.ats_board) RETURNS jsonb LANGUAGE sql STABLE
SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('ats',b.ats,'board',b.board,'company',b.company,'source',b.source,'status',b.status,'sightings',b.sightings,
  'open_count',b.open_count,'failures',b.failures,'last_error',b.last_error,'next_poll_at',b.next_poll_at,'last_polled_at',b.last_polled_at,
  'last_ok_at',b.last_ok_at,'last_new_at',b.last_new_at,'created_at',b.created_at) $$;

-- Suppliers with their intervals and health, and how many boards stand where.
CREATE FUNCTION platform.job_sources() RETURNS jsonb LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN jsonb_build_object(
  'suppliers',(SELECT jsonb_agg(jsonb_build_object('name',s.name,'interval_minutes',s.interval_minutes,
    'last_ok_at',CASE WHEN s.name='ats:boards' THEN (SELECT max(last_ok_at) FROM platform.ats_board) ELSE s.last_ok_at END,
    'failures',CASE WHEN s.name='ats:boards' THEN (SELECT count(*) FROM platform.ats_board WHERE status='active' AND failures>0) ELSE s.failures END,
    'last_error',CASE WHEN s.name='ats:boards' THEN '' ELSE s.last_error END,
    'credits_total',s.credits_total) ORDER BY s.name='ats:boards', s.name) FROM platform.supplier s),
  'boards',jsonb_build_object(
    'pending',(SELECT count(*) FROM platform.ats_board WHERE status='pending'),
    'active',(SELECT count(*) FROM platform.ats_board WHERE status='active'),
    'disabled',(SELECT count(*) FROM platform.ats_board WHERE status='disabled'),
    'limit',2000));
END $$;

-- One page of boards in a status: pending by how often postings named them,
-- the rest by company.
CREATE FUNCTION platform.ats_boards(p_status text,p_limit integer,p_offset integer) RETURNS jsonb LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN jsonb_build_object(
  'total',(SELECT count(*) FROM platform.ats_board WHERE status=p_status),
  'boards',COALESCE((SELECT jsonb_agg(platform.ats_board_json(b) ORDER BY
     CASE WHEN p_status='pending' THEN -b.sightings ELSE 0 END, lower(COALESCE(NULLIF(b.company,''),b.board)), b.ats, b.board)
   FROM (SELECT * FROM platform.ats_board WHERE status=p_status ORDER BY
     CASE WHEN p_status='pending' THEN -sightings ELSE 0 END, lower(COALESCE(NULLIF(company,''),board)), ats, board
     LIMIT least(greatest(p_limit,1),200) OFFSET greatest(p_offset,0)) b),'[]'));
END $$;

-- Activating a board polls it soon: its first poll only records what is open.
CREATE FUNCTION platform.assert_board_room(p_more integer) RETURNS void LANGUAGE plpgsql
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 IF (SELECT count(*) FROM platform.ats_board WHERE status='active') + p_more > 2000 THEN
  RAISE EXCEPTION 'at most 2000 boards can be active' USING ERRCODE='ZA409';
 END IF;
END $$;

-- Seeds boards the owner pasted ([{ats,board,company}]), activating any that
-- were pending or disabled. Returns how many were added and activated.
CREATE FUNCTION platform.add_ats_boards(p_boards jsonb) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ DECLARE x record; cur text; added integer := 0; activated integer := 0; BEGIN
 PERFORM platform.assert_owner();
 FOR x IN SELECT DISTINCT ON (r.ats,lower(r.board)) r.ats,r.board,left(COALESCE(r.company,''),200) company
  FROM jsonb_to_recordset(p_boards) r(ats text,board text,company text) LOOP
  SELECT status INTO cur FROM platform.ats_board WHERE ats=x.ats AND lower(board)=lower(x.board) FOR UPDATE;
  IF cur='active' THEN CONTINUE; END IF;
  PERFORM platform.assert_board_room(1);
  IF cur IS NULL THEN
   INSERT INTO platform.ats_board(ats,board,company,source,status,activated_at) VALUES(x.ats,x.board,x.company,'seed','active',now());
   added := added+1;
  ELSE
   UPDATE platform.ats_board SET status='active',activated_at=now(),next_poll_at=now(),failures=0,not_found_since=NULL,last_error=''
    WHERE ats=x.ats AND lower(board)=lower(x.board);
   activated := activated+1;
  END IF;
 END LOOP;
 PERFORM platform.audit_event('ats_boards.added','00000000-0000-0000-0000-000000000000',
  jsonb_build_object('boards',p_boards,'added',added,'activated',activated));
 RETURN jsonb_build_object('added',added,'activated',activated);
END $$;

-- Approves (active) or rejects/disables (disabled) a board.
CREATE FUNCTION platform.set_ats_board_status(p_ats text,p_board text,p_status text) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ DECLARE b platform.ats_board; BEGIN
 PERFORM platform.assert_owner();
 IF p_status NOT IN ('active','disabled') THEN RAISE EXCEPTION 'status must be active or disabled' USING ERRCODE='ZA422'; END IF;
 SELECT * INTO b FROM platform.ats_board WHERE ats=p_ats AND board=p_board FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'board not found' USING ERRCODE='ZA404'; END IF;
 IF p_status='active' AND b.status<>'active' THEN
  PERFORM platform.assert_board_room(1);
  UPDATE platform.ats_board SET status='active',activated_at=now(),next_poll_at=now(),failures=0,not_found_since=NULL,last_error=''
   WHERE ats=p_ats AND board=p_board RETURNING * INTO b;
 ELSIF p_status='disabled' AND b.status<>'disabled' THEN
  UPDATE platform.ats_board SET status='disabled',last_error=CASE WHEN b.status='pending' THEN 'rejected' ELSE 'disabled by the owner' END
   WHERE ats=p_ats AND board=p_board RETURNING * INTO b;
 END IF;
 PERFORM platform.audit_event('ats_board.status','00000000-0000-0000-0000-000000000000',platform.ats_board_json(b));
 RETURN platform.ats_board_json(b);
END $$;

-- Polls an active board on the next tick.
CREATE FUNCTION platform.poll_ats_board(p_ats text,p_board text) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ DECLARE b platform.ats_board; BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.ats_board SET next_poll_at=now() WHERE ats=p_ats AND board=p_board AND status='active' RETURNING * INTO b;
 IF NOT FOUND THEN RAISE EXCEPTION 'no active board by that name' USING ERRCODE='ZA404'; END IF;
 RETURN platform.ats_board_json(b);
END $$;

-- Sets how often a supplier runs. A shorter interval takes effect now rather
-- than after the run already scheduled.
CREATE FUNCTION platform.save_supplier_interval(p_name text,p_minutes integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 UPDATE platform.supplier SET interval_minutes=p_minutes,next_run_at=least(next_run_at,now()+make_interval(mins=>p_minutes)) WHERE name=p_name;
 IF NOT FOUND THEN RAISE EXCEPTION 'supplier not found' USING ERRCODE='ZA404'; END IF;
 IF p_name='ats:boards' THEN
  UPDATE platform.ats_board SET next_poll_at=least(next_poll_at,now()+make_interval(mins=>p_minutes)) WHERE status='active';
 END IF;
 PERFORM platform.audit_event('supplier.interval','00000000-0000-0000-0000-000000000000',jsonb_build_object('name',p_name,'interval_minutes',p_minutes));
 RETURN platform.job_sources();
END $$;

REVOKE EXECUTE ON FUNCTION platform.ats_board_json(platform.ats_board),platform.job_sources(),platform.ats_boards(text,integer,integer),
 platform.assert_board_room(integer),platform.add_ats_boards(jsonb),platform.set_ats_board_status(text,text,text),
 platform.poll_ats_board(text,text),platform.save_supplier_interval(text,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.job_sources(),platform.ats_boards(text,integer,integer),platform.add_ats_boards(jsonb),
 platform.set_ats_board_status(text,text,text),platform.poll_ats_board(text,text),platform.save_supplier_interval(text,integer) TO zapply_app;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.save_supplier_interval(text,integer),platform.poll_ats_board(text,text),platform.set_ats_board_status(text,text,text),
 platform.add_ats_boards(jsonb),platform.assert_board_room(integer),platform.ats_boards(text,integer,integer),platform.job_sources(),
 platform.ats_board_json(platform.ats_board);
DROP TABLE platform.ats_board;
DELETE FROM platform.supplier WHERE name='ats:boards';
ALTER TABLE platform.supplier DROP COLUMN interval_minutes;
-- +goose StatementEnd
