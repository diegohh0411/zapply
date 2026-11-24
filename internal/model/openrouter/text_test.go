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
	unbounded["pricing"] = map[string]string{"surcharge": "1"}
	// Lists web_search at a price; it is never billed on our calls, so the route stays.
	muse := valid("meta")
	muse["pricing"] = map[string]string{"prompt": "0.0000001", "completion": "0.0000005", "web_search": "0.0025", "input_cache_read": "0.000000004", "discount": "0"}
	flex := valid("provider/flex")
	flex["service_tier"] = "flex"
	priority := valid("provider/turbo")
	priority["service_tier"] = "priority"
	zero := valid("zero")
	zero["max_completion_tokens"] = 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/test/model/endpoints" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{valid("provider/fp8"), valid("provider/fp8"), unsupported, unbounded, zero, flex, priority, muse}}})
	}))
	defer server.Close()
	c := New("", "")
	c.BaseURL = server.URL
	endpoints, err := c.TextEndpoints(context.Background(), "test/model")
	if err != nil || len(endpoints) != 4 || endpoints[0].Tag != "provider/fp8" || endpoints[0].Name != "Provider" {
		t.Fatalf("endpoints=%+v err=%v", endpoints, err)
	}
	if endpoints[0].ServiceTier != "default" || endpoints[1].ServiceTier != "flex" || endpoints[1].Tag != "provider/flex" || endpoints[2].ServiceTier != "priority" || endpoints[3].Tag != "meta" {
		t.Fatalf("tier metadata lost: %+v", endpoints)
	}
	if _, err = c.TextEndpoints(context.Background(), "../invalid"); err == nil {
		t.Fatal("accepted invalid model")
	}
}

func TestTextEndpointsAcceptsOpenRouterAliases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/~openai/gpt-luna-latest/endpoints" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		// OpenRouter's router alias lists no providers of its own.
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
	}))
	defer server.Close()
	c := New("", "")
	c.BaseURL = server.URL
	endpoints, err := c.TextEndpoints(context.Background(), "~openai/gpt-luna-latest")
	if err != nil || len(endpoints) != 0 {
		t.Fatalf("endpoints=%+v err=%v", endpoints, err)
	}
}
