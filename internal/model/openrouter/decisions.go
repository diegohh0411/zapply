package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

// BillingBound holds the request limits and accounting support a decision
// model has beyond what the catalogue reports.
type BillingBound struct {
	MaxRequestBytes int
	MaxQuestions    int // zero means no documented count limit
	LookupSupported bool
}

// ownerSelectedBound applies to any model the owner selects.
var ownerSelectedBound = BillingBound{MaxRequestBytes: systemone.RequestBudget}

// Decision models bill input once per request (OpenRouter's System One docs;
// settled usage agrees: 96 questions on a 62 KB request billed 12,705 input
// tokens) and the request fee once per question. The priors assume one token
// per byte, about 5× the observed rate, until settlements calibrate a model.
var decisionPrior = billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 1024, OutputPerQ: 16}

// componentRoles classifies the catalogue's pricing keys. The input rate is the
// highest of its components. Decision calls send text and return JSON, so audio
// and image charges never apply. Output is unbounded: System One takes no
// output-token limit, and hidden reasoning need not appear in the answers, so a
// positive output rate makes the model incompatible. Any other positive
// component, web_search included, is a charge the reservation cannot bound.
var componentRoles = map[string]string{
	"prompt":             billing.RoleInput,
	"input_cache_read":   billing.RoleInput,
	"input_cache_write":  billing.RoleInput,
	"completion":         billing.RoleOutput,
	"internal_reasoning": billing.RoleOutput,
	"request":            billing.RoleRequest,
	"audio":              billing.RoleUnused,
	"input_audio":        billing.RoleUnused,
	"input_audio_cache":  billing.RoleUnused,
	"output_audio":       billing.RoleUnused,
	"image":              billing.RoleUnused,
	"image_output":       billing.RoleUnused,
	"discount":           billing.RoleDiscount,
}

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
func (c *Client) bound(model string) BillingBound {
	if b, ok := c.DecisionBounds[model]; ok {
		return b
	}
	return ownerSelectedBound
}
func (c *Client) compatibility(m DecisionModel) string {
	_, reason := c.sheet(m)
	return reason
}

// sheet prices every catalogue component or names the one it cannot price.
func (c *Client) sheet(m DecisionModel) (billing.Sheet, string) {
	if m.Context <= 0 {
		return billing.Sheet{}, "invalid_context"
	}
	b := c.bound(m.ID)
	s := billing.Sheet{Provider: "openrouter", Model: m.ID, RequestPerQuestion: true, Context: m.Context,
		MaxRequestBytes: min(b.MaxRequestBytes, systemone.RequestBudget),
		MaxQuestions:    b.MaxQuestions, Prior: decisionPrior, FetchedAt: m.FetchedAt, Components: []billing.Component{}}
	names := slices.Sorted(maps.Keys(m.Pricing))
	for _, name := range names {
		value := strings.Trim(string(m.Pricing[name]), `"`)
		role, known := componentRoles[name]
		if role == billing.RoleDiscount {
			// A discount never lowers a rate the reservation relies on.
			s.Components = append(s.Components, billing.Component{Name: name, Rate: value, Role: role})
			continue
		}
		rate, e := modelapi.ParseUSD(value)
		if e != nil {
			return billing.Sheet{}, "invalid_pricing_" + name
		}
		if rate == 0 {
			role = billing.RoleZero
		} else if !known {
			return billing.Sheet{}, "unbounded_pricing_" + name
		}
		switch role {
		case billing.RoleInput:
			s.InputRate = max(s.InputRate, rate)
		case billing.RoleOutput:
			return billing.Sheet{}, "unbounded_pricing_" + name
		case billing.RoleRequest:
			s.RequestRate = rate
		}
		s.Components = append(s.Components, billing.Component{Name: name, Rate: value, Role: role})
	}
	return s, ""
}

// DecisionSheet describes a catalogue model's billing for the gateway.
func (c *Client) DecisionSheet(ctx context.Context, model string) (billing.Sheet, error) {
	models, e := c.DecisionModels(ctx, "", false)
	if e != nil {
		return billing.Sheet{}, e
	}
	for _, m := range models {
		if m.ID != model {
			continue
		}
		s, reason := c.sheet(m)
		if reason != "" {
			return billing.Sheet{}, &modelapi.ProviderError{Code: reason, Uncharged: true}
		}
		return s, nil
	}
	return billing.Sheet{}, &modelapi.ProviderError{Code: "decision_model_unavailable", Uncharged: true}
}
func (c *Client) Decide(ctx context.Context, key string, r systemone.Request, q modelapi.Quote) (modelapi.Result, error) {
	if q.Model != r.Model || time.Since(q.FetchedAt) > 15*time.Minute {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "stale_quote", Uncharged: true}
	}
	// Revalidate against fresh metadata immediately before dispatch. A changed
	// bound/price cannot consume more than the reservation captured by the gateway.
	var calibration billing.Calibration
	if q.Calibration != nil {
		calibration = *q.Calibration
	}
	fresh, e := modelapi.QuoteDecision(ctx, c, r, calibration)
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
	return c.bound(model).LookupSupported
}
