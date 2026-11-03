// Package render builds CVs (part 7, docs/parts/07-rendering.md).
//
// Build turns the library, the job's latest match run and the user's picks
// into the content a template compiles, plus the order in which optional
// items are trimmed to fit the page budget. It only selects and orders: every
// string in the content is copied from the library.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/diegohh0411/zapply/internal/match"
)

// Schema is the content format version the templates read.
const Schema = 1

// Library is what Build reads, as loaded by loadLibrary. Evidence is flat,
// in library order, without archived items.
type Library struct {
	Profile  Profile    `json:"profile"`
	Evidence []Evidence `json:"evidence"`
	Skills   []Skill    `json:"skills"`
}

type Profile struct {
	Name     string `json:"name"`
	Headline string `json:"headline"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Location string `json:"location"`
	Links    []Link `json:"links"`
	Summary  string `json:"summary"`
}

type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type Evidence struct {
	ID             string         `json:"id"`
	ParentID       *string        `json:"parent_id"`
	Kind           string         `json:"kind"`
	Category       string         `json:"category"`
	Form           string         `json:"form"`
	Title          string         `json:"title"`
	Organization   string         `json:"organization"`
	Summary        string         `json:"summary"`
	Location       string         `json:"location"`
	URL            string         `json:"url"`
	StartDate      *string        `json:"start_date"`
	StartPrecision *string        `json:"start_precision"`
	EndDate        *string        `json:"end_date"`
	EndPrecision   *string        `json:"end_precision"`
	IsCurrent      bool           `json:"is_current"`
	AutoInclude    bool           `json:"auto_include"`
	Details        map[string]any `json:"details"`
	Highlights     []Highlight    `json:"highlights"`
}

type Highlight struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	AutoInclude bool   `json:"auto_include"`
}

type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Proficiency string `json:"proficiency"`
}

// Picks are the user's choices for one CV: an included item is pinned and
// never trimmed; an excluded one never appears. IDs are evidence or
// highlight IDs.
type Picks struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// Content is what a template compiles (templates/README.md, schema 1).
type Content struct {
	Schema   int            `json:"schema"`
	Locale   string         `json:"locale"`
	Date     string         `json:"date"`
	Profile  ContentProfile `json:"profile"`
	Sections []Section      `json:"sections"`
	Skills   []ContentSkill `json:"skills"`
}

type ContentProfile struct {
	Name     string `json:"name"`
	Headline string `json:"headline,omitempty"`
	Email    string `json:"email,omitempty"`
	Phone    string `json:"phone,omitempty"`
	Location string `json:"location,omitempty"`
	Links    []Link `json:"links,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

type Section struct {
	Key     string  `json:"key"`
	Entries []Entry `json:"entries"`
}

type Entry struct {
	ID           string             `json:"id"`
	Kind         string             `json:"kind"`
	Form         string             `json:"form"`
	Title        string             `json:"title"`
	Organization string             `json:"organization,omitempty"`
	Location     string             `json:"location,omitempty"`
	URL          string             `json:"url,omitempty"`
	Start        *Date              `json:"start,omitempty"`
	End          *Date              `json:"end,omitempty"`
	Current      bool               `json:"current,omitempty"`
	Summary      string             `json:"summary,omitempty"`
	Details      map[string]any     `json:"details,omitempty"`
	Highlights   []ContentHighlight `json:"highlights,omitempty"`
	Children     []Entry            `json:"children,omitempty"`
}

type Date struct {
	Date      string `json:"date"`
	Precision string `json:"precision"`
}

type ContentHighlight struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type ContentSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Proficiency string `json:"proficiency,omitempty"`
}

// Hash is the SHA-256 of the content's JSON, as stored and compared.
func (c Content) Hash() string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Sections print in this order; Build drops the empty ones.
var sectionOrder = []string{"experience", "education", "credentials", "projects", "publications",
	"presentations", "patents", "creative", "awards", "funding", "unpaid"}

