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
	reject := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/models":
			w.Write([]byte(`{"data":[
			 {"id":"cloudflare/clairic","context_length":65536,"architecture":{"output_modalities":["decisions"]},"pricing":{"prompt":"0.0000001","completion":"0"},"endpoints":["/api/alpha/extractions"]},
			 {"id":"test/decision","context_length":1000,"architecture":{"output_modalities":["decisions"]},"pricing":{"prompt":"0.000001","completion":"0"}}]}`))
		case "/api/alpha/extractions":
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Error("wrong key")
			}
			var body extraction.Request
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.State != "Senior Designer at Linear" || body.Fields["title"].Type != "text" {
				t.Error("bad wire shape")
			}
			if reject {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":{"message":"unknown key"}}`))
				return
			}
			w.Write([]byte(`{"id":"gen-dec-1-abc","model":"cloudflare/clairic","answers":{"title":{"type":"text","value":"Senior Designer","span":[0,15],"p_null":0.01,"confidence":0.99}},
			 "truncated":false,"usage":{"input_tokens":120,"output_tokens":0,"cost":0.000012}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c := New("", "")
	c.BaseURL = server.URL + "/api/v1"
	ctx := context.Background()
	r := extraction.Request{Model: "cloudflare/clairic", State: "Senior Designer at Linear", Fields: map[string]extraction.Field{
		"title": {Type: extraction.Text}, "salary_min": {Type: extraction.Number, Integer: true}}}

	sheet, err := c.ExtractionSheet(ctx, r.Model)
	if err != nil || !sheet.InputPerQuestion {
		t.Fatalf("sheet = %+v, %v; want the state billed per field", sheet, err)
	}
	if _, err := c.ExtractionSheet(ctx, "test/decision"); !providerCode(err, "extraction_unsupported") {
		t.Fatalf("a decision-only model must not be used for extraction: %v", err)
	}
	q, err := modelapi.QuoteExtraction(ctx, c, r, billing.Calibration{})
	if err != nil || q.Reserved <= 0 || q.Endpoint != "extractions" {
		t.Fatalf("quote = %+v, %v", q, err)
	}
	res, err := c.Extract(ctx, "test-key", r, q)
	if err != nil || res.GenerationID != "gen-dec-1-abc" || res.Amount == nil || *res.Amount != 12_000_000 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	answers, err := extraction.Parse(r, []byte(res.Text))
	if err != nil || answers["title"].Text() != "Senior Designer" {
		t.Fatalf("answers = %+v, %v", answers, err)
	}

	reject = true
	if _, err := c.Extract(ctx, "test-key", r, q); !providerCode(err, "request_rejected") {
		t.Fatalf("a 400 without a generation is an uncharged rejection: %v", err)
	}
}

func providerCode(err error, code string) bool {
	var pe *modelapi.ProviderError
	return errors.As(err, &pe) && pe.Code == code && pe.Uncharged
}
