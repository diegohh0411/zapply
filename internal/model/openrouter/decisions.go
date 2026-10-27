package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

// BillingBound is an adapter assertion backed by documented provider billing
// behavior. No production models are enabled until that evidence is recorded.
// Bounds are deliberately code-owned, never editable through the owner UI.
type BillingBound struct {
	Reference             string
	PerQuestion           bool
	RequestFeePerQuestion bool
	MaxRequestBytes       int
	MaxQuestions          int // zero means no documented count limit
	LookupSupported       bool
}

var verifiedDecisionBounds = map[string]BillingBound{}

type DecisionModel struct {
	FetchedAt    time.Time                  `json:"-"`
	ID           string                     `json:"id"`
	Name         string                     `json:"name"`
	Context      int                        `json:"context_length"`
	Pricing      map[string]json.RawMessage `json:"pricing"`
	Architecture struct {
		Output []string `json:"output_modalities"`
	} `json:"architecture"`
	Compatibility string `json:"compatibility_reason"`
}

func (c *Client) DecisionModels(ctx context.Context, key string, refresh bool) ([]DecisionModel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !refresh && time.Since(c.modelsAt) < 15*time.Minute {
		return slices.Clone(c.models), nil
	}
	path := "/models?output_modalities=decisions"
	models := []DecisionModel{}
	seen := map[string]bool{}
	for page := 0; path != ""; page++ {
		if page >= 100 || seen[path] {
			return nil, errors.New("invalid catalogue pagination")
		}
		seen[path] = true
		raw, err := c.request(ctx, "GET", path, key, nil)
		if err != nil {
			return nil, err
		}
		var body struct {
			Data []DecisionModel `json:"data"`
			Next string          `json:"next_page_url"`
		}
		if json.Unmarshal(raw, &body) != nil {
			return nil, errors.New("invalid model catalogue")
		}
		for _, m := range body.Data {
			if !slices.Contains(m.Architecture.Output, "decisions") {
				continue
			}
			m.FetchedAt = time.Now().UTC()
			m.Compatibility = c.compatibility(m)
			models = append(models, m)
		}
		path = body.Next
		if strings.HasPrefix(path, c.BaseURL+"/") {
			path = strings.TrimPrefix(path, c.BaseURL)
		}
		if path != "" && (!strings.HasPrefix(path, "/models?") || !strings.Contains(path, "output_modalities=decisions")) {
			return nil, errors.New("unsupported catalogue pagination")
		}
	}
	c.models = models
	c.modelsAt = time.Now()
	return slices.Clone(models), nil
}
func (c *Client) bound(model string) (BillingBound, bool) {
	if c.DecisionBounds != nil {
		b, ok := c.DecisionBounds[model]
		return b, ok
	}
	b, ok := verifiedDecisionBounds[model]
	return b, ok
}
func (c *Client) compatibility(m DecisionModel) string {
	b, ok := c.bound(m.ID)
	if !ok || b.Reference == "" || b.MaxRequestBytes <= 0 {
		return "billing_bounds_unverified"
	}
	if m.Context <= 0 {
		return "invalid_context"
	}
	if _, ok := m.Pricing["prompt"]; !ok {
		return "unsupported_pricing"
	}
	if _, ok := m.Pricing["completion"]; !ok {
		return "unsupported_pricing"
	}
	for k, v := range m.Pricing {
		n, e := modelapi.Cost(v)
		if e != nil {
			return "unsupported_pricing"
		}
		if k != "prompt" && k != "request" && n != 0 {
			return "unsupported_pricing"
		}
	}
	return ""
}
func (c *Client) QuoteDecision(ctx context.Context, r systemone.Request) (modelapi.Quote, error) {
	if r.Model == "" {
		return modelapi.Quote{}, &modelapi.ProviderError{Code: "model_not_configured", Uncharged: true}
	}
	models, e := c.DecisionModels(ctx, "", false)
	if e != nil {
		return modelapi.Quote{}, e
	}
	for _, m := range models {
		if m.ID != r.Model {
			continue
		}
		if reason := c.compatibility(m); reason != "" {
			return modelapi.Quote{}, &modelapi.ProviderError{Code: reason, Uncharged: true}
		}
		b, _ := c.bound(m.ID)
		raw, e := json.Marshal(r)
		if e != nil {
			return modelapi.Quote{}, e
		}

		rate := func(k string) string {
			if len(m.Pricing[k]) == 0 {
				return "0"
			}
			return strings.Trim(string(m.Pricing[k]), `"`)
		}
		evals, fees := 1, 1
		if b.PerQuestion {
			evals = len(r.Questions)
		}
		if b.RequestFeePerQuestion {
			fees = len(r.Questions)
		}
		if m.Context > math.MaxInt/evals {
			return modelapi.Quote{}, errors.New("quote overflow")
		}
		cost, e := modelapi.QuoteCost(rate("prompt"), "0", "0", m.Context*evals, 0)
		if e != nil {
			return modelapi.Quote{}, e
		}
		fee, e := modelapi.QuoteCost(rate("request"), "0", "0", fees, 0)
		if e != nil || cost > math.MaxInt64-fee {
			return modelapi.Quote{}, errors.New("quote overflow")
		}
		quote := modelapi.Quote{Decision: true, PerQuestion: b.PerQuestion, RequestFeePerQuestion: b.RequestFeePerQuestion, MaxRequestBytes: min(b.MaxRequestBytes, systemone.RequestBudget), MaxQuestions: b.MaxQuestions, BoundReference: b.Reference, Model: r.Model, Endpoint: "systemone", InputRate: rate("prompt"), OutputRate: "0", RequestRate: rate("request"), MaxInput: m.Context, Reserved: cost + fee, FetchedAt: m.FetchedAt}
		if len(r.Questions) == 0 || len(raw) > min(b.MaxRequestBytes, systemone.RequestBudget) || (b.MaxQuestions > 0 && len(r.Questions) > b.MaxQuestions) {
			return quote, &modelapi.ProviderError{Code: "request_too_large", Uncharged: true}
		}
		return quote, nil
	}
	return modelapi.Quote{}, &modelapi.ProviderError{Code: "decision_model_unavailable", Uncharged: true}
}
func (c *Client) Decide(ctx context.Context, key string, r systemone.Request, q modelapi.Quote) (modelapi.Result, error) {
	if q.Model != r.Model || time.Since(q.FetchedAt) > 15*time.Minute {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "stale_quote", Uncharged: true}
	}
	// Revalidate against fresh metadata immediately before dispatch. A changed
	// bound/price cannot consume more than the reservation captured by the gateway.
	fresh, e := c.QuoteDecision(ctx, r)
	if e != nil {
		return modelapi.Result{}, e
	}
	if fresh.Reserved > q.Reserved {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "stale_quote", Uncharged: true}
	}
	wire, e := systemone.Canonical(systemone.Raw(r))
	if e != nil {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "invalid_request", Uncharged: true}
	}
	raw, callErr := c.request(ctx, "POST", "/systemone", key, json.RawMessage(wire))
	var body struct {
		ID      string          `json:"id"`
		Answers json.RawMessage `json:"answers"`
		Usage   json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "response_uncertain"}
	}
	result := modelapi.Result{Usage: body.Usage}
	if generationID.MatchString(body.ID) {
		result.GenerationID = body.ID
	}
	if len(body.Answers) <= systemone.ResultBudget && json.Valid(body.Answers) && string(body.Answers) != "null" {
		result.Text = string(body.Answers)
	}
	var usage struct {
		Cost json.RawMessage `json:"cost"`
	}
	if json.Unmarshal(body.Usage, &usage) == nil && len(usage.Cost) > 0 && string(usage.Cost) != "null" {
		n, e := modelapi.Cost(usage.Cost)
		if e != nil {
			return result, &modelapi.ProviderError{Code: "cost_uncertain"}
		}
		result.Amount = &n
	}
	// HTTP statuses alone do not establish that a decision was uncharged.
	if callErr != nil {
		return result, &modelapi.ProviderError{Code: "response_uncertain"}
	}
	if result.Text == "" {
		return result, &modelapi.ProviderError{Code: "response_uncertain"}
	}
	return result, nil
}
func (c *Client) DecisionLookupSupported(model string) bool {
	b, ok := c.bound(model)
	return ok && b.LookupSupported
}
