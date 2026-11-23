package worker_test

import (
	"context"
	"testing"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/jackc/pgx/v5"
)

// extractor adds the extraction route to the fake provider.
type extractor struct {
	*fake
	request extraction.Request
}

func (e *extractor) ExtractionSheet(context.Context, string) (billing.Sheet, error) {
	return billing.Sheet{Provider: "test", Model: "cloudflare/clairic", InputRate: 1000, Context: 4000, InputPerQuestion: true,
		MaxRequestBytes: extraction.RequestBudget, Prior: billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 16}, FetchedAt: time.Now()}, nil
}
func (e *extractor) Extract(ctx context.Context, key string, r extraction.Request, q modelapi.Quote) (modelapi.Result, error) {
	e.request = r
	e.text = `{"title":{"type":"text","value":"Engineer"}}`
	return e.Generate(ctx, key, q)
}

func TestExtractionIsReservedAndSettled(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := &extractor{fake: f.p}
	f.r.Provider = p
	r := extraction.Request{Model: "cloudflare/clairic", State: "Engineer at Acme", Fields: map[string]extraction.Field{"title": {Type: extraction.Text}}}
	a := worker.Args{UserID: f.u}
	err := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		var err error
		a.OperationID, err = f.q.EnqueueExtraction(ctx, tx, f.u, "posting", r)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Process(ctx, a); err != nil {
		t.Fatal(err)
	}
	op := f.state(t, a)
	if op.Kind != "extraction" || op.Status != "completed" || op.Result != `{"title":{"type":"text","value":"Engineer"}}` {
		t.Fatalf("operation: %+v", op)
	}
	if p.request.State != r.State || p.calls.Load() != 1 {
		t.Errorf("provider saw %+v in %d calls", p.request, p.calls.Load())
	}
	if n, err := modelapi.ParseUSD(f.usage(t)["reserved_usd"].(string)); err != nil || n != 0 {
		t.Errorf("hold not released: %v", f.usage(t))
	}
	// Replays never call the provider again.
	if err := f.r.Process(ctx, a); err != nil || p.calls.Load() != 1 {
		t.Errorf("replay: %v, %d calls", err, p.calls.Load())
	}
}