// sectionOf places top-level evidence. Nested evidence stays under its
// parent, whatever its kind.
func sectionOf(e Evidence) string {
	switch e.Form {
	case "credential":
		return "credentials"
	case "output":
		switch e.Kind {
		case "publication":
			return "publications"
		case "presentation":
			return "presentations"
		case "patent":
			return "patents"
		case "creative_work":
			return "creative"
		}
		return "projects"
	case "recognition":
		if e.Kind == "funding" {
			return "funding"
		}
		return "awards"
	}
	switch e.Category {
	case "paid":
		return "experience"
	case "education":
		return "education"
	}
	return "unpaid"
}

// MaxSkills caps the skills listed (languages aside); KeptSkills of them are
// never trimmed.
const (
	MaxSkills  = 20
	KeptSkills = 8
)

// Available is library material the CV leaves out, offered for inclusion.
type Available struct {
	Entries    []AvailableEntry     `json:"entries"`
	Highlights []AvailableHighlight `json:"highlights"`
}

type AvailableEntry struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Section string `json:"section"`
	// Reason is excluded (by the user), suggested (matching found it
	// relevant but it isn't auto-included), or not_selected.
	Reason string `json:"reason"`
}

type AvailableHighlight struct {
	ID         string `json:"id"`
	EvidenceID string `json:"evidence_id"`
	Text       string `json:"text"`
	Excluded   bool   `json:"excluded"`
}

// Built is a draft's content before fitting.
type Built struct {
	Content Content `json:"content"`
	// Trim lists optional item IDs, least relevant first. Fitting drops a
	// prefix of it.
	Trim      []string  `json:"trim"`
	Available Available `json:"available"`
}

