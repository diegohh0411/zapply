package openrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
)

// lunaPricing is GPT-6 Luna's pricing as OpenRouter reports it, including the
// long-prompt override that applies from 272,000 prompt tokens.
const lunaPricing = `{"prompt":"0.0000001","completion":"0.0000005","web_search":"0.01","input_cache_read":"0.00000001","input_cache_write":"0.000000125","discount":0,"overrides":[{"min_prompt_tokens":272000,"prompt":"0.0000002","completion":"0.00000075","input_cache_read":"0.00000002","input_cache_write":"0.00000025"}]}`

func priceJSON(t *testing.T, s *billing.Sheet, raw string) string {
	t.Helper()
	var pricing map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &pricing); err != nil {
		t.Fatal(err)
	}
	return price(s, pricing)
}

func TestOverridesFoldIntoRates(t *testing.T) {
	s := billing.Sheet{OutputEnforced: true, Context: 1_050_000}
	if reason := priceJSON(t, &s, lunaPricing); reason != "" {
		t.Fatalf("reason=%s", reason)
	}
	// Input is the highest input component: the override's cache write (0.00000025).
	if want, _ := modelapi.ParseUSD("0.00000025"); s.InputRate != want {
		t.Fatalf("input rate %d want %d", s.InputRate, want)
	}
	if want, _ := modelapi.ParseUSD("0.00000075"); s.OutputRate != want {
		t.Fatalf("output rate %d want %d", s.OutputRate, want)
	}
	found := false
	for _, c := range s.Components {
		if c.Name == "overrides[0].completion" && c.Role == billing.RoleOutput {
			found = true
		}
	}
	if !found {
		t.Fatalf("override not recorded: %+v", s.Components)
	}
}

func TestOverrideBeyondContextIsIgnored(t *testing.T) {
	s := billing.Sheet{OutputEnforced: true, Context: 100_000}
	if reason := priceJSON(t, &s, lunaPricing); reason != "" {
		t.Fatalf("reason=%s", reason)
	}
	// Base input is the highest base input component: cache write (0.000000125).
	if want, _ := modelapi.ParseUSD("0.000000125"); s.InputRate != want {
		t.Fatalf("input rate %d want base %d", s.InputRate, want)
	}
	if want, _ := modelapi.ParseUSD("0.0000005"); s.OutputRate != want {
		t.Fatalf("output rate %d want base %d", s.OutputRate, want)
	}
}

func TestMalformedOverridesAreRejected(t *testing.T) {
	cases := map[string]string{
		"object":            `{"prompt":"0.0000001","overrides":{"min_prompt_tokens":272000}}`,
		"string threshold":  `{"prompt":"0.0000001","overrides":[{"min_prompt_tokens":"272000","prompt":"0.0000002"}]}`,
		"missing threshold": `{"prompt":"0.0000001","overrides":[{"prompt":"0.0000002"}]}`,
	}
	for name, raw := range cases {
		s := billing.Sheet{OutputEnforced: true, Context: 1_050_000}
		if reason := priceJSON(t, &s, raw); reason != "invalid_pricing_overrides" {
			t.Fatalf("%s: reason=%q", name, reason)
		}
	}
}

func TestUnknownKeyInOverrideIsUnbounded(t *testing.T) {
	s := billing.Sheet{OutputEnforced: true, Context: 1_050_000}
	raw := `{"prompt":"0.0000001","overrides":[{"min_prompt_tokens":272000,"surcharge":"1"}]}`
	if reason := priceJSON(t, &s, raw); reason != "unbounded_pricing_overrides[0].surcharge" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestOverrideOutputRequiresEnforcedCap(t *testing.T) {
	s := billing.Sheet{Context: 1_050_000}
	raw := `{"prompt":"0.0000001","overrides":[{"min_prompt_tokens":272000,"completion":"0.00000075"}]}`
	if reason := priceJSON(t, &s, raw); reason != "unbounded_pricing_overrides[0].completion" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestNonScalarPricingKeyIsUnsupported(t *testing.T) {
	s := billing.Sheet{OutputEnforced: true, Context: 1_050_000}
	raw := `{"prompt":"0.0000001","tiers":[{"min_prompt_tokens":1}]}`
	if reason := priceJSON(t, &s, raw); reason != "unsupported_pricing_tiers" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestLunaRoutesSurviveTextEndpoints(t *testing.T) {
	endpoint := func(tag string, extra map[string]any) map[string]any {
		e := map[string]any{"tag": tag, "provider_name": "Provider", "context_length": 1_050_000, "max_completion_tokens": 128000, "supported_parameters": []string{"max_tokens"}}
		var pricing map[string]any
		_ = json.Unmarshal([]byte(lunaPricing), &pricing)
		e["pricing"] = pricing
		maps := extra
		for k, v := range maps {
			e[k] = v
		}
		return e
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{
			endpoint("openai/flex", map[string]any{"service_tier": "flex"}),
			endpoint("openai", nil),
			endpoint("openai/fast", map[string]any{"service_tier": "fast"}),
		}}})
	}))
	defer server.Close()
	c := New("", "")
	c.BaseURL = server.URL
	endpoints, err := c.TextEndpoints(context.Background(), "openai/gpt-6-luna")
	if err != nil || len(endpoints) != 3 || endpoints[0].ServiceTier != "flex" || endpoints[2].ServiceTier != "priority" {
		t.Fatalf("endpoints=%+v err=%v", endpoints, err)
	}
}

func TestOverridePricedReservationCoversLongPrompt(t *testing.T) {
	s := billing.Sheet{Provider: "openrouter", Model: "openai/gpt-6-luna", Context: 1_050_000, MaxOutput: 128000, OutputEnforced: true,
		Prior: billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 100, OutputPerQ: 2}}
	if reason := priceJSON(t, &s, lunaPricing); reason != "" {
		t.Fatalf("reason=%s", reason)
	}
	p, err := billing.Predict(s, billing.Shape{RequestBytes: 300_000, Questions: 1}, billing.Calibration{})
	if err != nil {
		t.Fatal(err)
	}
	if floor := int64(300_100) * s.InputRate; p.Reserved < floor {
		t.Fatalf("reserved %d below input cost %d at override rate", p.Reserved, floor)
	}
}
