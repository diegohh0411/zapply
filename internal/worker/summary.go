package worker

import (
	"context"
	"encoding/json"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/summary"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SummaryRequest struct {
	MaxCharacters   int               `json:"max_characters,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Model           string            `json:"model"`
	Endpoint        string            `json:"endpoint"`
	JobID           uuid.UUID         `json:"job_id"`
	Input           string            `json:"input"`
	Messages        []summary.Message `json:"messages"`
}
type ChatProvider interface {
	modelapi.ChatPricer
	GenerateMessages(context.Context, string, modelapi.Quote, any) (modelapi.Result, error)
}

// Poll durable invalidation across API/worker processes. Cancellation never
// discards provider accounting: Process settles using its cleanup context.
func (r *Runtime) watchSummary(ctx context.Context, cancel context.CancelFunc, args Args, job uuid.UUID) func() {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				var stale bool
				err := r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
					return tx.QueryRow(ctx, "SELECT COALESCE((SELECT dirty OR operation_id IS DISTINCT FROM $1 FROM cv_summary WHERE user_id=$2 AND job_id=$3),true)", args.OperationID, args.UserID, job).Scan(&stale)
				})
				if err != nil || stale {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done); <-exited }
}

func (q *Queue) EnqueueSummary(ctx context.Context, tx pgx.Tx, u uuid.UUID, hash string, request SummaryRequest) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	body, err := json.Marshal(request)
	if err != nil {
		return id, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'summary',$4,$3)`, id, u, hash, id.String())
	if err != nil {
		return id, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_request(user_id,operation_id,body) VALUES($1,$2,$3)`, u, id, json.RawMessage(body))
	if err != nil {
		return id, err
	}
	opts := insertOptions()
	opts.ScheduledAt = time.Now().Add(700 * time.Millisecond)
	_, err = q.client.InsertTx(ctx, tx, Args{UserID: u, OperationID: id}, opts)
	return id, err
}

// outputTokenCap preserves the original bound for requests queued before cap configuration.
func (r SummaryRequest) outputTokenCap() int {
	if r.MaxOutputTokens == 0 {
		return 256
	}
	return r.MaxOutputTokens
}

// characterCap preserves the original limit for requests queued before configuration.
func (r SummaryRequest) characterCap() int {
	if r.MaxCharacters == 0 {
		return summary.MaxCharacters
	}
	return r.MaxCharacters
}
