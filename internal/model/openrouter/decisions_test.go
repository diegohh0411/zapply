package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

func TestDecisionHTTPAndReservation(t *testing.T) {
	calls := 0
	html := false
	oversized := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			if r.URL.Query().Get("output_modalities") != "decisions" {
				t.Error("missing decision filter")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"test/decision","name":"Decision","context_length":1000,"architecture":{"output_modalities":["decisions"]},"pricing":{"prompt":"0.000001","completion":"0","request":"0.001"}}]}`))
		case "/api/v1/systemone":
			calls++
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Error("wrong key")
			}
			var body systemone.Request
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Questions) != 2 {
				t.Error("bad wire shape")
			}
			if html {
				w.Write([]byte("<html>not the API</html>"))
				return
			}
			if oversized {
				w.WriteHeader(413)
				w.Write([]byte(`{"error":{"code":413}}`))
				return
			}
			w.Write([]byte(`{"id":"gen-test","answers":{"q1":{"type":"noul","noul":0.8}},"usage":{"cost":0.000001,"input_tokens":80,"output_tokens":2}}`))
		default:
			t.Errorf("wrong route: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := New("", "")
	c.HTTP = server.Client()
	c.BaseURL = server.URL + "/api/v1"
	request := systemone.Request{Model: "test/decision", State: systemone.Raw("record"), Questions: map[string]systemone.Question{"q1": {Type: "noul", Instructions: systemone.Raw("Is `record` relevant?")}, "q2": {Type: "noul", Instructions: systemone.Raw("Is `record` complete?")}}}
	// The one-token-per-byte prior exceeds this tiny context, so the quote
	// reserves the full context once plus the request fee per question.
	q, e := modelapi.QuoteDecision(context.Background(), c, request, billing.Calibration{})
	if e != nil || calls != 0 {
		t.Fatalf("owner-selected quote: %v, calls %d", e, calls)
	}
	if want, _ := modelapi.ParseUSD("0.003"); q.Reserved != want {
		t.Fatalf("owner-selected reserve %d want %d", q.Reserved, want)
	}
	empty := request
	empty.Questions = nil
	if _, err := modelapi.QuoteDecision(context.Background(), c, empty, billing.Calibration{}); err == nil || calls != 0 {
		t.Fatalf("empty request: %v, calls %d", err, calls)
	}
	c.DecisionBounds = map[string]BillingBound{"test/decision": {MaxRequestBytes: 10}}
	tooLarge, e := modelapi.QuoteDecision(context.Background(), c, request, billing.Calibration{})
	if pe := (*modelapi.ProviderError)(nil); !errors.As(e, &pe) || pe.Code != "request_too_large" || !tooLarge.Decision {
		t.Fatalf("oversized request quoted: %v", e)
	}
	c.DecisionBounds = nil
	result, e := c.Decide(context.Background(), "test-key", request, q)
	if e != nil || result.Amount == nil || *result.Amount != 1000000 || result.GenerationID != "gen-test" {
		t.Fatalf("result %#v %v", result, e)
	}
	html = true
	_, e = c.Decide(context.Background(), "test-key", request, q)
	var pe *modelapi.ProviderError
	if !errors.As(e, &pe) || pe.Uncharged {
		t.Fatal("HTML accepted or treated as uncharged")
	}
	html = false
	oversized = true
	_, e = c.Decide(context.Background(), "test-key", request, q)
	if !errors.As(e, &pe) || !pe.Uncharged {
		t.Fatal("error envelope without billing evidence left uncertain")
	}
	q.FetchedAt = time.Now().Add(-time.Hour)
	before := calls
	if _, e = c.Decide(context.Background(), "test-key", request, q); e == nil || calls != before {
		t.Fatal("stale quote sent")
	}
}

func TestDecisionQuoteAcceptsExtraPricingComponents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"~liquid/d1-ultra-latest","name":"D1 Ultra","context_length":32768,"architecture":{"output_modalities":["decisions"]},"pricing":{"prompt":"0.000000042","completion":"0","input_cache_read":"0.000000042"}}]}`))
	}))
	defer server.Close()
	c := New("", "")
	c.HTTP = server.Client()
	c.BaseURL = server.URL + "/api/v1"
	request := systemone.Request{Model: "~liquid/d1-ultra-latest", State: systemone.Raw("record"), Questions: map[string]systemone.Question{"q1": {Type: "noul", Instructions: systemone.Raw("Is `record` relevant?")}}}
	if _, e := modelapi.QuoteDecision(context.Background(), c, request, billing.Calibration{}); e != nil {
		t.Fatalf("quote with input_cache_read pricing: %v", e)
	}
}

