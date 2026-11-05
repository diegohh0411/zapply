package openrouter

import (
	"context"
	"encoding/json"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"net/http"
	"net/http/httptest"
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
