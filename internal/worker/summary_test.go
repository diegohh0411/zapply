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

// rowsProvider answers each summary call with the next text and records the
// messages each call sent.
type rowsProvider struct {
	*fake
	texts []string
	sent  *[][]summary.Message
}

func (p rowsProvider) ChatSheet(ctx context.Context, model, endpoint string) (billing.Sheet, error) {
	return summaryProvider{fake: p.fake}.ChatSheet(ctx, model, endpoint)
}
func (p rowsProvider) GenerateMessages(ctx context.Context, key string, q modelapi.Quote, messages any) (modelapi.Result, error) {
	*p.sent = append(*p.sent, messages.([]summary.Message))
	p.fake.text = p.texts[min(len(*p.sent), len(p.texts))-1]
	return p.Generate(ctx, key, q)
}

func TestSummaryRevisesToItsRowsThenTrims(t *testing.T) {
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
	instruction := summary.InstructionForRows(2, 100, 190)
	var operation uuid.UUID
	err := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		request := worker.SummaryRequest{Rows: 2, MinCharacters: 100, MaxCharacters: 400, Template: "classic", TemplateVersion: 1, Locale: "en", Instruction: instruction, MaxOutputTokens: 4096, JobID: job, Model: "test/model", Endpoint: "test", Input: "Selected compiler work", Messages: []summary.Message{{Role: "system", Content: instruction + strings.Repeat(" ", 8192)}, {Role: "user", Content: "Selected compiler work"}}}
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
	long := strings.Repeat("Built compilers for many teams. ", 10) // 310 characters: 4 lines below
	var sent [][]summary.Message
	f.p.amount /= 20 // three calls fit the test plan's allowance
	f.r.Provider = rowsProvider{f.p, []string{"Too short.", long, long}, &sent}
	// One line per started 100 characters.
	f.r.MeasureSummary = func(_ context.Context, template string, version int, locale, text string) (int, error) {
		if template != "classic" || version != 1 || locale != "en" {
			t.Fatalf("measured %s/%d %s", template, version, locale)
		}
		return (len(text)-1)/100 + 1, nil
	}
	args := worker.Args{UserID: f.u, OperationID: operation}
	for i := 0; i < 6 && f.state(t, args).Status != "completed"; i++ {
		_ = f.r.Process(ctx, args) // revisions snooze the job
	}
	op := f.state(t, args)
	if op.Status != "completed" || len(op.Result) > 200 || !strings.HasPrefix(op.Result, "Built compilers") || !strings.HasSuffix(op.Result, ".") {
		t.Fatalf("summary operation: %#v", op)
	}
	if len(sent) != 3 {
		t.Fatalf("calls = %d, want the first answer and two revisions", len(sent))
	}
	last := sent[2]
	if len(last) != 6 || last[2].Content != "Too short." || !strings.Contains(last[3].Content, "fills only 1 of the 2 lines") || !strings.Contains(last[5].Content, "wraps to 4 lines") {
		t.Fatalf("revision conversation: %+v", last)
	}
	var charged int64
	if err = admin.QueryRow(ctx, `SELECT sum(amount)::bigint FROM platform.model_usage WHERE user_id=$1`, f.u).Scan(&charged); err != nil || charged != 3*f.p.amount {
		t.Fatalf("every revision is charged once: %d %v", charged, err)
	}
}
