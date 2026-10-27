-- name: GetMatchRun :one
SELECT * FROM match_run WHERE user_id=$1 AND id=$2;

-- name: GetMatchRunForUpdate :one
SELECT * FROM match_run WHERE user_id=$1 AND id=$2 FOR UPDATE;
