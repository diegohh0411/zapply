package model

import (
	"context"
	"encoding/json"

	"github.com/diegohh0411/zapply/internal/model/billing"
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

// CalibrationOf reads platform.decision_calibration's JSON. A missing or
// malformed summary falls back to the sheet's prior.
func CalibrationOf(raw []byte) billing.Calibration {
	var c billing.Calibration
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.TokensPerKB < 0 || c.OutputPerQ < 0 || c.CostPermil < 0 {
		return billing.Calibration{}
	}
	return c
}
