package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type reasoningOptions struct {
	Mandatory bool     `json:"mandatory"`
	Efforts   []string `json:"supported_efforts"`
}

// Summaries need visible prose within a small output cap. Disable optional
// reasoning; mandatory models use their lowest advertised effort instead.
func (c *Client) summaryReasoning(ctx context.Context, model string) (map[string]any, error) {
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
				Reasoning *reasoningOptions `json:"reasoning"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &catalog); err != nil || len(catalog.Data) == 0 {
			return nil, errors.New("invalid reasoning catalog")
		}
		models := map[string]reasoningOptions{}
		for _, m := range catalog.Data {
			if m.Reasoning != nil {
				models[m.ID] = *m.Reasoning
			}
		}
		c.reasoningModels, c.reasoningAt = models, time.Now()
	}
	options, ok := c.reasoningModels[model]
	if !ok {
		return nil, nil
	}
	if !options.Mandatory {
		return map[string]any{"enabled": false}, nil
	}
	for i := len(options.Efforts) - 1; i >= 0; i-- {
		if options.Efforts[i] != "none" {
			return map[string]any{"effort": options.Efforts[i]}, nil
		}
	}
	return nil, nil
}
