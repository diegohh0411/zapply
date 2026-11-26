package posting

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxReadAttempts bounds the extractions one posting read runs: its first
// answer and up to two retries.
const maxReadAttempts = 3

// Reads queues a posting read's extraction, and retries it when the
// description sentences the model named are not ones the page confirms
// (Check). Each attempt is an operation the worker quotes, reserves and
// settles like any other extraction; posting_read links an attempt to the
// one it replaced.
type Reads struct{ queue *worker.Queue }

// NewReads returns reads over q, or nil when q is nil.
func NewReads(q *worker.Queue) *Reads {
	if q == nil {
		return nil
	}
	return &Reads{queue: q}
}

// Start queues the first extraction of a posting's state, inside tx.
func (r *Reads) Start(ctx context.Context, tx pgx.Tx, u uuid.UUID, model, state string) (uuid.UUID, error) {
	return r.enqueue(ctx, tx, u, ExtractionRequest(model, state), 1)
}

func (r *Reads) enqueue(ctx context.Context, tx pgx.Tx, u uuid.UUID, req extraction.Request, attempt int) (uuid.UUID, error) {
	id, err := r.queue.EnqueueExtraction(ctx, tx, u, "posting", req)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO posting_read(user_id,operation_id,attempt) VALUES($1,$2,$3)`, u, id, attempt)
	return id, err
}

// OnTerminal retries a read's extraction inside the transaction that finished
// the attempt, when its answer does not confirm the description. It reports
// false for operations that are not a read's attempt.
func (r *Reads) OnTerminal(ctx context.Context, tx pgx.Tx, op worker.Operation) (bool, error) {
	if op.Kind != "extraction" {
		return false, nil
	}
	var attempt int
	var next *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT attempt,next_operation_id FROM posting_read WHERE user_id=$1 AND operation_id=$2 FOR UPDATE`, op.UserID, op.ID).Scan(&attempt, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	// A replay: this attempt was already replaced, or it did not answer.
	if next != nil || op.Status != "completed" {
		return true, nil
	}
	var body []byte
	if err := tx.QueryRow(ctx, `SELECT body FROM model_request WHERE user_id=$1 AND operation_id=$2`, op.UserID, op.ID).Scan(&body); err != nil {
		return true, err
	}
	var req extraction.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return true, err
	}
	problems := Check(req, op.Result)
	if problems == nil {
		problems = []string{}
	}
	if len(problems) == 0 || attempt >= maxReadAttempts {
		_, err := tx.Exec(ctx, `UPDATE posting_read SET problems=$3 WHERE user_id=$1 AND operation_id=$2`, op.UserID, op.ID, problems)
		return true, err
	}
	child, err := r.enqueue(ctx, tx, op.UserID, ExtractionRequest(req.Model, req.State, problems...), attempt+1)
	if err != nil {
		return true, err
	}
	_, err = tx.Exec(ctx, `UPDATE posting_read SET problems=$3,next_operation_id=$4 WHERE user_id=$1 AND operation_id=$2`, op.UserID, op.ID, problems, child)
	return true, err
}

// LatestAttempt is the operation a read's poll reports: the read's first
// extraction, or the retry that replaced it, and so on down the chain.
func LatestAttempt(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) (uuid.UUID, error) {
	for range maxReadAttempts - 1 {
		var next *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT next_operation_id FROM posting_read WHERE user_id=$1 AND operation_id=$2`, u, id).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return id, err
		}
		if next == nil {
			return id, nil
		}
		id = *next
	}
	return id, nil
}
