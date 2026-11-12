package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// gatewayEfforts lists every effort OpenRouter accepts, highest first. A
// catalogue entry with supported_efforts set to null accepts all of them.
var gatewayEfforts = []string{"max", "xhigh", "high", "medium", "low", "minimal", "none"}

// ReasoningOptions is the thinking control a model accepts. Efforts use this
// API's vocabulary, highest first: "off" stands for the gateway's "none".
type ReasoningOptions struct {
	Mandatory bool     `json:"mandatory"`
	Efforts   []string `json:"efforts"`
}

// UnmarshalJSON reads the catalogue shape. An omitted supported_efforts means
// the model exposes no effort choice; null means every gateway effort is accepted.
func (o *ReasoningOptions) UnmarshalJSON(b []byte) error {
	var raw struct {
		Mandatory bool            `json:"mandatory"`
		Supported json.RawMessage `json:"supported_efforts"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var accepted []string
	switch {
	case len(raw.Supported) == 0:
	case string(raw.Supported) == "null":
		accepted = gatewayEfforts
	default:
		if err := json.Unmarshal(raw.Supported, &accepted); err != nil {
			return err
		}
	}
	o.Mandatory = raw.Mandatory
	o.Efforts = []string{}
	for _, e := range gatewayEfforts {
		if !slices.Contains(accepted, e) || (e == "none" && o.Mandatory) {
			continue
		}
		if e == "none" {
			e = "off"
		}
		o.Efforts = append(o.Efforts, e)
	}
	return nil
}

// Supports reports whether the catalogue accepts an explicit effort choice.
func (o *ReasoningOptions) Supports(effort string) bool {
	return o != nil && slices.Contains(o.Efforts, effort)
}

// summaryReasoning maps the owner's choice to the request's reasoning object.
// An empty effort is automatic: summaries need visible prose within a small
// output cap, so optional reasoning is disabled and mandatory reasoning uses
// its lowest advertised effort.
func (c *Client) summaryReasoning(ctx context.Context, model, effort string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reasoningModels == nil || time.Since(c.reasoningAt) > 15*time.Minute {
		raw, err := c.request(ctx, "GET", "/models", "", nil)
		if err != nil {
			return nil, err
		}
		var catalog struct {
			Data []struct {
				ID        string            `json:"id"`
				Reasoning *ReasoningOptions `json:"reasoning"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &catalog); err != nil || len(catalog.Data) == 0 {
			return nil, errors.New("invalid reasoning catalog")
		}
		models := map[string]ReasoningOptions{}
		for _, m := range catalog.Data {
			if m.Reasoning != nil {
				models[m.ID] = *m.Reasoning
			}
		}
		c.reasoningModels, c.reasoningAt = models, time.Now()
	}
	options, ok := c.reasoningModels[model]
	if effort != "" {
		if !ok || !slices.Contains(options.Efforts, effort) {
			return nil, errors.New("selected reasoning effort is unavailable")
		}
		if effort == "off" {
			return map[string]any{"enabled": false}, nil
		}
		return map[string]any{"effort": effort}, nil
	}
	if !ok {
		return nil, nil
	}
	if !options.Mandatory {
		return map[string]any{"enabled": false}, nil
	}
	for i := len(options.Efforts) - 1; i >= 0; i-- {
		if options.Efforts[i] != "off" {
			return map[string]any{"effort": options.Efforts[i]}, nil
		}
	}
	return nil, nil
}
