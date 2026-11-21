package model

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

// QuoteDecision prices one decision request from the provider's billing sheet
// and the model's calibration. A request over the sheet's limits returns the
// quote with request_too_large so the caller can fail it without retrying.
func QuoteDecision(ctx context.Context, p DecisionProvider, r systemone.Request, c billing.Calibration) (Quote, error) {
	if r.Model == "" {
		return Quote{}, &ProviderError{Code: "model_not_configured", Uncharged: true}
	}
	if len(r.Questions) == 0 {
		return Quote{}, &ProviderError{Code: "request_too_large", Uncharged: true}
	}
	sheet, e := p.DecisionSheet(ctx, r.Model)
	if e != nil {
		return Quote{}, e
	}
	raw, e := json.Marshal(r)
	if e != nil {
		return Quote{}, e
	}
	maxBytes := systemone.RequestBudget
	if sheet.MaxRequestBytes > 0 {
		maxBytes = min(maxBytes, sheet.MaxRequestBytes)
	}
	shape := billing.Shape{RequestBytes: len(raw), Questions: len(r.Questions)}
	prediction, e := billing.Predict(sheet, shape, c)
	if e != nil {
		return Quote{}, e
	}
	quote := Quote{Decision: true, MaxRequestBytes: maxBytes, MaxQuestions: sheet.MaxQuestions, Model: r.Model, Endpoint: "systemone",
		MaxInput: sheet.Context, MaxOutput: sheet.MaxOutput, Reserved: prediction.Reserved, FetchedAt: sheet.FetchedAt,
		Sheet: &sheet, Shape: &shape, Calibration: &c, Prediction: &prediction}
	if len(raw) > maxBytes || (sheet.MaxQuestions > 0 && len(r.Questions) > sheet.MaxQuestions) {
		return quote, &ProviderError{Code: "request_too_large", Uncharged: true}
	}
	return quote, nil
}

// QuoteExtraction prices one extraction request like a decision: each field
// counts as a question, and the sheet bills the state once per field, which
// bounds an extraction split into one pass per field.
func QuoteExtraction(ctx context.Context, p ExtractionProvider, r extraction.Request, c billing.Calibration) (Quote, error) {
	if r.Model == "" {
		return Quote{}, &ProviderError{Code: "model_not_configured", Uncharged: true}
	}
	if extraction.Validate(r) != nil {
		return Quote{}, &ProviderError{Code: "request_too_large", Uncharged: true}
	}
	sheet, e := p.ExtractionSheet(ctx, r.Model)
	if e != nil {
		return Quote{}, e
	}
	raw, e := json.Marshal(r)
	if e != nil {
		return Quote{}, e
	}
	maxBytes := extraction.RequestBudget
	if sheet.MaxRequestBytes > 0 {
		maxBytes = min(maxBytes, sheet.MaxRequestBytes)
	}
	shape := billing.Shape{RequestBytes: len(raw), Questions: len(r.Fields)}
	prediction, e := billing.Predict(sheet, shape, c)
	if e != nil {
		return Quote{}, e
	}
	quote := Quote{Decision: true, MaxRequestBytes: maxBytes, Model: r.Model, Endpoint: "extractions",
		MaxInput: sheet.Context, MaxOutput: sheet.MaxOutput, Reserved: prediction.Reserved, FetchedAt: sheet.FetchedAt,
		Sheet: &sheet, Shape: &shape, Calibration: &c, Prediction: &prediction}
	if len(raw) > maxBytes {
		return quote, &ProviderError{Code: "request_too_large", Uncharged: true}
	}
	return quote, nil
}

// CalibrationOf reads platform.model_calibration's JSON. A missing or
// malformed summary falls back to the sheet's prior.
func CalibrationOf(raw []byte) billing.Calibration {
	var c billing.Calibration
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.TokensPerKB < 0 || c.OutputPerQ < 0 || c.CostPermil < 0 {
		return billing.Calibration{}
	}
	return c
}

// ChatPricer describes a chat-completion endpoint's billing. Any provider that
// implements it gets the same prediction decisions use.
type ChatPricer interface {
	ChatSheet(ctx context.Context, model, endpoint string) (billing.Sheet, error)
}

// ChatRequest is what QuoteChat needs to know about a chat call before it is
// sent. Messages is measured as JSON; MaxOutput is the max_tokens the call
// will send, which the reservation holds in full.
type ChatRequest struct {
	Model, Endpoint string
	Messages        any
	MaxOutput       int
	ReasoningEffort string
}

// QuoteChat prices one chat call. The quote's rate strings feed the provider's
// per-request price ceiling, so routing cannot pick a pricier upstream.
func QuoteChat(ctx context.Context, p ChatPricer, r ChatRequest, c billing.Calibration) (Quote, error) {
	if r.Model == "" || r.Endpoint == "" {
		return Quote{}, &ProviderError{Code: "model_not_configured", Uncharged: true}
	}
	if r.MaxOutput <= 0 {
		return Quote{}, errors.New("chat quote needs max_tokens")
	}
	sheet, e := p.ChatSheet(ctx, r.Model, r.Endpoint)
	if e != nil {
		return Quote{}, e
	}
	raw, e := json.Marshal(r.Messages)
	if e != nil {
		return Quote{}, e
	}
	shape := billing.Shape{RequestBytes: len(raw), Questions: 1, MaxOutput: r.MaxOutput}
	prediction, e := billing.Predict(sheet, shape, c)
	if e != nil {
		return Quote{}, e
	}
	return Quote{Model: r.Model, Endpoint: r.Endpoint, InputRate: USD(sheet.InputRate), OutputRate: USD(sheet.OutputRate), RequestRate: USD(sheet.RequestRate),
		MaxInput: sheet.Context, MaxOutput: min(sheet.MaxOutput, r.MaxOutput), ReasoningEffort: r.ReasoningEffort, Reserved: prediction.Reserved, FetchedAt: sheet.FetchedAt,
		Sheet: &sheet, Shape: &shape, Calibration: &c, Prediction: &prediction}, nil
}
