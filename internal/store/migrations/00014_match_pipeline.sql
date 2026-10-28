-- Pipeline 2 asks linear question sets against a shared posting and library
-- state. Existing runs keep pipeline 1 so their stored answers recompute with
-- the rules that asked them. Trait is a new requirement class.
-- +goose Up
-- +goose StatementBegin
ALTER TABLE match_run ADD COLUMN pipeline smallint NOT NULL DEFAULT 1 CHECK(pipeline IN (1,2));
ALTER TABLE match_run ALTER COLUMN pipeline SET DEFAULT 2;
ALTER TABLE match_override DROP CONSTRAINT match_override_class_check;
ALTER TABLE match_override ADD CONSTRAINT match_override_class_check CHECK(class IN ('must','preferred','duty','trait','ignore'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM match_override WHERE class='trait';
ALTER TABLE match_override DROP CONSTRAINT match_override_class_check;
ALTER TABLE match_override ADD CONSTRAINT match_override_class_check CHECK(class IN ('must','preferred','duty','ignore'));
ALTER TABLE match_run DROP COLUMN pipeline;
-- +goose StatementEnd
