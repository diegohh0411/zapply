package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// These strings deliberately reproduce the pre-migration persisted protocol.
type oldGatewayArgs struct {
	UserID      uuid.UUID `json:"user_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

func (oldGatewayArgs) Kind() string { return "llm_diagnostic" }
func TestModelNamingPreservesAccountingAndQueuedJobs(t *testing.T) {
	f := setupVersion(t, 10)
	ctx := context.Background()
	pending, settled := uuid.New(), uuid.New()
	attempt := uuid.New()
	oldCost, _ := modelapi.ParseUSD("0.1")
	err := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		for _, id := range []uuid.UUID{pending, settled} {
			if _, e := tx.Exec(ctx, `INSERT INTO llm_operation(id,user_id,idempotency_key,request_hash) VALUES($1,$2,$3,'legacy-request')`, id, f.u, id.String()); e != nil {
				return e
			}
		}
		return f.q.InsertTx(ctx, tx, oldGatewayArgs{f.u, pending})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.r.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		var raw []byte
		if e := tx.QueryRow(ctx, `SELECT platform.reserve($1,$2,'test/model','legacy-request','{}',$3,$4)`, attempt, settled, oldCost, time.Now().Add(time.Minute)).Scan(&raw); e != nil {
			return e
		}
		var result struct {
			Reason string `json:"reason"`
		}
		if e := json.Unmarshal(raw, &result); e != nil {
			return e
		}
		if result.Reason != "" {
			t.Fatalf("legacy reservation: %s", result.Reason)
		}
		_, e := tx.Exec(ctx, `SELECT platform.settle($1,$2,NULL,'{}','OK','provider_response')`, attempt, oldCost)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, f.db.AdminURL, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM platform.model_usage WHERE attempt_id=$1 AND amount=$2`, attempt, oldCost).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ledger not preserved: %d %v", count, err)
	}
	// The current runtime must consume the old persisted kind without rewriting it.
	client, err := f.r.Client(1)
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	if err = client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if e := client.StopAndCancel(stop); e != nil {
			t.Error(e)
		}
	}()
	select {
	case <-events:
	case <-time.After(10 * time.Second):
		t.Fatal("legacy queued job not completed")
	}
	op, err := f.q.Get(ctx, f.u, pending)
	if err != nil || op.Status != "completed" || op.Result != "OK" {
		t.Fatalf("legacy operation: %#v %v", op, err)
	}
	if f.p.calls.Load() != 1 {
		t.Fatal("legacy operation sent more than once")
	}
	if got := (worker.Args{}).Kind(); got != "model_operation" {
		t.Fatalf("new jobs retain old kind: %s", got)
	}
}
