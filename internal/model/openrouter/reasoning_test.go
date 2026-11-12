package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func TestSummaryReasoningPreservesVisibleOutputBudget(t *testing.T) {
	for _, tc := range []struct{ name, metadata, expected string }{
		{"optional", `{"mandatory":false,"supported_efforts":["high","low","none"]}`, `{"enabled":false}`},
		{"mandatory", `{"mandatory":true,"supported_efforts":["high","low"]}`, `{"effort":"low"}`},
		{"plain", `null`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					w.Write([]byte(`{"data":[{"id":"test/model","reasoning":` + tc.metadata + `}]}`))
					return
				}
				calls++
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				got := string(body["reasoning"])
				if got == "" {
					got = "null"
				}
				if got != tc.expected {
					t.Errorf("reasoning=%s want %s", got, tc.expected)
				}
				if string(body["max_tokens"]) != "2048" {
					t.Error("changed output cap")
				}
				w.Write([]byte(`{"id":"gen-test","choices":[{"message":{"content":"Engineer building software."}}],"usage":{"cost":0.0001}}`))
			}))
			defer server.Close()
			c := New("test/model", "test")
			c.BaseURL = server.URL
			c.HTTP = server.Client()
			result, err := c.GenerateMessages(context.Background(), "key", modelapi.Quote{Model: "test/model", Endpoint: "test", InputRate: "0", OutputRate: "0", RequestRate: "0", MaxOutput: 2048, FetchedAt: time.Now()}, diagnosticMessages)
			if err != nil || result.Text == "" || result.Amount == nil || calls != 1 {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
			}
		})
	}
}

func TestReasoningOptionsReadsCatalogueShapes(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		want       []string
		mandatory  bool
	}{
		{"omitted efforts expose no choice", `{"mandatory":false}`, []string{}, false},
		{"null efforts accept every gateway effort", `{"mandatory":false,"supported_efforts":null}`, []string{"max", "xhigh", "high", "medium", "low", "minimal", "off"}, false},
		{"mandatory null efforts drop off", `{"mandatory":true,"supported_efforts":null}`, []string{"max", "xhigh", "high", "medium", "low", "minimal"}, true},
		{"optional list maps none to off", `{"mandatory":false,"supported_efforts":["low","high","none"]}`, []string{"high", "low", "off"}, false},
		{"unknown values are ignored", `{"mandatory":true,"supported_efforts":["xhigh","auto"]}`, []string{"xhigh"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var options ReasoningOptions
			if err := json.Unmarshal([]byte(tc.json), &options); err != nil {
				t.Fatal(err)
			}
			if options.Mandatory != tc.mandatory || !slices.Equal(options.Efforts, tc.want) {
				t.Fatalf("options=%+v want efforts %v mandatory %v", options, tc.want, tc.mandatory)
			}
		})
	}
}

func TestSummaryReasoningHonoursSelectedEffort(t *testing.T) {
	for _, tc := range []struct {
		name, effort, expected string
		calls                  int
	}{
		{"explicit effort", "high", `{"effort":"high"}`, 1},
		{"explicit off", "off", `{"enabled":false}`, 1},
		{"unsupported effort is rejected before spending", "xhigh", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					w.Write([]byte(`{"data":[{"id":"test/model","reasoning":{"mandatory":false,"supported_efforts":["high","low","none"]}}]}`))
					return
				}
				calls++
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if got := string(body["reasoning"]); got != tc.expected {
					t.Errorf("reasoning=%s want %s", got, tc.expected)
				}
				w.Write([]byte(`{"id":"gen-test","choices":[{"message":{"content":"Engineer building software."}}],"usage":{"cost":0.0001}}`))
			}))
			defer server.Close()
			c := New("test/model", "test")
			c.BaseURL = server.URL
			c.HTTP = server.Client()
			_, err := c.GenerateMessages(context.Background(), "key", modelapi.Quote{Model: "test/model", Endpoint: "test", InputRate: "0", OutputRate: "0", RequestRate: "0", MaxOutput: 2048, ReasoningEffort: tc.effort, FetchedAt: time.Now()}, diagnosticMessages)
			if calls != tc.calls {
				t.Fatalf("calls=%d want %d", calls, tc.calls)
			}
			var pe *modelapi.ProviderError
			if tc.calls == 0 && (!errors.As(err, &pe) || pe.Code != "reasoning_config_unavailable") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
