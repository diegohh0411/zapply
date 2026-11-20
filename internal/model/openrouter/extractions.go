package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
)

// extractionsPath is OpenRouter's alpha route for typed field extraction,
// outside the /api/v1 prefix the other calls share.
const extractionsPath = "/alpha/extractions"

// ExtractionSheet prices a catalogue model that serves the extractions route.
// The state is billed once per field: a model may split the fields into
// several passes over the same text.
func (c *Client) ExtractionSheet(ctx context.Context, model string) (billing.Sheet, error) {
	models, e := c.DecisionModels(ctx, "", false)
	if e != nil {
		return billing.Sheet{}, e
	}
	for _, m := range models {
		if m.ID != model {
			continue
		}
		if !slices.Contains(m.Endpoints, "/api"+extractionsPath) {
			return billing.Sheet{}, &modelapi.ProviderError{Code: "extraction_unsupported", Uncharged: true}
		}
		s, reason := c.sheet(m)
		if reason != "" {
			return billing.Sheet{}, &modelapi.ProviderError{Code: reason, Uncharged: true}
		}
		s.MaxRequestBytes = min(s.MaxRequestBytes, extraction.RequestBudget)
		s.InputPerQuestion = true
		return s, nil
	}
	return billing.Sheet{}, &modelapi.ProviderError{Code: "extraction_model_unavailable", Uncharged: true}
}

// Extract sends one extraction request priced by q.
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
	// OpenRouter validates the request before it reaches the provider, so a
	// rejection without a generation is not charged.
	var pe *modelapi.ProviderError
	if errors.As(callErr, &pe) && pe.Code == "request_rejected" {
		var envelope struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.ID == "" {
			return modelapi.Result{}, pe
		}
	}
	return systemOneResult(ctx, extractionsPath, raw, callErr)
}

// url resolves a request path: alpha routes sit beside /api/v1.
func (c *Client) url(path string) string {
	if strings.HasPrefix(path, "/alpha/") {
		return strings.TrimSuffix(c.BaseURL, "/v1") + path
	}
	return c.BaseURL + path
}
