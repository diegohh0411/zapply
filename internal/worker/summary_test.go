package worker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/summary"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type summaryProvider struct {
	*fake
	quote *modelapi.Quote
}

func (p summaryProvider) ChatSheet(ctx context.Context, model, endpoint string) (billing.Sheet, error) {
	return billing.Sheet{Provider: "openrouter", Model: model, RequestRate: p.amount, OutputRate: 1, Context: 100000, MaxOutput: 8192, OutputEnforced: true, Prior: billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 64}, FetchedAt: time.Now()}, nil
}
func (p summaryProvider) GenerateMessages(ctx context.Context, key string, q modelapi.Quote, messages any) (modelapi.Result, error) {
	*p.quote = q
	return p.Generate(ctx, key, q)
}

func TestSummaryUsesMemberAllowance(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	job := uuid.New()
	admin := f.db.Admin(t)
	if _, err := admin.Exec(ctx, `INSERT INTO job_status(id,user_id,name,normalized_name,stage,position) VALUES($1,$2,'Saved','saved','saved','a0') ON CONFLICT(user_id,normalized_name) DO NOTHING`, uuid.New(), f.u); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO job(id,user_id,status_id,title,position,content_hash) SELECT $1,$2,id,'Engineer','a0','\x00'::bytea FROM job_status WHERE user_id=$2 LIMIT 1`, job, f.u); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO cv_draft(user_id,job_id) VALUES($1,$2)`, f.u, job); err != nil {
		t.Fatal(err)
	}
	var operation uuid.UUID
	err := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		request := worker.SummaryRequest{MaxOutputTokens: 4096, JobID: job, Model: "test/model", Endpoint: "test", Input: "Selected compiler work", Messages: []summary.Message{{Role: "system", Content: summary.Instruction + strings.Repeat(" ", 8192)}, {Role: "user", Content: "Selected compiler work"}}}
		var err error
		operation, err = f.q.EnqueueSummary(ctx, tx, f.u, "hash", request)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO cv_summary(user_id,job_id,operation_id,dirty) VALUES($1,$2,$3,false)`, f.u, job, operation)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.p.text = "Engineer experienced in building compilers."
	var quote modelapi.Quote
	f.r.Provider = summaryProvider{f.p, &quote}
	args := worker.Args{UserID: f.u, OperationID: operation}
	if err = f.r.Process(ctx, args); err != nil {
		t.Fatal(err)
	}
	if quote.MaxOutput != 4096 || quote.Shape.MaxOutput != 4096 || quote.Reserved < f.p.amount+4096 {
		t.Fatalf("token cap missing from provider quote or reservation: %#v", quote)
	}
	if op := f.state(t, args); op.Status != "completed" || op.Result != f.p.text {
		t.Fatalf("summary operation: %#v", op)
	}
	var charged int64
	if err = admin.QueryRow(ctx, `SELECT sum(amount)::bigint FROM platform.model_usage WHERE user_id=$1`, f.u).Scan(&charged); err != nil || charged != f.p.amount {
		t.Fatalf("incorrect member charge %d %v", charged, err)
	}
	if f.p.calls.Load() != 1 {
		t.Fatalf("calls=%d", f.p.calls.Load())
	}
}
