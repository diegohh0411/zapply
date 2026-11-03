package render

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/match"
)

func ptr(s string) *string { return &s }

var today = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

func sample() Library {
	return Library{
		Profile: Profile{Name: "Ada"},
		Evidence: []Evidence{
			{ID: "job", Kind: "job", Category: "paid", Form: "engagement", Title: "Engineer", AutoInclude: true, IsCurrent: true,
				StartDate: ptr("2021-03-01"), StartPrecision: ptr("month"),
				Highlights: []Highlight{{"h1", "first", true}, {"h2", "second", true}, {"h3", "hidden", false}}},
			{ID: "proj", ParentID: ptr("job"), Kind: "project", Category: "paid", Form: "output", Title: "Project", AutoInclude: true,
				StartDate: ptr("2023-05-01"), StartPrecision: ptr("month")},
			{ID: "old", Kind: "job", Category: "paid", Form: "engagement", Title: "Old job", AutoInclude: true,
				StartDate: ptr("2010-01-01"), StartPrecision: ptr("year"), EndDate: ptr("2012-01-01"), EndPrecision: ptr("year"),
				Highlights: []Highlight{{"h4", "old one", true}, {"h5", "old two", true}}},
			{ID: "deg", Kind: "degree_program", Category: "education", Form: "engagement", Title: "BSc", AutoInclude: true,
				StartDate: ptr("2005-01-01"), StartPrecision: ptr("year"), EndDate: ptr("2009-01-01"), EndPrecision: ptr("year")},
			{ID: "brk", Kind: "career_break", Category: "unpaid", Form: "engagement", Title: "Caregiving", AutoInclude: false},
			{ID: "cert", Kind: "certification", Category: "education", Form: "credential", Title: "Cert", AutoInclude: true,
				StartDate: ptr("2024-04-02"), StartPrecision: ptr("day")},
		},
		Skills: []Skill{{"s1", "Go", "hard", ""}, {"s2", "SQL", "hard", ""}, {"s3", "Spanish", "language", "native"}},
	}
}

func ids(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func section(c Content, key string) *Section {
	for i := range c.Sections {
		if c.Sections[i].Key == key {
			return &c.Sections[i]
		}
	}
	return nil
}

func TestBuildWithoutRun(t *testing.T) {
	b := Build(sample(), nil, Picks{}, "en", today)
	c := b.Content
	if c.Date != "2026-10-28" || c.Schema != 1 || c.Locale != "en" {
		t.Fatalf("header: %+v", c)
	}
	var keys []string
	for _, s := range c.Sections {
		keys = append(keys, s.Key)
	}
	if !slices.Equal(keys, []string{"experience", "education", "credentials"}) {
		t.Fatalf("sections %v", keys)
	}
	exp := section(c, "experience")
	if got := ids(exp.Entries); !slices.Equal(got, []string{"job", "old"}) {
		t.Fatalf("experience %v, want current first", got)
	}
	job := exp.Entries[0]
	if len(job.Children) != 1 || job.Children[0].ID != "proj" {
		t.Fatalf("project should nest under its job: %+v", job.Children)
	}
	if len(job.Highlights) != 2 || job.Highlights[1].ID != "h2" {
		t.Fatalf("only auto-included highlights, in library order: %+v", job.Highlights)
	}
	if *job.Start != (Date{"2021-03", "month"}) || section(c, "credentials").Entries[0].Start.Date != "2024-04-02" {
		t.Fatalf("dates keep their precision: %+v", job.Start)
	}
	for _, e := range b.Available.Entries {
		if e.ID == "brk" && e.Reason == "not_selected" {
			goto found
		}
	}
	t.Fatalf("career break should be available, not shown: %+v", b.Available)
found:
	if len(b.Available.Highlights) != 1 || b.Available.Highlights[0].ID != "h3" {
		t.Fatalf("available highlights %+v", b.Available.Highlights)
	}
	// Older highlights trim first; then entries, oldest first. No entry's
	// first highlight is ever listed.
	if want := []string{"h5", "h2", "deg", "old", "proj", "cert", "job"}; !slices.Equal(b.Trim, want) {
		t.Fatalf("trim %v, want %v", b.Trim, want)
	}
}

func TestBuildWithRunAndPicks(t *testing.T) {
	ranking := &match.Result{
		Evidence: []match.Selection{
			{EvidenceID: "old", Relevance: 2, HighlightIDs: []string{"h5", "h4"}},
			{EvidenceID: "job", Relevance: 0.5, HighlightIDs: []string{"h2", "h1"}},
			{EvidenceID: "cert", Relevance: 0.1},
			{EvidenceID: "proj", Relevance: 0.1},
			{EvidenceID: "deg", Relevance: 0.01},
		},
		Selection:    []match.Selection{{EvidenceID: "old"}, {EvidenceID: "job"}},
		Suggestions:  []match.Selection{{EvidenceID: "cert"}},
		Requirements: []match.Coverage{{Requirement: match.Requirement{Class: "must"}, Status: "met", EvidenceID: "job"}},
		Skills:       []match.RankedSkill{{SkillID: "s2", Relevance: 3}, {SkillID: "s1", Relevance: 0}},
	}
	picks := Picks{Include: []string{"brk", "h3"}, Exclude: []string{"h2"}}
	b := Build(sample(), ranking, picks, "es", today)
	c := b.Content
	exp := section(c, "experience")
	if got := ids(exp.Entries); !slices.Equal(got, []string{"job", "old"}) {
		t.Fatalf("experience %v", got)
	}
	if exp.Entries[0].Children != nil {
		t.Fatal("the project wasn't selected by the run")
	}
	if hs := exp.Entries[1].Highlights; hs[0].ID != "h5" {
		t.Fatalf("highlights follow the run's order: %+v", hs)
	}
	if hs := exp.Entries[0].Highlights; len(hs) != 2 || hs[0].ID != "h1" || hs[1].ID != "h3" {
		t.Fatalf("excluded h2 gone, picked h3 added: %+v", hs)
	}
	if section(c, "education") == nil || section(c, "unpaid") == nil || section(c, "credentials") != nil {
		t.Fatalf("degree kept, picked break shown, unselected cert left out: %+v", c.Sections)
	}
	if len(c.Skills) != 2 || c.Skills[0].ID != "s2" || c.Skills[1].ID != "s3" {
		t.Fatalf("relevant skills then languages: %+v", c.Skills)
	}
	for _, e := range b.Available.Entries {
		if e.ID == "cert" && e.Reason != "suggested" {
			t.Fatalf("cert reason %s", e.Reason)
		}
	}
	// Pinned h3 and brk never trim; the job supports a met must-have, so it
	// goes last.
	if want := []string{"h4", "deg", "old"}; !slices.Equal(b.Trim, want) {
		t.Fatalf("trim %v, want %v", b.Trim, want)
	}
}

func TestApply(t *testing.T) {
	b := Build(sample(), nil, Picks{}, "en", today)
	c := Apply(b.Content, []string{"h2", "proj", "deg", "s1"})
	if section(c, "education") != nil {
		t.Fatal("a section left empty disappears")
	}
	job := section(c, "experience").Entries[0]
	if len(job.Highlights) != 1 || job.Children != nil {
		t.Fatalf("job %+v", job)
	}
	if len(c.Skills) != 2 || c.Skills[0].ID != "s2" {
		t.Fatalf("skills %+v", c.Skills)
	}
	if len(section(b.Content, "experience").Entries[0].Highlights) != 2 {
		t.Fatal("Apply must not change its input")
	}
}

func TestSkillsTrimPastKept(t *testing.T) {
	lib := sample()
	lib.Skills = nil
	for i := range 11 {
		lib.Skills = append(lib.Skills, Skill{ID: string(rune('a' + i)), Name: "x", Type: "tool"})
	}
	b := Build(lib, nil, Picks{}, "en", today)
	if got := b.Trim[len(b.Trim)-3:]; !slices.Equal(got, []string{"k", "j", "i"}) {
		t.Fatalf("skills past the first %d trim from the end: %v", KeptSkills, b.Trim)
	}
}

// The template fixtures and Build must agree on the content format.
func TestContentMatchesTemplateSchema(t *testing.T) {
	b := Build(sample(), nil, Picks{}, "en", today)
	raw, _ := json.Marshal(b.Content)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"schema", "locale", "date", "profile", "sections", "skills"} {
		if _, ok := m[k]; !ok {
			t.Errorf("content lacks %s", k)
		}
	}
	if b.Content.Hash() == Apply(b.Content, b.Trim[:1]).Hash() {
		t.Error("hash ignores content")
	}
}

