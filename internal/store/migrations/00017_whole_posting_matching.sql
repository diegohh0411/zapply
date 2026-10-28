-- +goose Up
ALTER TABLE match_run DROP CONSTRAINT match_run_pipeline_check;
ALTER TABLE match_run ADD CONSTRAINT match_run_pipeline_check CHECK(pipeline IN (1,2,3));
ALTER TABLE match_run ALTER COLUMN pipeline SET DEFAULT 3;

-- +goose Down
-- Existing v3 runs must be retained; the constraint prevents downgrading while
-- they exist rather than silently reinterpreting them as historical runs.
ALTER TABLE match_run ALTER COLUMN pipeline SET DEFAULT 2;
ALTER TABLE match_run DROP CONSTRAINT match_run_pipeline_check;
ALTER TABLE match_run ADD CONSTRAINT match_run_pipeline_check CHECK(pipeline IN (1,2));
