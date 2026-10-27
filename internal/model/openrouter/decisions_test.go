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
	// Owner-selected models reserve for full context and request fee per question.
	q, e := c.QuoteDecision(context.Background(), request)
	if e != nil || calls != 0 {
		t.Fatalf("owner-selected quote: %v, calls %d", e, calls)
	}
	if want, _ := modelapi.ParseUSD("0.004"); q.Reserved != want {
		t.Fatalf("worst-case reserve %d want %d", q.Reserved, want)
	}
	empty := request
	empty.Questions = nil
	if _, err := c.QuoteDecision(context.Background(), empty); err == nil || calls != 0 {
		t.Fatalf("empty request: %v, calls %d", err, calls)
	}
	c.DecisionBounds = map[string]BillingBound{"test/decision": {Reference: "fake provider fixture: context charged per question, request fee once", PerQuestion: true, MaxRequestBytes: 10000}}
	q, e = c.QuoteDecision(context.Background(), request)
	if e != nil {
		t.Fatal(e)
	}
	want, _ := modelapi.ParseUSD("0.003")
	if q.Reserved != want {
		t.Fatalf("reserve %d want %d", q.Reserved, want)
	}
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
	if !errors.As(e, &pe) || pe.Uncharged {
		t.Fatal("413 status treated as proof of no charge")
	}
	q.FetchedAt = time.Now().Add(-time.Hour)
	before := calls
	if _, e = c.Decide(context.Background(), "test-key", request, q); e == nil || calls != before {
		t.Fatal("stale quote sent")
	}
}