// Build selects and orders the CV. ranking is the latest completed match
// run's result, or nil, in which case auto-included items appear, most
// recent first.
func Build(lib Library, ranking *match.Result, picks Picks, locale string, date time.Time) Built {
	include := set(picks.Include)
	exclude := set(picks.Exclude)
	// Which evidence appears, and how relevant each is.
	relevance := map[string]float64{}
	ranked := map[string][]string{} // evidence → highlight IDs, most relevant first
	matched := map[string]bool{}
	selected := map[string]bool{}
	suggested := map[string]bool{}
	supports := map[string]bool{}
	if ranking != nil {
		for _, s := range ranking.Evidence {
			relevance[s.EvidenceID] = s.Relevance
			ranked[s.EvidenceID] = s.HighlightIDs
		}
		for _, s := range ranking.Selection {
			matched[s.EvidenceID] = true
		}
		for _, s := range ranking.Suggestions {
			suggested[s.EvidenceID] = true
		}
		for _, r := range ranking.Requirements {
			if r.Class == "must" && r.Status == "met" && r.EvidenceID != "" {
				supports[r.EvidenceID] = true
			}
		}
	}
	for _, e := range lib.Evidence {
		if _, ok := relevance[e.ID]; !ok {
			relevance[e.ID] = recency(e, date)
		}
		baseline := e.AutoInclude && e.Kind != "career_break"
		if ranking != nil {
			// A CV without its degrees reads as an error, so they stay even
			// when matching found them irrelevant. They can still be trimmed.
			baseline = matched[e.ID] || baseline && e.Kind == "degree_program" && e.ParentID == nil
		}
		if e.Kind == "career_break" {
			baseline = false
		}
		// Including a highlight also selects its entry, unless the entry
		// itself is explicitly excluded.
		baseline = baseline || slices.ContainsFunc(e.Highlights, func(h Highlight) bool { return include[h.ID] })
		if include[e.ID] || baseline && !exclude[e.ID] {
			selected[e.ID] = true
		} else {
			delete(selected, e.ID)
		}
	}
	// An entry whose parent is left out appears on its own.
	nested := func(e Evidence) bool { return e.ParentID != nil && selected[*e.ParentID] }

	highlights := map[string][]ContentHighlight{}
	for _, e := range lib.Evidence {
		if !selected[e.ID] {
			continue
		}
		order, ok := ranked[e.ID]
		if !ok {
			for _, h := range e.Highlights {
				if h.AutoInclude {
					order = append(order, h.ID)
				}
			}
		}
		text := map[string]string{}
		for _, h := range e.Highlights {
			text[h.ID] = h.Text
			if include[h.ID] && !slices.Contains(order, h.ID) {
				order = append(order, h.ID)
			}
		}
		hs := []ContentHighlight{}
		for _, id := range order {
			if t, ok := text[id]; ok && !exclude[id] {
				hs = append(hs, ContentHighlight{id, t})
			}
		}
		highlights[e.ID] = hs
	}

	var entry func(e Evidence) Entry
	entry = func(e Evidence) Entry {
		out := Entry{ID: e.ID, Kind: e.Kind, Form: e.Form, Title: e.Title, Organization: e.Organization,
			Location: e.Location, URL: e.URL, Current: e.IsCurrent, Summary: e.Summary,
			Start: date_(e.StartDate, e.StartPrecision), End: date_(e.EndDate, e.EndPrecision), Highlights: highlights[e.ID]}
		if len(e.Details) > 0 {
			out.Details = e.Details
		}
		for _, c := range lib.Evidence {
			if c.ParentID != nil && *c.ParentID == e.ID && selected[c.ID] {
				out.Children = append(out.Children, entry(c))
			}
		}
		sortEntries(out.Children)
		return out
	}
	bySection := map[string][]Entry{}
	for _, e := range lib.Evidence {
		if selected[e.ID] && !nested(e) {
			k := sectionOf(e)
			bySection[k] = append(bySection[k], entry(e))
		}
	}
	content := Content{Schema: Schema, Locale: locale, Date: date.Format(time.DateOnly), Sections: []Section{}, Skills: []ContentSkill{},
		Profile: ContentProfile{lib.Profile.Name, lib.Profile.Headline, lib.Profile.Email, lib.Profile.Phone, lib.Profile.Location, lib.Profile.Links, lib.Profile.Summary}}
	for _, k := range sectionOrder {
		if es := bySection[k]; len(es) > 0 {
			sortEntries(es)
			content.Sections = append(content.Sections, Section{k, es})
		}
	}

	// Skills: ranked ones that matching found relevant, else the library's.
	skills := []Skill{}
	skillByID := map[string]Skill{}
	for _, s := range lib.Skills {
		skillByID[s.ID] = s
	}
	if ranking != nil {
		for _, r := range ranking.Skills {
			if s, ok := skillByID[r.SkillID]; ok && r.Relevance > 0 && s.Type != "language" {
				skills = append(skills, s)
			}
		}
	} else {
		for _, s := range lib.Skills {
			if s.Type != "language" {
				skills = append(skills, s)
			}
		}
	}
	skills = skills[:min(len(skills), MaxSkills)]
	for _, s := range lib.Skills {
		if s.Type == "language" {
			skills = append(skills, s)
		}
	}
	for _, s := range skills {
		content.Skills = append(content.Skills, ContentSkill{s.ID, s.Name, s.Type, s.Proficiency})
	}

	return Built{Content: content, Trim: trimOrder(content, relevance, include, supports), Available: available(lib, selected, exclude, suggested, highlights)}
}

// trimOrder: highlights first (never an entry's first), then whole entries,
// keeping the evidence for met must-haves for last, then skills past
// KeptSkills. Pins protect both the item and its containing entries.
func trimOrder(c Content, relevance map[string]float64, pinned, supports map[string]bool) []string {
	type item struct {
		id    string
		score float64
	}
	var hs, es []item
	var walk func(e Entry) (bool, bool)
	walk = func(e Entry) (bool, bool) {
		protected, supporting := pinned[e.ID], supports[e.ID]
		for i, h := range e.Highlights {
			protected = protected || pinned[h.ID]
			if i > 0 && !pinned[h.ID] {
				hs = append(hs, item{h.ID, relevance[e.ID] * math.Pow(0.7, float64(i))})
			}
		}
		for _, child := range e.Children {
			pin, support := walk(child)
			protected, supporting = protected || pin, supporting || support
		}
		if !protected {
			s := relevance[e.ID]
			if supporting {
				s += 1000
			}
			es = append(es, item{e.ID, s})
		}
		return protected, supporting
	}
	for _, s := range c.Sections {
		for _, e := range s.Entries {
			walk(e)
		}
	}
	byScore := func(xs []item) {
		sort.SliceStable(xs, func(i, j int) bool {
			if xs[i].score != xs[j].score {
				return xs[i].score < xs[j].score
			}
			return xs[i].id < xs[j].id
		})
	}
	byScore(hs)
	byScore(es)
	out := []string{}
	for _, h := range hs {
		out = append(out, h.id)
	}
	for _, e := range es {
		out = append(out, e.id)
	}
	var other []string
	for _, s := range c.Skills {
		if s.Type != "language" {
			other = append(other, s.ID)
		}
	}
	for i := len(other) - 1; i >= KeptSkills; i-- {
		out = append(out, other[i])
	}
	return out
}

