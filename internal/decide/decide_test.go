package decide

import (
	"encoding/json"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"testing"
)

func TestCanonicalKeysAndPacking(t *testing.T) {
	q := systemone.Question{Type: "noul", Instructions: systemone.Raw("Does `record` qualify?")}
	a, e := New("first", []byte(`{"a":1,"b":2}`), q)
	if e != nil {
		t.Fatal(e)
	}
	b, e := New("second", []byte(`{ "b":2, "a":1 }`), q)
	if e != nil {
		t.Fatal(e)
	}
	if a.StateHash != b.StateHash || a.QuestionHash != b.QuestionHash {
		t.Fatal("canonical content misses")
	}
	q.Instructions = systemone.Raw("Does `record` meet the qualification?")
	c, _ := New("third", a.State, q)
	if c.QuestionHash == a.QuestionHash {
		t.Fatal("wording does not invalidate")
	}
	packed, e := Pack("test/model", []Pending{a, b, c}, 10000, 2)
	if e != nil || len(packed) != 2 || len(packed[0].Items) != 2 {
		t.Fatalf("packing %d %v", len(packed), e)
	}
	if _, e = Pack("test/model", []Pending{a}, 10, 0); e == nil {
		t.Fatal("oversized question accepted")
	}
}
func TestAnswerValidation(t *testing.T) {
	q := systemone.Question{Type: "score", Instructions: systemone.Raw("Rate `record`."), Criteria: systemone.Raw([]string{"No evidence", "Direct evidence"})}
	score, conf := .75, .4
	a := systemone.Answer{Type: "score", Score: &score, Confidence: &conf, Probabilities: map[string]float64{"0": .25, "1": .75}, Legend: map[string]json.RawMessage{"0": systemone.Raw("No evidence"), "1": systemone.Raw("Direct evidence")}}
	if e := systemone.Validate(q, a); e != nil {
		t.Fatal(e)
	}
	score = .7
	if systemone.Validate(q, a) == nil {
		t.Fatal("incorrect API score accepted")
	}
	score = .75
	a.Legend["1"] = systemone.Raw("Different criterion")
	if systemone.Validate(q, a) == nil {
		t.Fatal("wrong legend accepted")
	}
	choice := systemone.Question{Type: "choice", Instructions: systemone.Raw("Classify `record`."), Criteria: systemone.Raw(map[string]string{"yes": "It qualifies", "no": "It does not qualify"})}
	a = systemone.Answer{Type: "choice", Choice: "no", Confidence: &conf, Probabilities: map[string]float64{"yes": .75, "no": .25}}
	if systemone.Validate(choice, a) == nil {
		t.Fatal("nonmaximal choice accepted")
	}
}
