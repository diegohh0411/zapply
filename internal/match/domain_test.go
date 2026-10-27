package match

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/model/systemone"
)

func TestSourceLinesAndYears(t *testing.T) {
	text := "Qualifications\n• CPA\n• Go\n1. At least three years of experience\n2. 3–5 years in practice\n" + strings.Repeat("A long sentence. ", 60)
	lines, truncated := SplitLines(text)
	if truncated || len(lines) < 6 {
		t.Fatalf("lines: %#v", lines)
	}
	for _, l := range lines {
		if text[l.Start:l.End] != l.Text {
			t.Fatal("lost source span")
		}
		if len([]rune(l.Text)) > 400 {
			t.Fatal("oversized line")
		}
	}
	if lines[1].Text != "CPA" || lines[2].Text != "Go" {
		t.Fatal("discarded short qualification")
	}
	for text, want := range map[string]float64{"5+ years": 5, "at least three years": 3, "3–5 years": 3, "3.5 years": 3.5, "No years required": 0} {
		if got := YearsIn(text); got != want {
			t.Errorf("%s: %v", text, got)
		}
	}
	lines, truncated = SplitLines(strings.Repeat("Go\n", 121))
	if len(lines) != 120 || !truncated {
		t.Fatal("line cap")
	}
}
func TestQuestionStatesAndSpeculation(t *testing.T) {
	s := Snapshot{Title: "Engineer", Company: "Acme", Description: "Go", Lines: []Line{{Text: "Go"}}, Evidence: []Evidence{{ID: "e", Title: "Engineer", Highlights: []Highlight{{ID: "h", Text: "Built Go services"}}}}}
	read, e := ReadQuestions(s)
	if e != nil || len(read) != 5 {
		t.Fatalf("read %d %v", len(read), e)
	}
	req := []Requirement{{Text: "Go", TextHash: "r"}}
	score, e := ScoreQuestions(s, req)
	if e != nil || len(score) != 2 {
		t.Fatalf("score %d %v", len(score), e)
	}
	s.Description = "Different posting, same requirement"
	next, _ := ScoreQuestions(s, req)
	if score[0].StateHash != next[0].StateHash {
		t.Fatal("description invalidates stable requirement")
	}
	s.Company = "Other"
	next, _ = ScoreQuestions(s, req)
	if score[0].StateHash == next[0].StateHash {
		t.Fatal("company does not invalidate")
	}
	for _, q := range append(read, score...) {
		if e := systemone.ValidateQuestion(q.Question); e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(string(q.Question.Instructions), "`") {
			t.Fatal("missing explicit data path")
		}
	}
}
func ptr[T any](v T) *T { return &v }
func direct() systemone.Answer {
	return systemone.Answer{Type: "score", Score: ptr(3.), Confidence: ptr(.9), Probabilities: map[string]float64{"0": 0, "1": 0, "2": 0, "3": 1}}
}
func TestComputeUnknownFloorOverridesAndStableOrder(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := Snapshot{Evidence: []Evidence{{ID: "first", AutoInclude: true, Highlights: []Highlight{}}, {ID: "hidden", AutoInclude: false, Highlights: []Highlight{}}}}
	req := []Requirement{{Text: "Go", TextHash: "go", Class: "must"}}
	a := map[string]systemone.Answer{"seniority": {Choice: "mid"}, Pair("first", "go"): direct(), Pair("hidden", "go"): direct()}
	r := Compute(s, req, a, nil, now, false)
	if r.Score == nil || *r.Score != 100 || r.MustMet != 1 || len(r.Selection) != 1 || len(r.Suggestions) != 1 || r.Evidence[0].EvidenceID != "first" {
		t.Fatalf("result %#v", r)
	}
	delete(a, Pair("first", "go"))
	r = Compute(s, req, a, nil, now, false)
	if !r.Partial || r.Score != nil || !r.Requirements[0].Provisional {
		t.Fatal("missing pair counted as complete")
	}
	delete(a, Pair("hidden", "go"))
	r = Compute(s, req, a, nil, now, false)
	if r.Requirements[0].Status != "unknown" {
		t.Fatal("unknown mislabeled gap")
	}
	for _, i := range r.Insights {
		if i.Type == "gap" {
			t.Fatal("invented gap")
		}
	}
	r = Compute(s, req, a, map[string]string{"go": "ignore"}, now, false)
	if r.Partial || r.Score != nil || r.MustTotal != 0 || r.Requirements[0].Status != "ignored" {
		t.Fatal("ignored requirement still applies")
	}
}
func TestYearsAndChecks(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := []Evidence{{ID: "one", StartDate: ptr("2020-01-01"), EndDate: ptr("2023-01-01"), AutoInclude: true}, {ID: "two", StartDate: ptr("2022-01-01"), IsCurrent: true, AutoInclude: true}}
	if got := Years(e, now); got < 5.99 || got > 6.01 {
		t.Fatalf("overlap: %v", got)
	}
	a := map[string]systemone.Answer{"seniority": {Choice: "senior"}, Pair("one", "r"): direct(), Pair("two", "r"): direct()}
	r := Compute(Snapshot{Evidence: e}, []Requirement{{TextHash: "r", Class: "must", MinYears: 10}}, a, nil, now, false)
	if r.Score == nil || *r.Score > 60 || r.Requirements[0].Status != "partial" {
		t.Fatalf("years not capped: %#v", r)
	}
	if Recency(Evidence{EndDate: ptr("2000-01-01")}, now) != .5 {
		t.Fatal("recency floor")
	}
}
func TestExpectedStrength(t *testing.T) {
	a := systemone.Answer{Probabilities: map[string]float64{"0": .05, "1": .1, "2": .25, "3": .6}}
	if got := Strength(a); got < .774999 || got > .775001 {
		t.Fatalf("strength %v", got)
	}
}
func TestSpeculativeFailuresOutsideRequirementCapAreIgnored(t *testing.T) {
	s := Snapshot{Description: "posting"}
	a := map[string]systemone.Answer{}
	for i := 0; i < 41; i++ {
		s.Lines = append(s.Lines, Line{Text: fmt.Sprintf("Requirement %d", i)})
		a[fmt.Sprintf("line:%d:class", i)] = systemone.Answer{Choice: "must"}
		if i < 40 {
			a[fmt.Sprintf("line:%d:type", i)] = systemone.Answer{Choice: "skill"}
		}
	}
	r, incomplete := Requirements(s, a)
	if len(r) != 40 || incomplete {
		t.Fatal("ignored questions made result incomplete")
	}
}
func TestIgnoredReadFailureAndRoundedDistribution(t *testing.T) {
	s := Snapshot{Description: "posting", Lines: []Line{{Text: "Go"}}}
	a := map[string]systemone.Answer{"line:0:class": {Choice: "must"}}
	r, incomplete := Requirements(s, a)
	if !incomplete || len(r) != 1 {
		t.Fatal("missing type accepted")
	}
	_, incomplete = Requirements(s, a, map[string]string{r[0].TextHash: "ignore"})
	if incomplete {
		t.Fatal("ignored type failure still applicable")
	}
	if Strength(systemone.Answer{Probabilities: map[string]float64{"3": 1, "2": .005}}) > 1 {
		t.Fatal("rounding exceeded score range")
	}
}