func available(lib Library, selected, exclude, suggested map[string]bool, shown map[string][]ContentHighlight) Available {
	out := Available{Entries: []AvailableEntry{}, Highlights: []AvailableHighlight{}}
	for _, e := range lib.Evidence {
		if !selected[e.ID] {
			reason := "not_selected"
			if exclude[e.ID] {
				reason = "excluded"
			} else if suggested[e.ID] {
				reason = "suggested"
			}
			out.Entries = append(out.Entries, AvailableEntry{e.ID, e.Title, e.Kind, sectionOf(e), reason})
			continue
		}
		for _, h := range e.Highlights {
			if !slices.ContainsFunc(shown[e.ID], func(c ContentHighlight) bool { return c.ID == h.ID }) {
				out.Highlights = append(out.Highlights, AvailableHighlight{h.ID, e.ID, h.Text, exclude[h.ID]})
			}
		}
	}
	return out
}

// Apply returns c without the dropped entries, highlights and skills. The
// web app does the same while fitting (web/src/app/jobs/cv/cv-content.ts).
func Apply(c Content, dropped []string) Content {
	drop := set(dropped)
	var entries func([]Entry) []Entry
	entries = func(in []Entry) []Entry {
		var out []Entry
		for _, e := range in {
			if drop[e.ID] {
				continue
			}
			var hs []ContentHighlight
			for _, h := range e.Highlights {
				if !drop[h.ID] {
					hs = append(hs, h)
				}
			}
			e.Highlights = hs
			e.Children = entries(e.Children)
			out = append(out, e)
		}
		return out
	}
	out := c
	out.Sections = []Section{}
	for _, s := range c.Sections {
		if es := entries(s.Entries); len(es) > 0 {
			out.Sections = append(out.Sections, Section{s.Key, es})
		}
	}
	out.Skills = []ContentSkill{}
	for _, s := range c.Skills {
		if !drop[s.ID] {
			out.Skills = append(out.Skills, s)
		}
	}
	return out
}

func set(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// date_ keeps a library date at its precision: 2019, 2021-03 or 2023-06-14.
func date_(d, precision *string) *Date {
	if d == nil || precision == nil || len(*d) < 10 {
		return nil
	}
	n := map[string]int{"year": 4, "month": 7, "day": 10}[*precision]
	if n == 0 {
		return nil
	}
	return &Date{(*d)[:n], *precision}
}

// recency ranks evidence without a match run: current first, then by how
// recently it ended (or started).
func recency(e Evidence, now time.Time) float64 {
	if e.IsCurrent {
		return 1
	}
	d := e.EndDate
	if d == nil {
		d = e.StartDate
	}
	if d == nil {
		return 0
	}
	t, err := time.Parse(time.DateOnly, (*d)[:min(len(*d), 10)])
	if err != nil {
		return 0
	}
	return 1 / (1 + math.Max(0, now.Sub(t).Hours()/24/365.2425))
}

// sortEntries puts current entries first, then the latest end (or start).
func sortEntries(es []Entry) {
	key := func(e Entry) string {
		if e.Current {
			return "9999"
		}
		if e.End != nil {
			return e.End.Date
		}
		if e.Start != nil {
			return e.Start.Date
		}
		return ""
	}
	sort.SliceStable(es, func(i, j int) bool { return key(es[i]) > key(es[j]) })
}
