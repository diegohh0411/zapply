-- Owners can read a supplier now, outside its interval. TheirStack suppliers
-- are due on the next worker tick; the board supplier's active boards are due
-- on their next poll, like the per-board poll.

-- +goose Up
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;

-- Reads a supplier on the next tick. A run already in progress holds the
-- supplier's lease, so asking again is a conflict, not a queued second run.
CREATE FUNCTION platform.poll_supplier(p_name text) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ DECLARE s platform.supplier; BEGIN
 PERFORM platform.assert_owner();
 SELECT * INTO s FROM platform.supplier WHERE name=p_name FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'supplier not found' USING ERRCODE='ZA404'; END IF;
 IF s.running_until IS NOT NULL AND s.running_until>now() THEN
  RAISE EXCEPTION 'supplier is already running' USING ERRCODE='ZA409';
 END IF;
 UPDATE platform.supplier SET next_run_at=now() WHERE name=p_name;
 IF p_name='ats:boards' THEN
  UPDATE platform.ats_board SET next_poll_at=now() WHERE status='active';
 END IF;
 PERFORM platform.audit_event('supplier.poll','00000000-0000-0000-0000-000000000000',jsonb_build_object('name',p_name));
 RETURN platform.job_sources();
END $$;

REVOKE EXECUTE ON FUNCTION platform.poll_supplier(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.poll_supplier(text) TO zapply_app;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.poll_supplier(text);
-- +goose StatementEnd
