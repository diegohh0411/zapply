// Package extraction is the provider-independent contract for extraction
// models: a text state and named, typed fields in; one typed answer per field
// out. Callers describe what they need once; Profile says which field types a
// model answers, so a caller can ask a weaker model for text and parse it.
package extraction

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Field types every extraction model shares, and the typed ones only some do.
const (
	Text   = "text"
	Number = "number"
	Date   = "date"
	List   = "list"
)

// RequestBudget bounds a request body; a posting is cut to fit before sending.
const RequestBudget = 256 << 10

// ResultBudget bounds the answers kept from a response.
const ResultBudget = 64 << 10

// Field is one value to extract. Items is the element type of a list.
type Field struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	Items        string `json:"items,omitempty"`
	Integer      bool   `json:"integer,omitempty"`
}

// Request is what is sent: the model, the text, and the fields.
type Request struct {
	Model  string           `json:"model"`
	State  string           `json:"state"`
	Fields map[string]Field `json:"fields"`
}

// Answer is one field's answer. Value is null when the model judged the
// field absent. Confidence is absent on models that do not report one.
type Answer struct {
	Type       string          `json:"type"`
	Value      json.RawMessage `json:"value"`
	Confidence *float64        `json:"confidence,omitempty"`
}

// Profile is what a model accepts. Models not listed accept text and lists of
// text, which every extraction model on the route answers.
type Profile struct {
	Number bool // typed numbers
}

var fieldName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// profiles lists models whose number answers are typed. Models answer text
// otherwise, which the caller parses.
var profiles = map[string]Profile{
	"cloudflare/clairic":                {Number: true},
	"inference-net/schematron-v2-small": {Number: true},
	"inference-net/schematron-v2-turbo": {Number: true},
	"liquid/d1.2":                       {Number: true},
	"liquid/d1.2-ultra":                 {Number: true},
}

// ProfileOf returns what model answers.
func ProfileOf(model string) Profile { return profiles[model] }

// Validate checks a request against the model's profile before it is stored.
func Validate(r Request) error {
	if r.Model == "" {
		return errors.New("extraction model not configured")
	}
	if strings.TrimSpace(r.State) == "" {
		return errors.New("extraction state is empty")
	}
	if len(r.Fields) == 0 {
		return errors.New("extraction has no fields")
	}
	p := ProfileOf(r.Model)
	for name, f := range r.Fields {
		if !fieldName.MatchString(name) {
			return fmt.Errorf("invalid field name %q", name)
		}
		switch f.Type {
		case Text:
		case Number:
			if !p.Number {
				return fmt.Errorf("%s does not answer number fields", r.Model)
			}
		case List:
			if f.Items != Text {
				return fmt.Errorf("field %s: only lists of text are supported", name)
			}
		default:
			return fmt.Errorf("field %s: unsupported type %q", name, f.Type)
		}
		if f.Items != "" && f.Type != List {
			return fmt.Errorf("field %s: items only applies to lists", name)
		}
		if f.Integer && f.Type != Number {
			return fmt.Errorf("field %s: integer only applies to numbers", name)
		}
	}
	return nil
}

// Answers decodes a provider's answers, keeping only fields that were asked
// for and whose value matches the type asked. Malformed entries are dropped,
// not fatal: the caller keeps whatever the page already said.
func Answers(r Request, raw string) map[string]Answer {
	out := map[string]Answer{}
	var all map[string]Answer
	if json.Unmarshal([]byte(raw), &all) != nil {
		return out
	}
	for name, a := range all {
		f, ok := r.Fields[name]
		if !ok || len(a.Value) == 0 || string(a.Value) == "null" {
			continue
		}
		if a.Confidence != nil && (*a.Confidence < 0 || *a.Confidence > 1) {
			continue
		}
		var ok2 bool
		switch f.Type {
		case Text:
			var s string
			ok2 = json.Unmarshal(a.Value, &s) == nil
		case Number:
			var n float64
			ok2 = json.Unmarshal(a.Value, &n) == nil
		case List:
			var l []string
			ok2 = json.Unmarshal(a.Value, &l) == nil
		}
		if ok2 {
			a.Type = f.Type
			out[name] = a
		}
	}
	return out
}
