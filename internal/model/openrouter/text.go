package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