func TestPinnedDescendantsSurviveEveryTrim(t *testing.T) {
	for _, id := range []string{"h3", "proj"} {
		b := Build(sample(), nil, Picks{Include: []string{id}}, "en", today)
		for k := 0; k <= len(b.Trim); k++ {
			raw, _ := json.Marshal(Apply(b.Content, b.Trim[:k]))
			var c Content
			_ = json.Unmarshal(raw, &c)
			exp := section(c, "experience")
			if exp == nil || !slices.Contains(ids(exp.Entries), "job") {
				t.Fatalf("pin %s was lost with its ancestor at trim %d", id, k)
			}
		}
	}
}

func TestIncludeHighlightSelectsEntryAndCareerBreakNeedsPick(t *testing.T) {
	ranking := &match.Result{Selection: []match.Selection{{EvidenceID: "brk"}}}
	b := Build(sample(), ranking, Picks{Include: []string{"h3"}}, "en", today)
	exp := section(b.Content, "experience")
	if exp == nil || len(exp.Entries) != 1 || exp.Entries[0].ID != "job" {
		t.Fatalf("including a highlight must select its entry: %+v", b.Content)
	}
	if section(b.Content, "unpaid") != nil {
		t.Fatal("a match must not include a career break without a pick")
	}
}

func TestStaleMatchParentDoesNotHideActiveChild(t *testing.T) {
	lib := sample()
	child := lib.Evidence[1]
	child.Highlights = []Highlight{{"child-highlight", "Still active", true}}
	// An archived parent is absent from loadLibrary, but remains in the
	// completed match's selection. Its active child must stand alone.
	lib.Evidence = []Evidence{child}
	ranking := &match.Result{Selection: []match.Selection{{EvidenceID: "job"}, {EvidenceID: "proj"}}}
	for _, pins := range [][]string{{}, {"proj"}, {"child-highlight"}} {
		b := Build(lib, ranking, Picks{Include: pins}, "en", today)
		projects := section(b.Content, "projects")
		if projects == nil || !slices.Equal(ids(projects.Entries), []string{"proj"}) {
			t.Fatalf("pins %v: active child disappeared under an archived parent: %+v", pins, b.Content)
		}
		if len(pins) > 0 && slices.Contains(b.Trim, "proj") {
			t.Fatalf("pins %v: pinned child can be trimmed", pins)
		}
	}
}
