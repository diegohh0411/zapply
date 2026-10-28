-- Matching sends every request of a round at once, so a member may have up to
-- eight calls in flight instead of one. Held reservations still count against
-- both allowances, and eight stays below the default worker concurrency so
-- other members keep free workers.
-- +goose Up
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;
DO $$ DECLARE body text; BEGIN
 body:=pg_get_functiondef('platform.reserve(uuid,uuid,text,text,jsonb,bigint,timestamptz)'::regprocedure);
 IF position('IF n>0 THEN' IN body)=0 THEN RAISE EXCEPTION 'platform.reserve in-flight check not found'; END IF;
 EXECUTE replace(body,'IF n>0 THEN','IF n>=8 THEN');
END $$;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;
DO $$ DECLARE body text; BEGIN
 body:=pg_get_functiondef('platform.reserve(uuid,uuid,text,text,jsonb,bigint,timestamptz)'::regprocedure);
 IF position('IF n>=8 THEN' IN body)=0 THEN RAISE EXCEPTION 'platform.reserve in-flight check not found'; END IF;
 EXECUTE replace(body,'IF n>=8 THEN','IF n>0 THEN');
END $$;
RESET ROLE;
-- +goose StatementEnd
