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
-- statement_timestamp(), not now(): now() is the transaction start, before
-- the lock was acquired, so it would not follow seq order.
UPDATE outbox o SET seq = b.seq, published_at = statement_timestamp()
FROM (SELECT unnest(sqlc.arg(ids)::uuid[]) AS id, unnest(sqlc.arg(seqs)::bigint[]) AS seq) AS b
WHERE o.id = b.id;

-- PruneOutbox deletes a prefix of published seqs: everything below the first
-- event published at or after `before`. Runs under the exclusive relay lock,
-- so a replay never sees a cursor survive while events after it are gone.
-- name: PruneOutbox :execrows
DELETE FROM outbox WHERE id IN (
    SELECT p.id FROM outbox p
    WHERE p.seq IS NOT NULL
      AND p.seq < coalesce(
          (SELECT min(r.seq) FROM outbox r WHERE r.seq IS NOT NULL AND r.published_at >= sqlc.arg(before)::timestamptz),
          9223372036854775807)
    ORDER BY p.seq
    LIMIT sqlc.arg(n)
);
