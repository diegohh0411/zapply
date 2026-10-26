-- name: InsertEvidence :exec
INSERT INTO evidence (
    id, user_id, parent_id, organization_id, category, form, kind, title, summary, location, url,
    start_date, start_precision, end_date, end_precision, is_current, details, auto_include,
    position, content_hash
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20
);

-- name: UpdateEvidence :one
UPDATE evidence SET
    organization_id = $4, category = $5, form = $6, kind = $7, title = $8, summary = $9,
    location = $10, url = $11, start_date = $12, start_precision = $13, end_date = $14,
    end_precision = $15, is_current = $16, details = $17, auto_include = $18, content_hash = $19,
    version = version + 1, updated_at = now()
WHERE user_id = $1 AND id = $2 AND version = $3
RETURNING version;

-- name: GetEvidence :one
SELECT * FROM evidence WHERE user_id = $1 AND id = $2;

-- name: GetEvidenceForUpdate :one
SELECT * FROM evidence WHERE user_id = $1 AND id = $2 FOR UPDATE;

-- name: GetEvidences :many
SELECT * FROM evidence WHERE user_id = $1 AND id = ANY (@ids::uuid[]);

-- name: ListEvidences :many
SELECT * FROM evidence
WHERE user_id = @user_id
  AND (sqlc.narg('category')::evidence_category IS NULL OR category = sqlc.narg('category'))
  AND (sqlc.narg('form')::evidence_form IS NULL OR form = sqlc.narg('form'))
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind'))
  AND (sqlc.narg('parent_id')::uuid IS NULL OR parent_id = sqlc.narg('parent_id'))
  AND (NOT @top_level::boolean OR parent_id IS NULL)
  AND (@include_archived::boolean OR archived_at IS NULL)
  AND (sqlc.narg('after_category')::evidence_category IS NULL
       OR (category, position, id) > (sqlc.narg('after_category'), sqlc.narg('after_position')::text COLLATE "C", sqlc.narg('after_id')::uuid))
ORDER BY category, position, id
LIMIT @row_limit;

-- name: ListChildren :many
SELECT * FROM evidence WHERE user_id = $1 AND parent_id = ANY (@parent_ids::uuid[])
ORDER BY position, id;

-- name: CountChildren :one
SELECT count(*) FROM evidence WHERE user_id = $1 AND parent_id = $2;

-- name: SetChildrenCategory :many
UPDATE evidence SET category = $3, version = version + 1, updated_at = now()
WHERE user_id = $1 AND parent_id = $2 AND category <> $3
RETURNING id;

-- name: LastTopLevelPosition :one
SELECT coalesce(max(position), '')::text FROM evidence
WHERE user_id = $1 AND parent_id IS NULL AND category = $2;

-- name: LastChildPosition :one
SELECT coalesce(max(position), '')::text FROM evidence
WHERE user_id = $1 AND parent_id = $2;

-- name: MoveEvidence :one
-- Parent, position and category change together so the nesting trigger
-- never sees a half-moved row.
UPDATE evidence SET parent_id = $3, position = $4, category = $5, version = version + 1, updated_at = now()
WHERE user_id = $1 AND id = $2
RETURNING version;

-- name: SetEvidenceArchived :one
UPDATE evidence SET archived_at = CASE WHEN @archived::boolean THEN coalesce(archived_at, now()) END,
    version = version + 1, updated_at = now()
WHERE user_id = $1 AND id = $2
RETURNING version;

-- name: SetEvidenceHash :exec
UPDATE evidence SET content_hash = $3 WHERE user_id = $1 AND id = $2;

-- name: DeleteEvidence :execrows
DELETE FROM evidence WHERE user_id = $1 AND id = $2;

-- name: ListEvidenceSkills :many
SELECT es.evidence_id, s.name FROM evidence_skill es
JOIN skill s ON s.user_id = es.user_id AND s.id = es.skill_id
WHERE es.user_id = $1 AND es.evidence_id = ANY (@evidence_ids::uuid[])
ORDER BY s.normalized_name;

-- name: ClearEvidenceSkills :exec
DELETE FROM evidence_skill WHERE user_id = $1 AND evidence_id = $2;

-- name: LinkEvidenceSkills :exec
INSERT INTO evidence_skill (user_id, evidence_id, skill_id)
SELECT $1, $2, unnest(@skill_ids::uuid[])
ON CONFLICT DO NOTHING;

-- name: ListHighlights :many
SELECT * FROM highlight WHERE user_id = $1 AND evidence_id = ANY (@evidence_ids::uuid[])
ORDER BY evidence_id, position, id;

-- name: DeleteHighlightsExcept :exec
DELETE FROM highlight
WHERE user_id = $1 AND evidence_id = $2 AND NOT (id = ANY (@keep_ids::uuid[]));

-- name: UpsertHighlight :execrows
INSERT INTO highlight (id, user_id, evidence_id, text, metric_value, metric_unit, auto_include, position, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (id) DO UPDATE SET
    text = EXCLUDED.text, metric_value = EXCLUDED.metric_value, metric_unit = EXCLUDED.metric_unit,
    auto_include = EXCLUDED.auto_include, position = EXCLUDED.position, content_hash = EXCLUDED.content_hash
WHERE highlight.user_id = EXCLUDED.user_id AND highlight.evidence_id = EXCLUDED.evidence_id;

-- name: SetHighlightHash :exec
UPDATE highlight SET content_hash = $3 WHERE user_id = $1 AND id = $2;

-- name: ListHighlightSkills :many
SELECT hs.highlight_id, s.name FROM highlight_skill hs
JOIN skill s ON s.user_id = hs.user_id AND s.id = hs.skill_id
JOIN highlight h ON h.user_id = hs.user_id AND h.id = hs.highlight_id
WHERE hs.user_id = $1 AND h.evidence_id = ANY (@evidence_ids::uuid[])
ORDER BY s.normalized_name;

-- name: ClearHighlightSkills :exec
DELETE FROM highlight_skill WHERE user_id = $1 AND highlight_id = $2;

-- name: LinkHighlightSkills :exec
INSERT INTO highlight_skill (user_id, highlight_id, skill_id)
SELECT $1, $2, unnest(@skill_ids::uuid[])
ON CONFLICT DO NOTHING;

-- name: ListSiblings :many
-- Siblings in a position scope: children of a parent, or top-level evidence
-- of a category.
SELECT id, position FROM evidence
WHERE user_id = @user_id
  AND ((sqlc.narg('parent_id')::uuid IS NULL AND parent_id IS NULL AND category = @category)
       OR parent_id = sqlc.narg('parent_id'))
ORDER BY position, id;
