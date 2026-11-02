// Package openrouter implements bounded text and System One inference.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const DiagnosticPrompt = "Reply with the word OK."
const maxBody = 2 << 20

var generationID = regexp.MustCompile(`^gen-[0-9A-Za-z-]{1,124}$`)

type Client struct {
	HTTP            *http.Client
	BaseURL         string // Test override only; production constructor always uses OpenRouter.
	Model, Endpoint string
	mu              sync.Mutex
	cached          modelapi.Quote
	models          []DecisionModel
	modelsAt        time.Time
	// DecisionBounds is for fake HTTP tests only. Production uses ownerSelectedBound.
	DecisionBounds map[string]BillingBound
}

func New(model, endpoint string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		BaseURL: "https://openrouter.ai/api/v1", Model: model, Endpoint: endpoint}
}
func (c *Client) request(ctx context.Context, method, path, key string, body any) ([]byte, error) {
	ctx, span := otel.Tracer("zapply/openrouter").Start(ctx, "openrouter "+method, trace.WithAttributes(attribute.String("server.address", "openrouter.ai"), attribute.String("http.request.method", method)))
	defer span.End()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &modelapi.ProviderError{Code: "transport_uncertain"}
	}
	defer res.Body.Close()
	span.SetAttributes(attribute.Int("http.response.status_code", res.StatusCode))
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil || len(b) > maxBody {
		return nil, &modelapi.ProviderError{Code: "response_uncertain"}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		code := "provider_error"
		uncharged := false
		switch res.StatusCode {
		case 400, 401, 404:
			code = "request_rejected"
			uncharged = true
		case 402:
			code = "provider_balance"
			uncharged = true
		case 429:
			code = "rate_limited"
			uncharged = true
		}
		retry := time.Minute
		if n, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && n > 0 {
			retry = time.Duration(n) * time.Second
		}
		if t, err := http.ParseTime(res.Header.Get("Retry-After")); err == nil && time.Until(t) > 0 {
			retry = time.Until(t)
		}
		return b, &modelapi.ProviderError{Code: code, Uncharged: uncharged, RetryAfter: retry}
	}
	return b, nil
}

// diagnosticMessages is the whole diagnostic chat call.
var diagnosticMessages = []map[string]string{{"role": "user", "content": DiagnosticPrompt}}

// chatPrior assumes one token per byte, safe for any tokenizer, plus message
// template overhead, until settlements calibrate a model.
var chatPrior = billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 64, OutputPerQ: 256}

var modelID = regexp.MustCompile(`^[a-zA-Z0-9_.:-]+/[a-zA-Z0-9_.:-]+$`)

