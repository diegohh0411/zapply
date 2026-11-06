package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"slices"
	"strings"
)

func (c *Client) TextModels(ctx context.Context, key string) ([]DecisionModel, error) {
	raw, err := c.request(ctx, "GET", "/models", key, nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data []DecisionModel `json:"data"`
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return nil, errors.New("invalid model catalogue")
	}
	out := []DecisionModel{}
	for _, m := range response.Data {
		if slices.Contains(m.Architecture.Output, "text") {
			out = append(out, m)
		}
	}
	return out, nil
}

// TextEndpoint is a selectable provider route whose output and pricing can be bounded.
type TextEndpoint struct {
	Tag         string `json:"tag"`
	Name        string `json:"name"`
	ServiceTier string `json:"service_tier"`
}

func (c *Client) TextEndpoints(ctx context.Context, model string) ([]TextEndpoint, error) {
	if !modelID.MatchString(model) || slices.Contains(strings.Split(model, "/"), "..") || slices.Contains(strings.Split(model, "/"), ".") {
		return nil, errors.New("invalid model identifier")
	}
	raw, err := c.request(ctx, "GET", "/models/"+model+"/endpoints", "", nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data struct {
			Endpoints []struct {
				Tag         string                     `json:"tag"`
				ServiceTier string                     `json:"service_tier"`
				Name        string                     `json:"name"`
				Provider    string                     `json:"provider_name"`
				Context     int                        `json:"context_length"`
				MaxOutput   *int                       `json:"max_completion_tokens"`
				Supported   []string                   `json:"supported_parameters"`
				Pricing     map[string]json.RawMessage `json:"pricing"`
			} `json:"endpoints"`
		} `json:"data"`
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return nil, errors.New("invalid endpoint catalogue")
	}
	out := []TextEndpoint{}
	seen := map[string]bool{}
	for _, e := range response.Data.Endpoints {
		if e.Tag == "" || seen[e.Tag] || e.Context <= 0 || !slices.Contains(e.Supported, "max_tokens") || (e.MaxOutput != nil && *e.MaxOutput < 1) {
			continue
		}
		sheet := billing.Sheet{OutputEnforced: true}
		if price(&sheet, e.Pricing) != "" {
			continue
		}
		name := e.Name
		if name == "" {
			name = e.Provider
		}
		if name == "" {
			name = e.Tag
		}
		tier := e.ServiceTier
		if tier == "" || tier == "standard" {
			tier = "default"
		}
		if tier == "fast" {
			tier = "priority"
		}
		out = append(out, TextEndpoint{Tag: e.Tag, Name: name, ServiceTier: tier})
		seen[e.Tag] = true
	}
	return out, nil
}
