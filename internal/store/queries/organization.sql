-- name: ListOrganizations :many
SELECT * FROM organization WHERE user_id = $1 ORDER BY normalized_name, id;

-- name: GetOrganization :one
SELECT * FROM organization WHERE user_id = $1 AND id = $2;

-- name: CreateOrganization :one
INSERT INTO organization (id, user_id, name, normalized_name, type, website, location)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateOrganization :one
UPDATE organization
SET name = $3, normalized_name = $4, type = $5, website = $6, location = $7, updated_at = now()
WHERE user_id = $1 AND id = $2
RETURNING *;

-- name: DeleteOrganization :execrows
DELETE FROM organization WHERE user_id = $1 AND id = $2;

-- name: OrganizationInUse :one
SELECT EXISTS (SELECT 1 FROM evidence WHERE user_id = $1 AND organization_id = $2);

-- name: EvidenceIDsForOrganization :many
SELECT id FROM evidence WHERE user_id = $1 AND organization_id = $2;

-- name: GetOrganizationNames :many
SELECT id, name FROM organization WHERE user_id = $1 AND id = ANY (@ids::uuid[]);
