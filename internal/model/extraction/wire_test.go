package extraction

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := Request{Model: "cloudflare/clairic", State: "text", Fields: map[string]Field{"title": {Type: Text}}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	bad := map[string]Request{
		"no model":      {State: "x", Fields: ok.Fields},
		"no state":      {Model: "m", Fields: ok.Fields},
		"no fields":     {Model: "m", State: "x"},
		"bad name":      {Model: "m", State: "x", Fields: map[string]Field{"a b": {Type: Text}}},
		"bad type":      {Model: "m", State: "x", Fields: map[string]Field{"a": {Type: "enum"}}},
		"text integer":  {Model: "m", State: "x", Fields: map[string]Field{"a": {Type: Text, Integer: true}}},
		"zero null":     {Model: "m", State: "x", Fields: map[string]Field{"a": {Type: Text, NullThreshold: &zero}}},
		"over budget":   {Model: "m", State: strings.Repeat("x", RequestBudget), Fields: ok.Fields},
		"invalid utf-8": {Model: "m", State: "\xff", Fields: ok.Fields},
	}
	for name, r := range bad {
		if Validate(r) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseKeepsOnlyUsableAnswers(t *testing.T) {
	r := Request{Fields: map[string]Field{"title": {Type: Text}, "pay": {Type: Number}, "city": {Type: Text}, "team": {Type: Text}}}
	got, err := Parse(r, []byte(`{
	 "title":{"type":"text","value":"Designer","p_null":0.1,"confidence":0.9},
	 "pay":{"type":"number","value":120000,"p_null":0.02},
	 "city":{"type":"text","value":null,"guess":"Paris","p_null":0.8},
	 "team":{"type":"number","value":3},
	 "extra":{"type":"text","value":"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["title"].Text() != "Designer" || got["pay"].Number() != 120000 {
		t.Fatalf("answers = %+v", got)
	}
	if c := got["title"].Certainty(); c < 0.809 || c > 0.811 {
		t.Errorf("certainty = %v, want confidence × (1 - p_null)", c)
	}
	if c := got["pay"].Certainty(); c != 0.98 {
		t.Errorf("certainty = %v, want 1 - p_null", c)
	}
	if _, err := Parse(r, []byte(`[]`)); err == nil {
		t.Error("non-object answers accepted")
	}
}
