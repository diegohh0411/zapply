package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
)

func TestExtractionHTTPAndReservation(t *testing.T) {
	reject, unprocessable := false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			if r.URL.Query().Get("endpoints") != "/api/alpha/extractions" {
				t.Error("missing extraction filter")
			}
			w.Write([]byte(`{"data":[{"id":"cloudflare/clairic","name":"Clairic","context_length":0,"pricing":{"prompt":"0.0000001","completion":"0"}},{"id":"bad/priced","name":"Priced","pricing":{"prompt":"0.0000001","completion":"0.00001"}}]}`))
		case "/api/alpha/extractions":
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Error("wrong key")
			}
			var body extraction.Request
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Fields["salary_min"].Type != "number" || body.State == "" {
				t.Error("bad wire shape")
			}
			if unprocessable {
				w.WriteHeader(422)
				w.Write([]byte(`{"error":{"code":422,"message":"extraction failed"}}`))
				return
			}
			if reject {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":{"message":"unknown key"}}`))
				return
			}
			w.Write([]byte(`{"id":"gen-dec-1-abc","answers":{"salary_min":{"type":"number","value":120000,"confidence":0.99}},"usage":{"cost":0.0000123,"input_tokens":123}}`))
		default:
			t.Errorf("wrong route: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := New("", "")
	c.HTTP = server.Client()
	c.BaseURL = server.URL + "/api/v1"
	models, e := c.ExtractionModels(context.Background(), false)
	if e != nil || len(models) != 2 || !models[0].Typed || models[0].Compatibility != "" || models[1].Compatibility != "unbounded_pricing_completion" {
		t.Fatalf("catalogue: %+v %v", models, e)
	}
	r := extraction.Request{Model: "cloudflare/clairic", State: "Pays $120,000 a year.", Fields: map[string]extraction.Field{
		"salary_min": {Type: extraction.Number, Integer: true}, "title": {Type: extraction.Text}}}
	q, e := modelapi.QuoteExtraction(context.Background(), c, r, billing.Calibration{})
	if e != nil || q.Reserved <= 0 || q.Shape.Questions != 2 {
		t.Fatalf("quote: %+v %v", q, e)
	}
	res, e := c.Extract(context.Background(), "test-key", r, q)
	if e != nil || res.Amount == nil || *res.Amount != 12_300_000 || res.GenerationID != "gen-dec-1-abc" || res.Text == "" {
		t.Fatalf("extract: %+v %v", res, e)
	}
	if *res.Amount > q.Reserved {
		t.Errorf("charge %d over reservation %d", *res.Amount, q.Reserved)
	}
	reject = true
	_, e = c.Extract(context.Background(), "test-key", r, q)
	var pe *modelapi.ProviderError
	if !errors.As(e, &pe) || pe.Code != "request_rejected" || !pe.Uncharged {
		t.Errorf("rejected request: %v", e)
	}
	// An error envelope without a generation ID or usage was not billed.
	reject, unprocessable = false, true
	_, e = c.Extract(context.Background(), "test-key", r, q)
	if !errors.As(e, &pe) || !pe.Uncharged {
		t.Errorf("422 without billing evidence: %v", e)
	}
	unprocessable = false
	// Untyped models are refused number fields before anything is sent.
	r.Model = "scaledown/extract"
	if _, e := modelapi.QuoteExtraction(context.Background(), c, r, billing.Calibration{}); !errors.As(e, &pe) || pe.Code != "invalid_request" {
		t.Errorf("untyped number field: %v", e)
	}
}
