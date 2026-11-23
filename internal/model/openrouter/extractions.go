package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
)

// extractionsPath is OpenRouter's alpha Extractions route, which lives beside
// /api/v1 rather than under it.
const extractionsPath = alphaPrefix + "/extractions"

// extractionPrior assumes one token per byte until settlements calibrate a
// model. Extraction models bill no output.
var extractionPrior = billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 1024}

// fallbackContext caps the reservation when the catalogue omits a context
// length. It only bounds the ceiling; the prediction comes from the request.
const fallbackContext = 1 << 20

// ExtractionModel is a catalogue entry served on the Extractions route.
type ExtractionModel struct {
	FetchedAt     time.Time                  `json:"-"`
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Context       int                        `json:"context_length"`
	Pricing       map[string]json.RawMessage `json:"pricing"`
	Compatibility string                     `json:"compatibility_reason"`
	// Typed reports whether number fields are answered as numbers; other
	// models are asked for text, which callers parse.
	Typed bool `json:"typed"`
}

// ExtractionModels lists the models served on the Extractions route, cached
// for fifteen minutes.
func (c *Client) ExtractionModels(ctx context.Context, refresh bool) ([]ExtractionModel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !refresh && time.Since(c.extractionAt) < 15*time.Minute {
		return slices.Clone(c.extractionModels), nil
	}
	raw, err := c.request(ctx, "GET", "/models?endpoints=%2Fapi%2Falpha%2Fextractions", "", nil)
	if err != nil {
		return nil, err
	}
	var body struct {
		Data []ExtractionModel `json:"data"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("invalid model catalogue")
	}
	now := time.Now().UTC()
	models := make([]ExtractionModel, 0, len(body.Data))
	for _, m := range body.Data {
		if !modelID.MatchString(m.ID) {
			continue
		}
		m.FetchedAt = now
		_, m.Compatibility = extractionSheet(m)
		m.Typed = extraction.ProfileOf(m.ID).Number
		models = append(models, m)
	}
	c.extractionModels, c.extractionAt = models, time.Now()
	return slices.Clone(models), nil
}

// extractionSheet prices a catalogue model, or names what it cannot price.
func extractionSheet(m ExtractionModel) (billing.Sheet, string) {
	ctx := m.Context
	if ctx <= 0 {
		ctx = fallbackContext
	}
	s := billing.Sheet{Provider: "openrouter", Model: m.ID, Context: ctx, InputPerQuestion: true,
		MaxRequestBytes: extraction.RequestBudget, Prior: extractionPrior, FetchedAt: m.FetchedAt}
	// Output is free on extraction models; a priced output cannot be bounded.
	if reason := price(&s, m.Pricing); reason != "" {
		return billing.Sheet{}, reason
	}
	return s, ""
}

// ExtractionSheet describes a catalogue model's billing for the gateway.
func (c *Client) ExtractionSheet(ctx context.Context, model string) (billing.Sheet, error) {
	models, e := c.ExtractionModels(ctx, false)
	if e != nil {
		return billing.Sheet{}, e
	}
	for _, m := range models {
		if m.ID != model {
			continue
		}
		s, reason := extractionSheet(m)
		if reason != "" {
			return billing.Sheet{}, &modelapi.ProviderError{Code: reason, Uncharged: true}
		}
		return s, nil
	}
	return billing.Sheet{}, &modelapi.ProviderError{Code: "extraction_model_unavailable", Uncharged: true}
}

// Extract sends one extraction request. HTTP statuses alone do not establish
// that a call was uncharged once it was sent; an error envelope without any
// billing evidence does (see confirmUncharged).
func (c *Client) Extract(ctx context.Context, key string, r extraction.Request, q modelapi.Quote) (modelapi.Result, error) {
	if q.Model != r.Model || time.Since(q.FetchedAt) > 15*time.Minute {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "stale_quote", Uncharged: true}
	}
	var calibration billing.Calibration
	if q.Calibration != nil {
		calibration = *q.Calibration
	}
	fresh, e := modelapi.QuoteExtraction(ctx, c, r, calibration)
	if e != nil {
		return modelapi.Result{}, e
	}
	if fresh.Reserved > q.Reserved {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "stale_quote", Uncharged: true}
	}
	raw, callErr := c.request(ctx, "POST", extractionsPath, key, r)
	var pe *modelapi.ProviderError
	if errors.As(callErr, &pe) && pe.Code == "request_rejected" {
		// OpenRouter validates fields before any provider sees them.
		return modelapi.Result{}, callErr
	}
	var body struct {
		ID      string          `json:"id"`
		Answers json.RawMessage `json:"answers"`
		Usage   json.RawMessage `json:"usage"`
	}
	parseErr := json.Unmarshal(raw, &body)
	logResponse(ctx, "/alpha/extractions", raw, parseErr)
	if parseErr != nil {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "response_uncertain"}
	}
	result := modelapi.Result{Usage: body.Usage}
	if generationID.MatchString(body.ID) {
		result.GenerationID = body.ID
	}
	if len(body.Answers) <= extraction.ResultBudget && json.Valid(body.Answers) && string(body.Answers) != "null" {
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
	if callErr != nil {
		// A rejection carrying no generation ID or usage was not billed.
		confirmUncharged(raw, callErr)
		if errors.As(callErr, &pe) && pe.Uncharged {
			return result, callErr
		}
		return result, &modelapi.ProviderError{Code: "response_uncertain"}
	}
	if result.Text == "" {
		return result, &modelapi.ProviderError{Code: "response_uncertain"}
	}
	return result, nil
}
