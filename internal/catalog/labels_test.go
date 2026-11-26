package catalog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/model/systemone"
)

func TestKeys(t *testing.T) {
	s := Sighting{ATSKey: "lever:acme:123", Company: "Acme, Inc.", Title: "Señor Engineer", Location: "Mexico City, Mexico", URL: "https://jobs.lever.co/acme/123?utm_source=x"}
	keys := s.Keys()
	if len(keys) != 3 || keys[0] != "ats:lever:acme:123" || !strings.HasPrefix(keys[1], "url:") || keys[2] != "fp:acme|senor engineer|mx" {
		t.Fatalf("keys = %v", keys)
	}
	// The source's country wins; without one, an unknown place stays apart.
	s.Country, s.Location = "es", "Somewhere"
	if k := s.Keys()[2]; k != "fp:acme|senor engineer|es" {
		t.Errorf("country fp = %s", k)
	}
	s.Country = ""
	if k := s.Keys()[2]; k != "fp:acme|senor engineer|somewhere" {
		t.Errorf("unknown country fp = %s", k)
	}
}

func TestCountry(t *testing.T) {
	for in, want := range map[string]string{
		"Mexico City, Mexico": "MX", "Austin, TX": "US", "Toronto, ON, CA": "CA", "Remote - Spain": "ES", "Remote - US": "US",
		"Berlin, Germany": "DE", "São Paulo, Brasil": "BR", "Remote": "", "New York, New York": "US", "Bogotá, Colombia": "CO", "London, UK": "GB",
	} {
		if got := Country(in); got != want {
			t.Errorf("Country(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"mx": "MX", "México": "MX", "USA": "US", "Narnia": ""} {
		if got := CountryCode(in); got != want {
			t.Errorf("CountryCode(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"en": "en", "Español": "es", "pt-BR": "pt", "English": "en", "Klingon": ""} {
		if got := LanguageCode(in); got != want {
			t.Errorf("LanguageCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuestionsAreValidRequests(t *testing.T) {
	qs := decisionQuestions()
	if len(qs) != 5 {
		t.Fatalf("%d questions", len(qs))
	}
	for i, q := range qs {
		if err := systemone.ValidateQuestion(q); err != nil {
			t.Errorf("q%d: %v", i+1, err)
		}
	}
	var areas map[string]string
	if err := json.Unmarshal(qs[qArea].Criteria, &areas); err != nil || len(areas) != len(Areas) {
		t.Fatalf("areas criteria: %v %d", err, len(areas))
	}
	for _, a := range Areas {
		if areas[a] == "" {
			t.Errorf("area %s has no criterion", a)
		}
	}
	// The boundary cases and Spanish titles are in the prompt.
	all := string(qs[qSeniority].Criteria) + string(qs[qArea].Criteria)
	for _, want := range []string{"Practicante", "Becario", "Recién egresado", "Gerente", "Jr", "Sr", "Solutions Engineer", "Mercor", "Alignerr"} {
		if !strings.Contains(all, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	// The extraction asks only for types every extraction model answers.
	if err := extraction.Validate(extractionRequest("some/text-only-model", postingRow{Title: "Engineer"})); err != nil {
		t.Errorf("extraction request: %v", err)
	}
}

// fakeAnswers is what a well-behaved decision model answers for the posting.
func fakeAnswers(t *testing.T, picks map[int]map[string]float64, remote float64) map[string]systemone.Answer {
	t.Helper()
	out := map[string]systemone.Answer{}
	for i, q := range decisionQuestions() {
		key := "q" + string(rune('1'+i))
		if q.Type == "noul" {
			out[key] = systemone.Answer{Type: "noul", Noul: &remote}
			continue
		}
		var criteria map[string]json.RawMessage
		_ = json.Unmarshal(q.Criteria, &criteria)
		probs := map[string]float64{}
		rest := 1.0
		for k, p := range picks[i] {
			probs[k] = p
			rest -= p
		}
		others := len(criteria) - len(picks[i])
		best, bp := "", -1.0
		for k := range criteria {
			if _, ok := probs[k]; !ok {
				probs[k] = rest / float64(others)
			}
			if probs[k] > bp {
				best, bp = k, probs[k]
			}
		}
		c := bp - 0.1
		a := systemone.Answer{Type: "choice", Choice: best, Probabilities: probs, Confidence: &c}
		if err := systemone.Validate(q, a); err != nil {
			t.Fatalf("fake answer %s invalid: %v", key, err)
		}
		out[key] = a
	}
	return out
}

func TestDecisionLabels(t *testing.T) {
	// A Solutions Engineer: software first, sales second.
	answers := fakeAnswers(t, map[int]map[string]float64{
		qSeniority: {"senior": 0.8}, qArea: {"engineering_software": 0.62, "sales": 0.31}, qEmployment: {"full_time": 0.9}, qMinYears: {"5": 0.7},
	}, 0.12)
	l, ok := decisionLabels(answers)
	if !ok || *l.Seniority != "senior" || !reflect.DeepEqual(l.Areas, []string{"engineering_software", "sales"}) || *l.EmploymentType != "full_time" ||
		*l.Remote || *l.MinYears != 5 {
		t.Fatalf("labels = %+v, %v", l, ok)
	}
	if l.Confidence["remote"] != 0.88 || l.Confidence["seniority"] != 0.7 {
		t.Errorf("confidence = %v", l.Confidence)
	}
	// A clear single area has no second one; "unstated" years are unknown.
	answers = fakeAnswers(t, map[int]map[string]float64{
		qSeniority: {"intern": 0.9}, qArea: {"ai_training": 0.9}, qEmployment: {"contract": 0.9}, qMinYears: {"unstated": 0.9},
	}, 0.97)
	l, ok = decisionLabels(answers)
	if !ok || !reflect.DeepEqual(l.Areas, []string{"ai_training"}) || l.MinYears != nil || !*l.Remote {
		t.Fatalf("labels = %+v, %v", l, ok)
	}
	// Answers missing seniority or area are not labels.
	delete(answers, "q1")
	if _, ok := decisionLabels(answers); ok {
		t.Error("labels without seniority")
	}
}

func TestExtractionLabels(t *testing.T) {
	r := extractionRequest("cloudflare/clairic", postingRow{Title: "Backend Engineer"})
	raw := `{"specialty":{"type":"text","value":"  backend   payments ","confidence":0.8},
 "countries":{"type":"list","value":["mx","México","US","Narnia"],"confidence":0.9},
 "languages":{"type":"list","value":["Spanish","en","en-US"],"confidence":0.3},
 "unasked":{"type":"text","value":"x"}}`
	l := extractionLabels(r, raw)
	if l.Specialty == nil || *l.Specialty != "backend payments" || !reflect.DeepEqual(l.Countries, []string{"MX", "US"}) || l.Languages != nil {
		t.Fatalf("labels = %+v", l)
	}
	if l.Confidence["countries"] != 0.9 {
		t.Errorf("confidence = %v", l.Confidence)
	}
	if l := extractionLabels(r, "not json"); l.Specialty != nil || l.Countries != nil {
		t.Errorf("garbage = %+v", l)
	}
}
