package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EnqueueExtraction stores an extraction request as a model operation in tx
// and queues it. The worker quotes, reserves and settles it like any other
// model call; the answers become the operation's result.
func (q *Queue) EnqueueExtraction(ctx context.Context, tx pgx.Tx, u uuid.UUID, purpose string, r extraction.Request) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	if err := extraction.Validate(r); err != nil {
		return id, err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return id, err
	}
	h := sha256.Sum256(body)
	if _, err = tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'extraction',$3,$4)`, id, u, purpose+":"+id.String(), hex.EncodeToString(h[:])); err != nil {
		return id, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO model_request(user_id,operation_id,body) VALUES($1,$2,$3)`, u, id, json.RawMessage(body)); err != nil {
		return id, err
	}
	_, err = q.client.InsertTx(ctx, tx, Args{UserID: u, OperationID: id}, insertOptions())
	return id, err
}
