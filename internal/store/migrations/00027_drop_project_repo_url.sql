-- +goose Up
-- Projects keep one link: move any repo_url into url when url is empty, then drop the key.
UPDATE evidence
SET url = CASE WHEN url = '' THEN details->>'repo_url' ELSE url END,
    details = details - 'repo_url'
WHERE kind = 'project' AND details ? 'repo_url';

-- +goose Down
-- Irreversible data cleanup: repo_url values were merged into url.
SELECT 1;
