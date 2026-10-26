-- name: ListSkills :many
SELECT * FROM skill WHERE user_id = $1 ORDER BY normalized_name, id;

-- name: GetSkill :one
SELECT * FROM skill WHERE user_id = $1 AND id = $2;

-- name: GetSkillsByNormalizedName :many
SELECT * FROM skill WHERE user_id = $1 AND normalized_name = ANY (@names::text[]);

-- name: CreateSkill :one
INSERT INTO skill (id, user_id, name, normalized_name, type, proficiency)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: CreateSkillIfMissing :exec
INSERT INTO skill (id, user_id, name, normalized_name)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, normalized_name) DO NOTHING;

-- name: UpdateSkill :one
UPDATE skill SET name = $3, normalized_name = $4, type = $5, proficiency = $6
WHERE user_id = $1 AND id = $2
RETURNING *;

-- name: DeleteSkill :execrows
DELETE FROM skill WHERE user_id = $1 AND id = $2;

-- name: EvidenceIDsForSkill :many
SELECT evidence_id AS id FROM evidence_skill es WHERE es.user_id = $1 AND es.skill_id = $2
UNION
SELECT h.evidence_id FROM highlight_skill hs
JOIN highlight h ON h.user_id = hs.user_id AND h.id = hs.highlight_id
WHERE hs.user_id = $1 AND hs.skill_id = $2;

-- name: RelinkEvidenceSkills :exec
INSERT INTO evidence_skill (user_id, evidence_id, skill_id)
SELECT es.user_id, es.evidence_id, @into_id FROM evidence_skill es
WHERE es.user_id = $1 AND es.skill_id = @from_id
ON CONFLICT DO NOTHING;

-- name: RelinkHighlightSkills :exec
INSERT INTO highlight_skill (user_id, highlight_id, skill_id)
SELECT hs.user_id, hs.highlight_id, @into_id FROM highlight_skill hs
WHERE hs.user_id = $1 AND hs.skill_id = @from_id
ON CONFLICT DO NOTHING;
