-- Queries for zapply relay, run as zapply_relay. ClaimOutbox and
-- PublishOutbox run while the relay holds its advisory lock, so batches never
-- interleave and seq follows publish order.

-- name: ClaimOutbox :many
SELECT id, user_id, topic, payload FROM outbox
WHERE published_at IS NULL
ORDER BY created_at, id
LIMIT $1
FOR UPDATE;

-- name: NextOutboxSeqs :many
SELECT nextval('outbox_seq')::bigint AS seq FROM generate_series(1, sqlc.arg(n)::int);

-- name: PublishOutbox :exec
UPDATE outbox o SET seq = b.seq, published_at = now()
FROM (SELECT unnest(sqlc.arg(ids)::uuid[]) AS id, unnest(sqlc.arg(seqs)::bigint[]) AS seq) AS b
WHERE o.id = b.id;

-- name: PruneOutbox :execrows
DELETE FROM outbox WHERE id IN (
    SELECT p.id FROM outbox p
    WHERE p.published_at < sqlc.arg(before)::timestamptz
    LIMIT sqlc.arg(n)
);
