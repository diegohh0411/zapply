package openrouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
)

// componentRoles classifies OpenRouter's pricing keys for chat and decision
// endpoints alike. Input and output rates are the highest of their components.
// Reasoning bills as output and counts toward max_tokens. Calls send text and
// receive text or JSON, so audio and image charges never apply; multimodal
// requests must price them first. web_search is a server-side tool our calls
// never enable, so it is never billed. Any other positive component is a
// charge the reservation cannot bound.
var componentRoles = map[string]string{
	"prompt":             billing.RoleInput,
	"input_cache_read":   billing.RoleInput,
	"input_cache_write":  billing.RoleInput,
	"completion":         billing.RoleOutput,
	"internal_reasoning": billing.RoleOutput,
	"request":            billing.RoleRequest,
	"audio":              billing.RoleUnused,
	"input_audio":        billing.RoleUnused,
	"input_audio_cache":  billing.RoleUnused,
	"output_audio":       billing.RoleUnused,
	"image":              billing.RoleUnused,
	"image_output":       billing.RoleUnused,
	"web_search":         billing.RoleUnused,
	"discount":           billing.RoleDiscount,
}

// price fills the sheet's rates from a catalogue pricing object and returns
// the compatibility reason for the first component it cannot price. Output is
// priced only when the sheet's endpoint enforces an output cap.
func price(s *billing.Sheet, pricing map[string]json.RawMessage) string {
	s.Components = []billing.Component{}
	for _, name := range slices.Sorted(maps.Keys(pricing)) {
		if name == "overrides" {
			if reason := priceOverrides(s, pricing[name]); reason != "" {
				return reason
			}
			continue
		}
		if reason := priceComponent(s, name, name, pricing[name]); reason != "" {
			return reason
		}
	}
	return ""
}

// priceOverrides folds OpenRouter's long-prompt pricing into the flat rates.
// An override applies once a prompt reaches min_prompt_tokens; one that no
// prompt within the endpoint's context can reach is skipped. Each applicable
// override is priced like a base component, so the sheet keeps the highest rate.
func priceOverrides(s *billing.Sheet, raw json.RawMessage) string {
	var overrides []map[string]json.RawMessage
	if json.Unmarshal(raw, &overrides) != nil {
		return "invalid_pricing_overrides"
	}
	for i, override := range overrides {
		var threshold int
		if json.Unmarshal(override["min_prompt_tokens"], &threshold) != nil || threshold < 0 {
			return "invalid_pricing_overrides"
		}
		if s.Context > 0 && threshold >= s.Context {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(override)) {
			if name == "min_prompt_tokens" {
				continue
			}
			label := fmt.Sprintf("overrides[%d].%s", i, name)
			if reason := priceComponent(s, name, label, override[name]); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// priceComponent prices one component. key selects its role; label names it in
// the sheet and in any reason returned.
func priceComponent(s *billing.Sheet, key, label string, raw json.RawMessage) string {
	if t := bytes.TrimSpace(raw); len(t) > 0 && (t[0] == '[' || t[0] == '{') {
		return "unsupported_pricing_" + label
	}
	value := strings.Trim(string(raw), `"`)
	role, known := componentRoles[key]
	if role == billing.RoleDiscount {
		// A discount never lowers a rate the reservation relies on.
		s.Components = append(s.Components, billing.Component{Name: label, Rate: value, Role: role})
		return ""
	}
	rate, e := modelapi.ParseUSD(value)
	if e != nil {
		return "invalid_pricing_" + label
	}
	if rate == 0 {
		role = billing.RoleZero
	} else if !known || (role == billing.RoleOutput && !s.OutputEnforced) {
		return "unbounded_pricing_" + label
	}
	switch role {
	case billing.RoleInput:
		s.InputRate = max(s.InputRate, rate)
	case billing.RoleOutput:
		s.OutputRate = max(s.OutputRate, rate)
	case billing.RoleRequest:
		s.RequestRate = rate
	}
	s.Components = append(s.Components, billing.Component{Name: label, Rate: value, Role: role})
	return ""
}
