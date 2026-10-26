-- Statuses

-- name: ListJobStatuses :many
SELECT * FROM job_status WHERE user_id = $1 ORDER BY position, id;

-- name: GetJobStatusForUpdate :one
-- Locking the status serializes position allocation within its column.
SELECT * FROM job_status WHERE user_id = $1 AND id = $2 FOR UPDATE;

-- name: CreateJobStatus :one
INSERT INTO job_status (id, user_id, name, normalized_name, color, stage, position)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateJobStatus :one
UPDATE job_status SET name = $3, normalized_name = $4, color = $5, stage = $6, updated_at = now()
WHERE user_id = $1 AND id = $2
RETURNING *;

-- name: SetJobStatusPosition :one
UPDATE job_status SET position = $3, updated_at = now()
WHERE user_id = $1 AND id = $2
RETURNING *;

-- name: DeleteJobStatus :execrows
DELETE FROM job_status WHERE user_id = $1 AND id = $2;

-- name: LockJobScope :exec
-- Serializes status seeding and ordering for one user.
SELECT pg_advisory_xact_lock(hashtextextended(@scope::text, 0));

-- Jobs

-- name: GetJob :one
SELECT * FROM job WHERE user_id = $1 AND id = $2;

-- name: GetJobForUpdate :one
SELECT * FROM job WHERE user_id = $1 AND id = $2 FOR UPDATE;

-- name: GetJobIDByURL :one
SELECT id FROM job WHERE user_id = $1 AND normalized_url = $2 AND normalized_url <> '';

