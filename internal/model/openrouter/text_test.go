package openrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTextEndpointsOnlyOffersBoundedRoutes(t *testing.T) {
	valid := func(tag string) map[string]any {
		return map[string]any{"tag": tag, "provider_name": "Provider", "context_length": 1000, "max_completion_tokens": 256, "supported_parameters": []string{"max_tokens"}, "pricing": map[string]string{"prompt": "0.00001", "completion": "0.00002"}}
	}
	unsupported := valid("unsupported")
	unsupported["supported_parameters"] = []string{}
	unbounded := valid("unbounded")
	unbounded["pricing"] = map[string]string{"web_search": "1"}
	zero := valid("zero")
	zero["max_completion_tokens"] = 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/test/model/endpoints" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{valid("provider/fp8"), valid("provider/fp8"), unsupported, unbounded, zero}}})
	}))
	defer server.Close()
	c := New("", "")
	c.BaseURL = server.URL
	endpoints, err := c.TextEndpoints(context.Background(), "test/model")
	if err != nil || len(endpoints) != 1 || endpoints[0].Tag != "provider/fp8" || endpoints[0].Name != "Provider" {
		t.Fatalf("endpoints=%+v err=%v", endpoints, err)
	}
	if _, err = c.TextEndpoints(context.Background(), "../invalid"); err == nil {
		t.Fatal("accepted invalid model")
	}
}
