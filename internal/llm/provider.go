package llm

import (
	"context"
	"encoding/json"
	"time"
)

type Quote struct {
	Model       string    `json:"model"`
	Endpoint    string    `json:"endpoint"`
	InputRate   string    `json:"input_rate"`
	OutputRate  string    `json:"output_rate"`
	RequestRate string    `json:"request_rate"`
	MaxInput    int       `json:"max_input"`
	MaxOutput   int       `json:"max_output"`
	Reserved    int64     `json:"reserved"`
	FetchedAt   time.Time `json:"fetched_at"`
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
