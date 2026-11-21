package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/diegohh0411/zapply/internal/worker"
)

// extractor is the test provider with an extraction model.
type extractor struct {
	*fake
	got extraction.Request
}

func (e *extractor) ExtractionSheet(context.Context, string) (billing.Sheet, error) {
	return billing.Sheet{Provider: "test", Model: "cloudflare/clairic", InputRate: 100_000, Context: 65536,
		InputPerQuestion: true, Prior: billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 1024}, FetchedAt: time.Now()}, nil
}
func (e *extractor) Extract(_ context.Context, key string, r extraction.Request, q modelapi.Quote) (modelapi.Result, error) {
	e.calls.Add(1)
	e.got = r
	amount := int64(12_000_000)
	if key != "test-provider-secret" || q.Reserved < amount {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "request_rejected", Uncharged: true}
	}
	return modelapi.Result{GenerationID: "gen-dec-test", Amount: &amount, Usage: json.RawMessage(`{"input_tokens":120,"output_tokens":0}`),
		Text: `{"title":{"type":"text","value":"Staff Engineer","confidence":0.98,"p_null":0.01}}`}, nil
}
func (e *extractor) DecisionLookupSupported(string) bool { return false }

func TestExtractionIsMeteredAndAnswered(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := &extractor{fake: f.p}
	f.r.Provider = p
	r := posting.ExtractionRequest("cloudflare/clairic", "Staff Engineer at Acme, remote.")
	body, _ := json.Marshal(r)
	id := uuid.Must(uuid.NewV7())
	if err := f.db.Store.InTenant(ctx, f.u, func(tx pgx.Tx) error {
		var model string
		if err := tx.QueryRow(ctx, `SELECT platform.selected_extraction_model()`).Scan(&model); err != nil || model != "cloudflare/clairic" {
			t.Errorf("default extraction model = %q, %v", model, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'extraction',$3,'h')`, id, f.u, "posting:"+id.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO model_request(user_id,operation_id,body) VALUES($1,$2,$3)`, f.u, id, json.RawMessage(body))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	args := worker.Args{UserID: f.u, OperationID: id}
	if err := f.r.Process(ctx, args); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 || p.got.State != r.State || len(p.got.Fields) != len(r.Fields) {
		t.Fatalf("calls = %d, request = %+v", p.calls.Load(), p.got)
	}
	op := f.state(t, args)
	if op.Status != "completed" || op.Kind != "extraction" {
		t.Fatalf("operation = %+v", op)
	}
	answers, err := extraction.Parse(r, []byte(op.Result))
	if err != nil || answers["title"].Text() != "Staff Engineer" {
		t.Fatalf("answers = %+v, %v", answers, err)
	}
	u := f.usage(t)
	reserved, _ := modelapi.ParseUSD(u["reserved_usd"].(string))
	spent, _ := modelapi.ParseUSD(u["spent_7d_usd"].(string))
	if reserved != 0 || spent != 12_000_000 {
		t.Errorf("usage = %v, want the provider's charge settled", u)
	}
	// A finished operation is not dispatched again.
	if err := f.r.Process(ctx, args); err != nil || p.calls.Load() != 1 {
		t.Fatalf("replayed: %d calls, %v", p.calls.Load(), err)
	}
}
