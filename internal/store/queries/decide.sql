-- name: LookupDecisionAnswer :one
SELECT answer FROM decision_answer WHERE user_id=$1 AND model=$2 AND state_hash=$3 AND question_hash=$4;
