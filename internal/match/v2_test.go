package match

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/model/systemone"
)

func v2Snapshot() Snapshot {
	done := "completed"
	return Snapshot{Title: "Engineer", Company: "Acme", Description: "x",
		Lines: []Line{{Text: "5+ years building Go services"}, {Text: "Bachelor degree in computer science"}, {Text: "Fluent Spanish"}, {Text: "Curious and kind"}, {Text: "We offer snacks"}},
		Evidence: []Evidence{
			{ID: "job", Kind: "job", Title: "Go engineer", AutoInclude: true, IsCurrent: true, StartDate: ptr("2023-01-01"), Highlights: []Highlight{{ID: "h1", Text: "Built Go APIs", AutoInclude: true}, {ID: "h2", Text: "Ran on-call", AutoInclude: true}}},
			{ID: "degree", Kind: "course", Title: "BSc Computer Science", AutoInclude: true, Details: map[string]any{"degree_type": "bachelor", "status": done}},
			{ID: "hidden", Kind: "project", Title: "Side project", AutoInclude: false, Highlights: []Highlight{{ID: "h3", Text: "Shipped a CLI"}}},
			{ID: "break", Kind: "career_break", Title: "Break"},
		},
		Skills: []Skill{{ID: "go", Name: "Go", Type: "hard"}, {ID: "es", Name: "Spanish", Type: "language", Proficiency: "B1"}},
	}
}

func choiceAnswer(pick string, options ...string) systemone.Answer {
	c := .9
	a := systemone.Answer{Type: "choice", Choice: pick, Confidence: &c, Probabilities: map[string]float64{}}
	for _, o := range options {
		a.Probabilities[o] = 0
	}
	a.Probabilities[pick] = 1
	return a
}
func scoreAnswer(level, levels int) systemone.Answer {
	c := .9
	a := systemone.Answer{Type: "score", Confidence: &c, Probabilities: map[string]float64{}}
	for i := range levels {
		a.Probabilities[fmt.Sprint(i)] = 0
	}
	a.Probabilities[fmt.Sprint(level)] = 1
	return a
}

func TestV2QuestionCountsAreLinear(t *testing.T) {
	s := v2Snapshot()
	read, e := ReadQuestionsV2(s)
	if e != nil {
		t.Fatal(e)
	}
	// 5 lines, seniority, degree, 1 language, 2 skills, 3 entries, 3 highlights.
	if want := 5 + 1 + 1 + 1 + 2 + 3 + 3; len(read) != want {
		t.Fatalf("read %d want %d", len(read), want)
	}
	reqs := []Requirement{{Text: "a", TextHash: "a", Line: 0}, {Text: "b", TextHash: "b", Line: 1}}
	cover, e := CoverQuestionsV2(s, reqs)
	if e != nil || len(cover) != 4*len(reqs) {
		t.Fatalf("cover %d %v", len(cover), e)
	}
	for _, q := range append(read, cover...) {
		if strings.Contains(string(q.State), `"break"`) || strings.Contains(string(q.State), "auto_include") || strings.Contains(string(q.State), `"job"`) && strings.Contains(string(q.State), `"id"`) {
			t.Fatalf("state leaks ids, selection flags or career breaks: %s", q.State)
		}
	}
	// Posting-only questions survive library edits; library questions do not.
	edited := v2Snapshot()
	edited.Evidence[0].Title = "Senior Go engineer"
	again, _ := ReadQuestionsV2(edited)
	same := map[string]string{}
	for _, q := range read {
		same[q.Subject] = q.StateHash
	}
	for _, q := range again {
		library := strings.HasPrefix(q.Subject, "relevance:") || strings.HasPrefix(q.Subject, "highlight:")
		if library == (same[q.Subject] == q.StateHash) {
			t.Fatalf("%s: library question %v, unchanged state %v", q.Subject, library, same[q.Subject] == q.StateHash)
		}
	}
}