// ChatSheet describes one provider endpoint of a chat model. Only endpoints
// that honour max_tokens qualify: it caps output, reasoning included, so
// output can be priced and reserved at the cap.
func (c *Client) ChatSheet(ctx context.Context, model, endpoint string) (billing.Sheet, error) {
	if !modelID.MatchString(model) {
		return billing.Sheet{}, errors.New("invalid model identifier")
	}
	raw, err := c.request(ctx, "GET", "/models/"+model+"/endpoints", "", nil)
	if err != nil {
		return billing.Sheet{}, err
	}
	var metadata struct {
		Data struct {
			Endpoints []struct {
				Tag       string                     `json:"tag"`
				Context   int                        `json:"context_length"`
				MaxOutput *int                       `json:"max_completion_tokens"`
				Pricing   map[string]json.RawMessage `json:"pricing"`
				Supported []string                   `json:"supported_parameters"`
			} `json:"endpoints"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &metadata) != nil {
		return billing.Sheet{}, &modelapi.ProviderError{Code: "invalid_pricing", Uncharged: true}
	}
	for _, e := range metadata.Data.Endpoints {
		if e.Tag != endpoint || e.Context <= 0 || !slices.Contains(e.Supported, "max_tokens") {
			continue
		}
		maxOutput := e.Context
		if e.MaxOutput != nil {
			maxOutput = min(maxOutput, *e.MaxOutput)
		}
		if maxOutput < 1 {
			continue
		}
		s := billing.Sheet{Provider: "openrouter", Model: model, Context: e.Context, MaxOutput: maxOutput, OutputEnforced: true,
			Prior: chatPrior, FetchedAt: time.Now().UTC()}
		if reason := price(&s, e.Pricing); reason != "" {
			return billing.Sheet{}, &modelapi.ProviderError{Code: reason, Uncharged: true}
		}
		return s, nil
	}
	return billing.Sheet{}, &modelapi.ProviderError{Code: "model_endpoint_unavailable", Uncharged: true}
}

func (c *Client) Quote(ctx context.Context) (modelapi.Quote, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Model == "" || c.Endpoint == "" {
		return modelapi.Quote{}, &modelapi.ProviderError{Code: "model_not_configured", Uncharged: true}
	}
	if time.Since(c.cached.FetchedAt) < 15*time.Minute {
		return c.cached, nil
	}
	q, err := modelapi.QuoteChat(ctx, c, modelapi.ChatRequest{Model: c.Model, Endpoint: c.Endpoint, Messages: diagnosticMessages, MaxOutput: 128}, billing.Calibration{})
	if err != nil {
		return modelapi.Quote{}, err
	}
	c.cached = q
	return q, nil
}

func million(s string) json.Number {
	r, _ := new(big.Rat).SetString(s)
	r.Mul(r, big.NewRat(1_000_000, 1))
	return json.Number(r.FloatString(12))
}
func (c *Client) Generate(ctx context.Context, key string, q modelapi.Quote) (modelapi.Result, error) {
	return c.GenerateMessages(ctx, key, q, diagnosticMessages)
}

// GenerateMessages uses the same bounded endpoint and cost accounting as diagnostic calls.
// Callers must reserve allowance for the quoted request before invoking it.
func (c *Client) GenerateMessages(ctx context.Context, key string, q modelapi.Quote, messages any) (modelapi.Result, error) {
	if time.Since(q.FetchedAt) > 15*time.Minute {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "stale_quote", Uncharged: true}
	}
	raw, err := c.request(ctx, "POST", "/chat/completions", key, map[string]any{
		"model": q.Model, "messages": messages, "stream": false, "max_tokens": q.MaxOutput,
		"provider": map[string]any{"only": []string{q.Endpoint}, "allow_fallbacks": false, "require_parameters": true,
			"max_price": map[string]any{"prompt": million(q.InputRate), "completion": million(q.OutputRate), "request": json.Number(q.RequestRate)}},
	})
	var response struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return modelapi.Result{}, &modelapi.ProviderError{Code: "response_uncertain"}
	}
	if !regexp.MustCompile(`^gen-[0-9A-Za-z-]{1,124}$`).MatchString(response.ID) {
		response.ID = ""
	}
	r := modelapi.Result{GenerationID: response.ID, Usage: response.Usage}
	if len(response.Choices) > 0 && len(response.Choices[0].Message.Content) <= 16000 {
		r.Text = response.Choices[0].Message.Content
	}
	var usage struct {
		Cost       json.RawMessage `json:"cost"`
		Prompt     int             `json:"prompt_tokens"`
		Completion int             `json:"completion_tokens"`
	}
	if json.Unmarshal(response.Usage, &usage) == nil && len(usage.Cost) > 0 && string(usage.Cost) != "null" {
		cost, err := modelapi.Cost(usage.Cost)
		if err != nil {
			return r, &modelapi.ProviderError{Code: "cost_uncertain"}
		}
		r.Amount = &cost
	}
	if err != nil {
		var pe *modelapi.ProviderError
		if errors.As(err, &pe) {
			// Account for explicit cost even on non-2xx errors. A generation or
			// processed tokens means that an HTTP error is not proof of no charge.
			if len(response.Error) == 0 || string(response.Error) == "null" || r.Amount != nil || r.GenerationID != "" || usage.Prompt > 0 || usage.Completion > 0 {
				pe.Uncharged = false
			}
			var envelope struct {
				Metadata struct {
					LimitSource string `json:"limit_source"`
				} `json:"metadata"`
			}
			if json.Unmarshal(response.Error, &envelope) == nil && r.Amount == nil && r.GenerationID == "" && usage.Prompt == 0 && usage.Completion == 0 &&
				slices.Contains([]string{"openrouter_guardrail_budget", "openrouter_workspace_budget", "openrouter_key_limit"}, envelope.Metadata.LimitSource) {
				pe.Uncharged = true
				pe.Code = "provider_budget"
			}
		}
	}
	return r, err
}
func (c *Client) Lookup(ctx context.Context, key, id string) (modelapi.Result, error) {
	if !regexp.MustCompile(`^gen-[0-9A-Za-z-]{1,124}$`).MatchString(id) {
		return modelapi.Result{}, errors.New("invalid generation ID")
	}
	raw, err := c.request(ctx, "GET", "/generation?id="+url.QueryEscape(id), key, nil)
	if err != nil {
		return modelapi.Result{}, err
	}
	var data struct {
		Data struct {
			Cost json.RawMessage `json:"total_cost"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &data) != nil || len(data.Data.Cost) == 0 || string(data.Data.Cost) == "null" {
		return modelapi.Result{}, errors.New("generation accounting not available")
	}
	cost, err := modelapi.Cost(data.Data.Cost)
	if err != nil {
		return modelapi.Result{}, err
	}
	return modelapi.Result{GenerationID: id, Amount: &cost, Usage: data.Data.Cost}, nil
}
