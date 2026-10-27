package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/llm"
)

const catalog = `{"data":{"endpoints":[{"tag":"test","context_length":1000,"max_completion_tokens":1024,"supported_parameters":["max_tokens"],"pricing":{"prompt":"0.0000004","completion":"0.0000016","request":"0"}}]}}`

func TestRoutingReservationAndBilledCost(t *testing.T) {
	var metadata, calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models/test/model/endpoints":
			metadata.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("key sent to public catalog")
			}
			_, _ = w.Write([]byte(catalog))
		case "/chat/completions":
			calls.Add(1)
			if r.Header.Get("Authorization") != "Bearer secret" {
				t.Error("missing credential")
			}
			var body struct {
				MaxTokens int `json:"max_tokens"`
				Provider  struct {
					Only      []string               `json:"only"`
					Fallbacks bool                   `json:"allow_fallbacks"`
					Require   bool                   `json:"require_parameters"`
					Max       map[string]json.Number `json:"max_price"`
				} `json:"provider"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid request")
			}
			if body.MaxTokens != 128 || body.Provider.Fallbacks || !body.Provider.Require || len(body.Provider.Only) != 1 || body.Provider.Only[0] != "test" || body.Provider.Max["prompt"].String() != "0.400000000000" {
				t.Errorf("routing: %+v", body)
			}
			_, _ = w.Write([]byte(`{"id":"gen-test","choices":[{"message":{"content":"OK"}}],"usage":{"cost":0.0001,"cost_details":{"upstream_inference_cost":99}}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := New("test/model", "test")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	q, err := c.Quote(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := llm.ParseUSD("0.0006048")
	if q.Reserved != want {
		t.Fatalf("reserved %s", llm.USD(q.Reserved))
	}
	_, err = c.Quote(context.Background())
	if err != nil || metadata.Load() != 1 {
		t.Fatalf("catalog not cached: %v", err)
	}
	result, err := c.Generate(context.Background(), "secret", q)
	if err != nil {
		t.Fatal(err)
	}
	amount, _ := llm.ParseUSD("0.0001")
	if result.Amount == nil || *result.Amount != amount || result.Text != "OK" {
		t.Fatalf("billing: %+v", result)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate request")
	}
}
func TestAmbiguousRequestIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"do not log this raw body"}`))
	}))
	defer server.Close()
	c := New("test/model", "test")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	q := llm.Quote{Model: "test/model", Endpoint: "test", InputRate: "0", OutputRate: "0", RequestRate: "0", MaxOutput: 128, FetchedAt: time.Now()}
	_, err := c.Generate(context.Background(), "secret", q)
	var p *llm.ProviderError
	if !errors.As(err, &p) || p.Uncharged || calls.Load() != 1 {
		t.Fatalf("ambiguous outcome: %v calls %d", err, calls.Load())
	}
	q.FetchedAt = time.Now().Add(-16 * time.Minute)
	_, err = c.Generate(context.Background(), "secret", q)
	if !errors.As(err, &p) || !p.Uncharged || calls.Load() != 1 {
		t.Fatal("stale quote dispatched")
	}
}
func TestGenerationLookupPreservesExactCharge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "gen-test" {
			t.Error("wrong generation ID")
		}
		_, _ = w.Write([]byte(`{"data":{"total_cost":0.0000000000001}}`))
	}))
	defer server.Close()
	c := New("test/model", "test")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	result, err := c.Lookup(context.Background(), "secret", "gen-test")
	if err != nil || result.Amount == nil || *result.Amount != 1 {
		t.Fatalf("lookup %+v %v", result, err)
	}
}

func TestErrorResponsesPreserveBillingEvidence(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		charged   bool
		uncharged bool
	}{
		{"charged moderation", 403, `{"id":"gen-filtered","error":{"code":403,"metadata":{"provider_name":"test"}},"usage":{"cost":0.2,"prompt_tokens":2}}`, true, false},
		{"unknown moderation", 403, `{"error":{"code":403,"metadata":{"provider_name":"test"}}}`, false, false},
		{"credits rejected", 402, `{"error":{"code":402}}`, false, true},
		{"key budget rejected", 403, `{"error":{"code":403,"metadata":{"limit_source":"openrouter_key_limit"}}}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := New("test/model", "test")
			c.BaseURL = server.URL
			c.HTTP = server.Client()
			result, err := c.Generate(context.Background(), "secret", llm.Quote{Model: "test/model", Endpoint: "test", InputRate: "0", OutputRate: "0", RequestRate: "0", MaxOutput: 128, FetchedAt: time.Now()})
			var pe *llm.ProviderError
			if !errors.As(err, &pe) || pe.Uncharged != tc.uncharged || (result.Amount != nil) != tc.charged {
				t.Fatalf("result=%+v error=%+v", result, pe)
			}
		})
	}
}
