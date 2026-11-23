package model

import (
	"context"
	"encoding/json"

	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
)

// ExtractionProvider serves extraction models. Any provider that implements
// it can be selected; callers only see extraction.Request and answers.
type ExtractionProvider interface {
	ExtractionSheet(ctx context.Context, model string) (billing.Sheet, error)
	Extract(ctx context.Context, key string, r extraction.Request, q Quote) (Result, error)
}

// QuoteExtraction prices one extraction request. Providers may split fields
// into passes that each read the whole state, so input is priced once per
// field: never below what is billed.
func QuoteExtraction(ctx context.Context, p ExtractionProvider, r extraction.Request, c billing.Calibration) (Quote, error) {
	if r.Model == "" {
		return Quote{}, &ProviderError{Code: "model_not_configured", Uncharged: true}
	}
	if extraction.Validate(r) != nil {
		return Quote{}, &ProviderError{Code: "invalid_request", Uncharged: true}
	}
	sheet, e := p.ExtractionSheet(ctx, r.Model)
	if e != nil {
		return Quote{}, e
	}
	raw, e := json.Marshal(r)
	if e != nil {
		return Quote{}, e
	}
	shape := billing.Shape{RequestBytes: len(raw), Questions: len(r.Fields)}
	prediction, e := billing.Predict(sheet, shape, c)
	if e != nil {
		return Quote{}, e
	}
	quote := Quote{Model: r.Model, Endpoint: "extractions", MaxRequestBytes: sheet.MaxRequestBytes,
		MaxInput: sheet.Context, Reserved: prediction.Reserved, FetchedAt: sheet.FetchedAt,
		Sheet: &sheet, Shape: &shape, Calibration: &c, Prediction: &prediction}
	if sheet.MaxRequestBytes > 0 && len(raw) > sheet.MaxRequestBytes {
		return quote, &ProviderError{Code: "request_too_large", Uncharged: true}
	}
	return quote, nil
}
