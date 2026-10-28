package model

import (
	"context"
	"encoding/json"
	"time"

	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

type Quote struct {
	Decision        bool      `json:"decision,omitempty"`
	MaxRequestBytes int       `json:"max_request_bytes,omitempty"`
	MaxQuestions    int       `json:"max_questions,omitempty"`
	Model           string    `json:"model"`
	Endpoint        string    `json:"endpoint"`
	InputRate       string    `json:"input_rate"`
	OutputRate      string    `json:"output_rate"`
	RequestRate     string    `json:"request_rate"`
	MaxInput        int       `json:"max_input"`
	MaxOutput       int       `json:"max_output"`
	Reserved        int64     `json:"reserved"`
	FetchedAt       time.Time `json:"fetched_at"`

	// Decision quotes carry everything the prediction used, so an estimate or
	// a revalidation can repeat it without the network.
	Sheet       *billing.Sheet       `json:"sheet,omitempty"`
	Shape       *billing.Shape       `json:"shape,omitempty"`
	Calibration *billing.Calibration `json:"calibration,omitempty"`
	Prediction  *billing.Prediction  `json:"prediction,omitempty"`
}

type Result struct {
	GenerationID string
	Text         string
	Amount       *int64
	Usage        json.RawMessage
}

// ProviderError is deliberately safe for logs: raw responses can contain secrets.
type ProviderError struct {
	Code       string
	Uncharged  bool
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string { return "provider: " + e.Code }

type Provider interface {
	Quote(context.Context) (Quote, error)
	Generate(context.Context, string, Quote) (Result, error)
	Lookup(context.Context, string, string) (Result, error)
}

// DecisionProvider extends the gateway without changing diagnostic providers.
// A provider only describes a model's billing; QuoteDecision predicts from it.
type DecisionProvider interface {
	DecisionSheet(ctx context.Context, model string) (billing.Sheet, error)
	Decide(context.Context, string, systemone.Request, Quote) (Result, error)
	DecisionLookupSupported(string) bool
}
