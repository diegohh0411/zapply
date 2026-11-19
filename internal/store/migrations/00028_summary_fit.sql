-- +goose Up
-- The page fit the browser reported for the content identified by fit_key.
-- Summaries are written only from the content this fit keeps.
ALTER TABLE cv_summary ADD COLUMN fit_key text NOT NULL DEFAULT '', ADD COLUMN fit_dropped text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE cv_summary DROP COLUMN fit_dropped, DROP COLUMN fit_key;
