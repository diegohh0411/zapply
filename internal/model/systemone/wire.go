// Package systemone defines the typed System One protocol. Question keys are
// correlation identifiers; instructions must identify their subjects explicitly.
package systemone

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
)

const ResultBudget = 262144
const RequestBudget = 262144

type Question struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}
type Request struct {
	Model     string              `json:"model"`
	State     json.RawMessage     `json:"state"`
	Questions map[string]Question `json:"questions"`
}
type Answer struct {
	Type          string                     `json:"type"`
	Noul          *float64                   `json:"noul,omitempty"`
	Choice        string                     `json:"choice,omitempty"`
	Score         *float64                   `json:"score,omitempty"`
	Probabilities map[string]float64         `json:"probabilities,omitempty"`
	Confidence    *float64                   `json:"confidence,omitempty"`
	Legend        map[string]json.RawMessage `json:"legend,omitempty"`
}

// Canonical preserves JSON numbers, sorts object keys and rejects trailing data.
func Canonical(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	if n, ok := v.(json.Number); ok {
		value, e := canonicalNumber(string(n))
		if e != nil {
			return nil, e
		}
		v = json.Number(value)
	} else if e := normalize(v); e != nil {
		return nil, e
	}
	return json.Marshal(v)
}
func Hash(v any) (string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	b, e = Canonical(b)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func Raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func unit(f float64) bool       { return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 1 }
func ValidateQuestion(q Question) error {
	if !json.Valid(q.Instructions) || string(q.Instructions) == "null" {
		return errors.New("missing instructions")
	}
	switch q.Type {
	case "noul":
		return nil
	case "choice":
		var c map[string]json.RawMessage
		if json.Unmarshal(q.Criteria, &c) != nil || len(c) < 2 {
			return errors.New("invalid choice criteria")
		}
	case "score":
		var c []json.RawMessage
		if json.Unmarshal(q.Criteria, &c) != nil || len(c) < 2 || len(c) > 10 {
			return errors.New("invalid score criteria")
		}
	default:
		return errors.New("invalid question type")
	}
	return nil
}
func Validate(q Question, a Answer) error {
	bad := errors.New("invalid decision answer")
	if ValidateQuestion(q) != nil || q.Type != a.Type {
		return bad
	}
	if q.Type == "noul" {
		if a.Noul == nil || !unit(*a.Noul) {
			return bad
		}
		return nil
	}
	if a.Confidence == nil || !unit(*a.Confidence) {
		return bad
	}
	options := map[string]json.RawMessage{}
	if q.Type == "choice" {
		if json.Unmarshal(q.Criteria, &options) != nil {
			return bad
		}
	} else {
		var levels []json.RawMessage
		if json.Unmarshal(q.Criteria, &levels) != nil {
			return bad
		}
		for i, c := range levels {
			options[strconv.Itoa(i)] = c
		}
		if len(a.Legend) != len(options) {
			return bad
		}
		for k, v := range options {
			x, e := Canonical(v)
			y, f := Canonical(a.Legend[k])
			if e != nil || f != nil || !bytes.Equal(x, y) {
				return bad
			}
		}
	}
	if len(a.Probabilities) != len(options) {
		return bad
	}
	sum, score, best := 0., 0., 0.
	for k := range options {
		p, ok := a.Probabilities[k]
		if !ok || !unit(p) {
			return bad
		}
		sum += p
		best = math.Max(best, p)
		if q.Type == "score" {
			i, _ := strconv.Atoi(k)
			score += float64(i) * p
		}
	}
	if math.Abs(sum-1) > .01 {
		return bad
	}
	if q.Type == "score" {
		if a.Score == nil || math.IsNaN(*a.Score) || math.Abs(*a.Score-score) > 1e-5 {
			return bad
		}
	} else {
		if _, ok := options[a.Choice]; !ok || a.Probabilities[a.Choice] < best-1e-9 {
			return bad
		}
	}
	return nil
}

// Normalize numeric spelling as well as key order: jsonb may expand exponent
// notation or retain decimal zeroes when stored requests are loaded again.
func normalize(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			if n, ok := item.(json.Number); ok {
				value, e := canonicalNumber(string(n))
				if e != nil {
					return e
				}
				x[k] = json.Number(value)
			} else if e := normalize(item); e != nil {
				return e
			}
		}
	case []any:
		for i, item := range x {
			if n, ok := item.(json.Number); ok {
				value, e := canonicalNumber(string(n))
				if e != nil {
					return e
				}
				x[i] = json.Number(value)
			} else if e := normalize(item); e != nil {
				return e
			}
		}
	}
	return nil
}
func canonicalNumber(s string) (string, error) {
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign = "-"
		s = s[1:]
	}
	exponent := int64(0)
	if at := strings.IndexAny(s, "eE"); at >= 0 {
		var e error
		exponent, e = strconv.ParseInt(s[at+1:], 10, 32)
		if e != nil {
			return "", errors.New("JSON exponent out of range")
		}
		s = s[:at]
	}
	integer := len(s)
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		integer = dot
		s = strings.ReplaceAll(s, ".", "")
	}
	leading := len(s) - len(strings.TrimLeft(s, "0"))
	digits := strings.TrimRight(strings.TrimLeft(s, "0"), "0")
	if digits == "" {
		return "0", nil
	}
	exponent += int64(integer - leading - 1)
	coefficient := digits[:1]
	if len(digits) > 1 {
		coefficient += "." + digits[1:]
	}
	if exponent != 0 {
		coefficient += "e" + strconv.FormatInt(exponent, 10)
	}
	return sign + coefficient, nil
}
