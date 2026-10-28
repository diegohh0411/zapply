package openrouter

import (
	"encoding/json"
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
// requests must price them first. Any other positive component, web_search
// included, is a charge the reservation cannot bound.
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
	"discount":           billing.RoleDiscount,
}

// price fills the sheet's rates from a catalogue pricing object and returns
// the compatibility reason for the first component it cannot price. Output is
// priced only when the sheet's endpoint enforces an output cap.
func price(s *billing.Sheet, pricing map[string]json.RawMessage) string {
	s.Components = []billing.Component{}
	for _, name := range slices.Sorted(maps.Keys(pricing)) {
		value := strings.Trim(string(pricing[name]), `"`)
		role, known := componentRoles[name]
		if role == billing.RoleDiscount {
			// A discount never lowers a rate the reservation relies on.
			s.Components = append(s.Components, billing.Component{Name: name, Rate: value, Role: role})
			continue
		}
		rate, e := modelapi.ParseUSD(value)
		if e != nil {
			return "invalid_pricing_" + name
		}
		if rate == 0 {
			role = billing.RoleZero
		} else if !known || (role == billing.RoleOutput && !s.OutputEnforced) {
			return "unbounded_pricing_" + name
		}
		switch role {
		case billing.RoleInput:
			s.InputRate = max(s.InputRate, rate)
		case billing.RoleOutput:
			s.OutputRate = max(s.OutputRate, rate)
		case billing.RoleRequest:
			s.RequestRate = rate
		}
		s.Components = append(s.Components, billing.Component{Name: name, Rate: value, Role: role})
	}
	return ""
}