func TestV2ComputeAttributesChecksAndWeights(t *testing.T) {
	s := v2Snapshot()
	a := map[string]systemone.Answer{"seniority": choiceAnswer("mid", "mid", "not_specified"), "degree": choiceAnswer("bachelor", "bachelor", "not_specified"), "language:es": choiceAnswer("C1", "C1", "not_specified")}
	classes := []string{"must", "must", "must", "trait", "other"}
	for i, c := range classes {
		a[fmt.Sprintf("line:%d:class", i)] = choiceAnswer(c, "must", "preferred", "duty", "trait", "other")
	}
	for _, id := range []string{"go", "es"} {
		n := .5
		a["skill:"+id] = systemone.Answer{Type: "noul", Noul: &n}
	}
	for _, id := range []string{"job", "degree", "hidden"} {
		a["relevance:"+id] = scoreAnswer(1, 5)
	}
	a["relevance:job"] = scoreAnswer(4, 5)
	for _, id := range []string{"h1", "h2", "h3"} {
		a["highlight:"+id] = scoreAnswer(2, 5)
	}
	reqs, incomplete := RequirementsV2(s, a)
	if incomplete || len(reqs) != 4 {
		t.Fatalf("requirements %d incomplete %v", len(reqs), incomplete)
	}
	entries := []string{"e0", "e1", "e2", "none"}
	hl := []string{"h0", "h1", "h2", "none"}
	set := func(r Requirement, strength int, entry, cite, check string) {
		a["strength:"+r.TextHash] = scoreAnswer(strength, 4)
		a["cover:"+r.TextHash] = choiceAnswer(entry, entries...)
		a["cite:"+r.TextHash] = choiceAnswer(cite, hl...)
		a["check:"+r.TextHash] = choiceAnswer(check, "degree", "language", "other")
	}
	set(reqs[0], 3, "e0", "h0", "other")      // 5+ years: only ~3.8 years of Go work
	set(reqs[1], 3, "none", "none", "degree") // bachelor: completed degree satisfies
	set(reqs[2], 3, "none", "none", "language")
	set(reqs[3], 2, "e2", "h2", "other") // trait from a hidden entry
	asOf := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	r := ComputeV2(s, reqs, a, nil, asOf, false)
	if r.Partial || r.Score == nil || r.ScoringVersion != ScoringVersionV2 {
		t.Fatalf("partial result %#v", r)
	}
	byText := map[string]Coverage{}
	for _, c := range r.Requirements {
		byText[c.Text] = c
	}
	years := byText["5+ years building Go services"]
	if years.Status != "partial" || years.Strength != .6 || years.EvidenceID != "job" || len(years.Citations) != 1 || years.Citations[0].ID != "h1" {
		t.Fatalf("years check: %#v", years)
	}
	degree := byText["Bachelor degree in computer science"]
	if degree.Status != "met" || degree.EvidenceID != "degree" || degree.Degree != "bachelor" || !strings.Contains(degree.Check, "bachelor completed") {
		t.Fatalf("degree check: %#v", degree)
	}
	language := byText["Fluent Spanish"]
	if language.Status != "partial" || language.Language != "C1" || !strings.Contains(language.Check, "Spanish B1") {
		t.Fatalf("language check: %#v", language)
	}
	trait := byText["Curious and kind"]
	if trait.Class != "trait" || trait.Status != "partial" || trait.EvidenceID != "hidden" {
		t.Fatalf("trait: %#v", trait)
	}
	if r.MustTotal != 3 || r.MustMet != 3 {
		t.Fatalf("must %d of %d", r.MustMet, r.MustTotal)
	}
	// Weighted: (2×.6 + 2×1 + 2×.6 + .5×.6) / 6.5.
	if want := 100 * (2*.6 + 2*1 + 2*.6 + .5*.6) / 6.5; *r.Score < want-1e-9 || *r.Score > want+1e-9 {
		t.Fatalf("score %v want %v", *r.Score, want)
	}
	if len(r.Selection) != 2 || r.Selection[0].EvidenceID != "job" || len(r.Suggestions) != 1 || r.Suggestions[0].EvidenceID != "hidden" {
		t.Fatalf("selection %#v suggestions %#v", r.Selection, r.Suggestions)
	}
	// A missing round-two answer leaves the result partial without a score.
	delete(a, "cite:"+reqs[3].TextHash)
	if r = ComputeV2(s, reqs, a, nil, asOf, false); !r.Partial || r.Score != nil {
		t.Fatal("missing citation answer not partial")
	}
	// An ignored line leaves coverage; a trait override reweights.
	a["cite:"+reqs[3].TextHash] = choiceAnswer("h2", hl...)
	r = ComputeV2(s, reqs, a, map[string]string{reqs[0].TextHash: "ignore", reqs[1].TextHash: "trait"}, asOf, false)
	if r.MustTotal != 1 || r.Requirements[0].Status != "ignored" {
		t.Fatalf("overrides %#v", r)
	}
}