func sheetFor(t *testing.T, pricing string) (billing.Sheet, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"test/decision","name":"Decision","context_length":32768,"architecture":{"output_modalities":["decisions"]},"pricing":` + pricing + `}]}`))
	}))
	defer server.Close()
	c := New("", "")
	c.HTTP = server.Client()
	c.BaseURL = server.URL + "/api/v1"
	return c.DecisionSheet(context.Background(), "test/decision")
}

func TestDecisionSheetPricesEveryComponent(t *testing.T) {
	request := systemone.Request{Model: "test/decision", State: systemone.Raw("record"), Questions: map[string]systemone.Question{"q1": {Type: "noul", Instructions: systemone.Raw("Is `record` relevant?")}}}
	reserve := func(s billing.Sheet) int64 {
		shape := billing.Shape{RequestBytes: len(systemone.Raw(request)), Questions: 1}
		p, e := billing.Predict(s, shape, billing.Calibration{})
		if e != nil {
			t.Fatal(e)
		}
		return p.Reserved
	}

	// Cache reads pricier than the prompt price every input token.
	s, e := sheetFor(t, `{"prompt":"0.000000042","completion":"0","input_cache_read":"0.0000001"}`)
	if e != nil || s.InputRate != 100_000 {
		t.Fatalf("cache read rate: %+v %v", s, e)
	}

	// No prompt price, a cache read price and no request fee still reserve.
	s, e = sheetFor(t, `{"input_cache_read":"0.000000042"}`)
	if e != nil || s.InputRate != 42_000 || reserve(s) <= 0 {
		t.Fatalf("missing prompt: %+v %v", s, e)
	}

	// The request fee is charged per question.
	s, e = sheetFor(t, `{"prompt":"0","completion":"0","request":"0.001"}`)
	if e != nil || s.RequestRate != 1_000_000_000 || !s.RequestPerQuestion {
		t.Fatalf("request pricing: %+v %v", s, e)
	}

	// Audio and image charges never apply to decision calls.
	s, e = sheetFor(t, `{"prompt":"0.000000042","input_audio":"0.00001","audio":"0.01","image":"0.002","discount":0.25}`)
	if e != nil || s.InputRate != 42_000 {
		t.Fatalf("audio-capable model rejected: %v", e)
	}
	roles := map[string]string{}
	for _, c := range s.Components {
		roles[c.Name] = c.Role
	}
	if roles["input_audio"] != billing.RoleUnused || roles["discount"] != billing.RoleDiscount || roles["prompt"] != billing.RoleInput {
		t.Fatalf("roles %v", roles)
	}

	// Per-call charges the reservation cannot bound make the model incompatible.
	for pricing, reason := range map[string]string{
		`{"prompt":"0.000000042","completion":"0.000001"}`:         "unbounded_pricing_completion",
		`{"prompt":"0.000000042","internal_reasoning":"0.000002"}`: "unbounded_pricing_internal_reasoning",
		`{"prompt":"0.000000042","new_thing":"0.1"}`:               "unbounded_pricing_new_thing",
		`{"prompt":"0.000000042","request":"free"}`:                "invalid_pricing_request",
	} {
		_, e = sheetFor(t, pricing)
		var pe *modelapi.ProviderError
		if !errors.As(e, &pe) || pe.Code != reason || !pe.Uncharged {
			t.Errorf("%s: %v, want %s", pricing, e, reason)
		}
	}
	// Zero-priced extra components are harmless, and web_search is never billed on our calls.
	if _, e = sheetFor(t, `{"prompt":"0.000000042","web_search":"0","new_thing":0}`); e != nil {
		t.Fatalf("zero components rejected: %v", e)
	}
	if _, e = sheetFor(t, `{"prompt":"0.000000042","web_search":"0.0025"}`); e != nil {
		t.Fatalf("web_search rejected: %v", e)
	}
}