-- name: CreateJob :one
INSERT INTO job (id, user_id, status_id, title, company, url, normalized_url, location,
    work_mode, employment_type, salary_min, salary_max, salary_currency, salary_period,
    source, description, notes, applied_on, position, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
RETURNING *;

-- name: UpdateJob :one
UPDATE job SET title = $3, company = $4, url = $5, normalized_url = $6, location = $7,
    work_mode = $8, employment_type = $9, salary_min = $10, salary_max = $11,
    salary_currency = $12, salary_period = $13, source = $14, description = $15, notes = $16,
    applied_on = $17, content_hash = $18, version = version + 1, updated_at = now()
WHERE user_id = $1 AND id = $2
RETURNING *;

-- name: MoveJob :one
-- status_changed_at only moves when the column does.
UPDATE job SET status_changed_at = CASE WHEN status_id = @status_id THEN status_changed_at ELSE now() END,
    status_id = @status_id, position = @position, applied_on = sqlc.narg('applied_on'),
    version = version + 1, updated_at = now()
WHERE user_id = @user_id AND id = @id
RETURNING *;

-- name: SetJobPosition :exec
UPDATE job SET position = $3 WHERE user_id = $1 AND id = $2;

-- name: SetJobArchived :one
UPDATE job SET archived_at = CASE WHEN @archived::boolean THEN coalesce(archived_at, now()) END,
    version = version + 1, updated_at = now()
WHERE user_id = $1 AND id = $2
RETURNING *;

-- name: DeleteJob :execrows
DELETE FROM job WHERE user_id = $1 AND id = $2;

-- name: ListColumn :many
-- One board column in order (archived jobs keep their place).
SELECT id, position FROM job WHERE user_id = $1 AND status_id = $2 ORDER BY position, id;

-- name: CountJobsInStatus :one
SELECT count(*) FROM job WHERE user_id = $1 AND status_id = $2;

-- Lists select ids in the requested order, then load summaries by id, so
-- every sort shares one row type and the heavy text columns stay behind.

-- name: ListJobIDsByUpdated :many
SELECT id, updated_at FROM job
WHERE job.user_id = @user_id
  AND (@include_archived::boolean OR archived_at IS NULL)
  AND (coalesce(cardinality(sqlc.narg('status_ids')::uuid[]), 0) = 0 OR status_id = ANY (sqlc.narg('status_ids')::uuid[]))
  AND (coalesce(cardinality(sqlc.narg('stages')::text[]), 0) = 0
       OR status_id IN (SELECT s.id FROM job_status s WHERE s.user_id = job.user_id AND s.stage::text = ANY (sqlc.narg('stages')::text[])))
  AND (sqlc.narg('q')::text IS NULL OR title ILIKE sqlc.narg('q') OR company ILIKE sqlc.narg('q'))
  AND (sqlc.narg('after_time')::timestamptz IS NULL
       OR (updated_at, id) < (sqlc.narg('after_time')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY updated_at DESC, id DESC
LIMIT @row_limit;

-- name: ListJobIDsByCreated :many
SELECT id, created_at FROM job
WHERE job.user_id = @user_id
  AND (@include_archived::boolean OR archived_at IS NULL)
  AND (coalesce(cardinality(sqlc.narg('status_ids')::uuid[]), 0) = 0 OR status_id = ANY (sqlc.narg('status_ids')::uuid[]))
  AND (coalesce(cardinality(sqlc.narg('stages')::text[]), 0) = 0
       OR status_id IN (SELECT s.id FROM job_status s WHERE s.user_id = job.user_id AND s.stage::text = ANY (sqlc.narg('stages')::text[])))
  AND (sqlc.narg('q')::text IS NULL OR title ILIKE sqlc.narg('q') OR company ILIKE sqlc.narg('q'))
  AND (sqlc.narg('after_time')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_time')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: ListJobIDsByPosition :many
SELECT id, position FROM job
WHERE job.user_id = @user_id
  AND (@include_archived::boolean OR archived_at IS NULL)
  AND (coalesce(cardinality(sqlc.narg('status_ids')::uuid[]), 0) = 0 OR status_id = ANY (sqlc.narg('status_ids')::uuid[]))
  AND (coalesce(cardinality(sqlc.narg('stages')::text[]), 0) = 0
       OR status_id IN (SELECT s.id FROM job_status s WHERE s.user_id = job.user_id AND s.stage::text = ANY (sqlc.narg('stages')::text[])))
  AND (sqlc.narg('q')::text IS NULL OR title ILIKE sqlc.narg('q') OR company ILIKE sqlc.narg('q'))
  AND (sqlc.narg('after_position')::text IS NULL
       OR (position, id) > (sqlc.narg('after_position')::text COLLATE "C", sqlc.narg('after_id')::uuid))
ORDER BY position, id
LIMIT @row_limit;

-- name: GetJobSummaries :many
SELECT id, status_id, title, company, url, location, work_mode, employment_type,
    salary_min, salary_max, salary_currency, salary_period, source, applied_on, position,
    version, status_changed_at, archived_at, created_at, updated_at
FROM job WHERE user_id = $1 AND id = ANY (@ids::uuid[]);

-- Activity

-- name: InsertJobActivity :one
INSERT INTO job_activity (id, user_id, job_id, type, from_status, to_status, body, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListJobActivities :many
SELECT * FROM job_activity WHERE user_id = $1 AND job_id = $2 ORDER BY occurred_at DESC, id DESC;

-- name: GetJobActivity :one
SELECT * FROM job_activity WHERE user_id = $1 AND job_id = $2 AND id = $3;

-- name: UpdateJobActivity :one
UPDATE job_activity SET body = $4, occurred_at = $5, updated_at = now()
WHERE user_id = $1 AND job_id = $2 AND id = $3 AND type = 'note'
RETURNING *;

-- name: DeleteJobActivity :execrows
DELETE FROM job_activity WHERE user_id = $1 AND job_id = $2 AND id = $3 AND type = 'note';

-- Views

-- name: ListJobViews :many
SELECT * FROM job_view WHERE user_id = $1 ORDER BY position, id;

-- name: CountJobViews :one
SELECT count(*) FROM job_view WHERE user_id = $1;

-- name: CreateJobView :one
INSERT INTO job_view (id, user_id, name, layout, filters, sort, position)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateJobView :one
UPDATE job_view SET name = $3, layout = $4, filters = $5, sort = $6, updated_at = now()
WHERE user_id = $1 AND id = $2
RETURNING *;

-- name: DeleteJobView :execrows
DELETE FROM job_view WHERE user_id = $1 AND id = $2;
