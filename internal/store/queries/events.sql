-- name: InsertOutbox :exec
INSERT INTO outbox (id, user_id, topic, payload) VALUES ($1, $2, $3, $4);

-- name: ClaimIdempotencyKey :execrows
INSERT INTO idempotency_key (user_id, key, request_hash, status, body)
VALUES ($1, $2, $3, 0, '')
ON CONFLICT (user_id, key) DO NOTHING;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_key WHERE user_id = $1 AND key = $2;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_key SET status = $3, body = $4 WHERE user_id = $1 AND key = $2;
