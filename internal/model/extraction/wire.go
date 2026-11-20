// Package extraction defines typed field extraction (OpenRouter's
// /api/alpha/extractions): a state text plus named, typed fields, answered one
// value per field with a probability that the text holds nothing for it.
package extraction

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"unicode/utf8"
)

// RequestBudget bounds a stored request: a posting's text plus the field specs.
const RequestBudget = 262144

// Field types this package sends. The route has more (date, list, estimate…);
// add them here when a schema needs one.
const (
	Text   = "text"
	Number = "number"
)

type Field struct {
	Type          string   `json:"type"`
	Instructions  string   `json:"instructions,omitempty"`
	Unit          string   `json:"unit,omitempty"`
	Integer       bool     `json:"integer,omitempty"`
	NullThreshold *float64 `json:"null_threshold,omitempty"`
}

type Request struct {
	Model  string           `json:"model"`
	State  string           `json:"state"`
	Fields map[string]Field `json:"fields"`
}

// Answer is one field's result. Value is null when the model judged the state
// to hold nothing for the field.
type Answer struct {
	Type       string          `json:"type"`
	Value      json.RawMessage `json:"value"`
	PNull      *float64        `json:"p_null,omitempty"`
	Confidence *float64        `json:"confidence,omitempty"`
	Span       *[2]int         `json:"span,omitempty"`
}

var fieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func Validate(r Request) error {
	if r.Model == "" {
		return errors.New("missing model")
	}
	if r.State == "" || !utf8.ValidString(r.State) {
		return errors.New("missing state")
	}
	if len(r.Fields) == 0 {
		return errors.New("no fields")
	}
	for name, f := range r.Fields {
		if !fieldName.MatchString(name) {
			return errors.New("invalid field name")
		}
		if f.Type != Text && f.Type != Number {
			return errors.New("invalid field type")
		}
		if f.Integer && f.Type != Number {
			return errors.New("integer applies to numbers only")
		}
		if t := f.NullThreshold; t != nil && !(*t > 0 && *t <= 1) {
			return errors.New("invalid null threshold")
		}
	}
	if b, _ := json.Marshal(r); len(b) > RequestBudget {
		return errors.New("request too large")
	}
	return nil
}

// Parse reads a response's answers, keeping only well-formed answers to
// fields the request asked for. A field without a usable answer is absent.
func Parse(r Request, raw []byte) (map[string]Answer, error) {
	var all map[string]Answer
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, errors.New("invalid extraction answers")
	}
	out := map[string]Answer{}
	for name, a := range all {
		f, ok := r.Fields[name]
		if !ok || a.Type != f.Type || !unit(a.PNull) || !unit(a.Confidence) {
			continue
		}
		if len(a.Value) == 0 || string(a.Value) == "null" {
			continue
		}
		var ok2 bool
		switch f.Type {
		case Text:
			var s string
			ok2 = json.Unmarshal(a.Value, &s) == nil && s != ""
		case Number:
			var n float64
			ok2 = json.Unmarshal(a.Value, &n) == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
		}
		if ok2 {
			out[name] = a
		}
	}
	return out, nil
}

func unit(p *float64) bool { return p == nil || (*p >= 0 && *p <= 1) }

// Text returns a text answer's value.
func (a Answer) Text() string {
	var s string
	_ = json.Unmarshal(a.Value, &s)
	return s
}

// Number returns a number answer's value.
func (a Answer) Number() float64 {
	var n float64
	_ = json.Unmarshal(a.Value, &n)
	return n
}

// Certainty is the model's confidence in the value, or 1-p_null when the
// model reports only that.
func (a Answer) Certainty() float64 {
	switch {
	case a.Confidence != nil && a.PNull != nil:
		return *a.Confidence * (1 - *a.PNull)
	case a.Confidence != nil:
		return *a.Confidence
	case a.PNull != nil:
		return 1 - *a.PNull
	}
	return 0
}