func TestCacheStatesContainOnlyRelevantContent(t *testing.T) {
	s := Snapshot{Title: "Engineer", Description: "Go", Lines: []Line{{Text: "Go"}}, Skills: []Skill{{ID: "go", Name: "Go"}}, Evidence: []Evidence{{ID: "e", Title: "Service", AutoInclude: true, ContentHash: "before", Highlights: []Highlight{{ID: "h", Text: "Improved throughput", AutoInclude: true, MetricValue: ptr(20.), MetricUnit: "percent"}}}}}
	before, _ := ReadQuestions(s)
	scores, _ := ScoreQuestions(s, []Requirement{{Text: "Go", TextHash: "r"}})
	s.Skills = append([]Skill{{ID: "new", Name: "Python"}}, s.Skills...)
	after, _ := ReadQuestions(s)
	keys := map[string]string{}
	for _, p := range after {
		keys[p.Subject] = p.StateHash + p.QuestionHash
	}
	for _, p := range before {
		if keys[p.Subject] != p.StateHash+p.QuestionHash {
			t.Fatalf("unrelated skill invalidated %s", p.Subject)
		}
	}
	s.Evidence[0].AutoInclude = false
	s.Evidence[0].Highlights[0].AutoInclude = false
	s.Evidence[0].ContentHash = "after"
	next, _ := ScoreQuestions(s, []Requirement{{Text: "Go", TextHash: "r"}})
	for i, p := range scores {
		if p.StateHash != next[i].StateHash {
			t.Fatal("selection flags invalidated scores")
		}
	}
	if strings.Contains(string(next[0].State), "auto_include") || strings.Contains(string(next[0].State), "content_hash") {
		t.Fatal("bookkeeping sent to model")
	}
	if !strings.Contains(string(next[0].State), "metric_value") || !strings.Contains(string(next[0].State), "percent") {
		t.Fatal("metric missing from model state")
	}
	s.Evidence[0].Highlights[0].MetricValue = ptr(30.)
	changed, _ := ScoreQuestions(s, []Requirement{{Text: "Go", TextHash: "r"}})
	if changed[0].StateHash == next[0].StateHash {
		t.Fatal("metric edit did not invalidate")
	}
}
func TestLanguageCheckUsesUnlinkedLibrarySkills(t *testing.T) {
	s := Snapshot{Evidence: []Evidence{{ID: "e", AutoInclude: true}}, Skills: []Skill{{ID: "english", Name: "English", Type: "language", Proficiency: "C2"}}}
	req := []Requirement{{Text: "English at C1", TextHash: "english", Class: "must", Type: "language", Language: "C1"}}
	yes := 1.
	a := map[string]systemone.Answer{"seniority": {Choice: "mid"}, "skill:english": {Noul: &yes}, Pair("e", "english"): direct()}
	r := Compute(s, req, a, nil, time.Now(), false)
	if r.Score == nil || *r.Score != 100 || r.Requirements[0].Status != "met" || !strings.Contains(r.Requirements[0].Check, "English C2") {
		t.Fatalf("unlinked language ignored: %#v", r)
	}
	s.Skills[0].Proficiency = "B2"
	r = Compute(s, req, a, nil, time.Now(), false)
	if r.Requirements[0].Strength != .6 {
		t.Fatal("insufficient proficiency not capped")
	}
	s.Skills[0].Name = "French"
	r = Compute(s, req, a, nil, time.Now(), false)
	if r.Requirements[0].Strength != .6 {
		t.Fatal("different language accepted")
	}
}
