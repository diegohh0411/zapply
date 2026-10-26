-- Index a fixed-size duplicate key: a valid Unicode URL can escape to many
-- kilobytes and exceed PostgreSQL's btree entry limit. Keep the full URL for
-- exact duplicate lookup. A separate migration also upgrades existing installs.
-- +goose Up
-- convert_to is STABLE because its encoding is normally a runtime argument.
-- This wrapper fixes UTF8; the same input always yields the same bytes.
CREATE FUNCTION job_url_digest(text) RETURNS bytea
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN sha256(convert_to($1, 'UTF8'));
DROP INDEX job_url;
CREATE UNIQUE INDEX job_url ON job (user_id, job_url_digest(normalized_url))
    WHERE normalized_url <> '';

-- +goose Down
DROP INDEX job_url;
CREATE UNIQUE INDEX job_url ON job (user_id, normalized_url) WHERE normalized_url <> '';
DROP FUNCTION job_url_digest(text);
