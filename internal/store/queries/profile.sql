-- name: GetProfile :one
SELECT * FROM profile WHERE user_id = $1;

-- name: InsertProfile :one
INSERT INTO profile (user_id, full_name, headline, email, phone, location, links, summary)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_id) DO NOTHING
RETURNING *;

-- name: UpdateProfile :one
UPDATE profile
SET full_name = $3, headline = $4, email = $5, phone = $6, location = $7, links = $8,
    summary = $9, version = version + 1, updated_at = now()
WHERE user_id = $1 AND version = $2
RETURNING *;
