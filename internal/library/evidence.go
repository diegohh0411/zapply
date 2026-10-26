package library

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Evidence is one evidence document: the evidence, its skills and highlights.
type Evidence struct {
	ID             uuid.UUID
	ParentID       *uuid.UUID
	OrganizationID *uuid.UUID
	Category       Category
	Form           Form
	Kind           string
	Title          string
	Summary        string
	Location       string
	URL            string
	Start, End     *PartialDate
	IsCurrent      bool
	Details        map[string]any
	AutoInclude    bool
	Position       string
	Version        int
	ContentHash    []byte
	ArchivedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Skills         []string
	Highlights     []Highlight
	Children       []ChildSummary // only on single reads
}

type Highlight struct {
	ID          uuid.UUID
	Text        string
	MetricValue *float64
	MetricUnit  string
	AutoInclude bool
	Skills      []string
	ContentHash []byte
}

type ChildSummary struct {
	ID       uuid.UUID
	Kind     string
	Form     Form
	Title    string
	Archived bool
}

// EvidenceInput is a create or full-replace request. ParentID is only read on
// create; re-parenting goes through Move.
type EvidenceInput struct {
	Kind           string
	Category       Category // optional for children: inherits the parent's
	ParentID       *uuid.UUID
	OrganizationID *uuid.UUID
	Title          string
	Summary        string
	Location       string
	URL            string
	Start, End     string
	IsCurrent      bool
	Details        map[string]any
	AutoInclude    *bool // default true; false for career_break
	Skills         []string
	Highlights     []HighlightInput
}

type HighlightInput struct {
	ID          *uuid.UUID // keeps an existing highlight (and its scores) on update
	Text        string
	MetricValue *float64
	MetricUnit  string
	AutoInclude *bool
	Skills      []string
}

const (
	maxTitle      = 300
	maxSummary    = 5000
	maxShortText  = 300
	maxURL        = 2000
	maxHighlight  = 1000
	maxHighlights = 50
	maxSkills     = 100
	maxSkillName  = 100
	maxMetricUnit = 30
)

// validated is EvidenceInput after validation and normalization.
type validated struct {
	kind  *Kind
	in    EvidenceInput
	start *PartialDate
	end   *PartialDate
}

// validate checks everything that does not need the database. Parent rules
// are checked by the service once the parent is loaded.
func validate(in EvidenceInput) (validated, []FieldError) {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{field, msg}) }

	v := validated{in: in}
	k, ok := LookupKind(in.Kind)
	if !ok {
		add("kind", "unknown kind")
		return v, errs
	}
	v.kind = k

	if in.Category != "" && !k.Allows(in.Category) {
		add("category", fmt.Sprintf("kind %s allows %s", k.Name, joinCategories(k.Categories)))
	}
	if in.Category == "" && in.ParentID == nil {
		add("category", "is required")
	}
	if k.ChildOnly && in.ParentID == nil {
		add("parent_id", fmt.Sprintf("kind %s must be nested under an engagement", k.Name))
	}

	v.in.Title = strings.TrimSpace(in.Title)
	checkText(add, "title", v.in.Title, maxTitle, true)
	v.in.Summary = strings.TrimSpace(in.Summary)
	checkText(add, "summary", v.in.Summary, maxSummary, false)
	v.in.Location = strings.TrimSpace(in.Location)
	checkText(add, "location", v.in.Location, maxShortText, false)
	v.in.URL = strings.TrimSpace(in.URL)
	if v.in.URL != "" {
		if u, err := url.Parse(v.in.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(v.in.URL) > maxURL {
			add("url", "must be an http(s) URL")
		}
	}

	if in.Start != "" {
		d, err := ParsePartialDate(in.Start)
		if err != nil {
			add("start", err.Error())
		} else {
			v.start = &d
		}
	} else if k.Form == Engagement {
		add("start", "is required for engagements")
	}
	if in.End != "" {
		d, err := ParsePartialDate(in.End)
		switch {
		case err != nil:
			add("end", err.Error())
		case k.Form == Output || k.Form == Recognition:
			add("end", fmt.Sprintf("%s evidence has a single date", k.Form))
		case in.Start == "":
			add("end", "requires start")
		case v.start != nil && !d.NotBefore(*v.start):
			add("end", "must not be before start")
		default:
			v.end = &d
		}
	}
	if in.IsCurrent {
		if k.Form != Engagement {
			add("is_current", "only engagements can be current")
		} else if in.End != "" {
			add("is_current", "a current engagement has no end")
		}
	}

	details := in.Details
	if details == nil {
		details = map[string]any{}
	}
	clean, derrs := k.ValidateDetails(details)
	errs = append(errs, derrs...)
	v.in.Details = clean

	if in.AutoInclude == nil {
		ai := k.Name != "career_break"
		v.in.AutoInclude = &ai
	}

	v.in.Skills = normalizeSkillList(add, "skills", in.Skills)

	if len(in.Highlights) > maxHighlights {
		// Don't validate (or allocate for) an oversized list item by item.
		add("highlights", fmt.Sprintf("at most %d", maxHighlights))
		return v, capErrors(errs)
	}
	seen := map[uuid.UUID]bool{}
	v.in.Highlights = make([]HighlightInput, len(in.Highlights))
	for i, h := range in.Highlights {
		p := fmt.Sprintf("highlights[%d]", i)
		h.Text = strings.TrimSpace(h.Text)
		checkText(add, p+".text", h.Text, maxHighlight, true)
		h.MetricValue = canonicalZero(h.MetricValue)
		h.MetricUnit = strings.TrimSpace(h.MetricUnit)
		if h.MetricUnit != "" && h.MetricValue == nil {
			add(p+".metric_unit", "requires metric_value")
		}
		checkText(add, p+".metric_unit", h.MetricUnit, maxMetricUnit, false)
		if h.ID != nil {
			if seen[*h.ID] {
				add(p+".id", "duplicate highlight id")
			}
			seen[*h.ID] = true
		}
		if h.AutoInclude == nil {
			t := true
			h.AutoInclude = &t
		}
		h.Skills = normalizeSkillList(add, p+".skills", h.Skills)
		v.in.Highlights[i] = h
	}
	return v, capErrors(errs)
}

// maxFieldErrors bounds a validation response, however broken the request.
const maxFieldErrors = 50

func capErrors(errs []FieldError) []FieldError {
	if len(errs) <= maxFieldErrors {
		return errs
	}
	return append(errs[:maxFieldErrors:maxFieldErrors], FieldError{"", fmt.Sprintf("%d more errors not shown", len(errs)-maxFieldErrors)})
}

// canonicalZero turns -0 into 0: Postgres numeric has no negative zero, so
// hashing -0 would disagree with the value read back.
func canonicalZero(f *float64) *float64 {
	if f != nil && *f == 0 {
		z := 0.0
		return &z
	}
	return f
}

func checkText(add func(string, string), field, s string, max int, required bool) {
	switch {
	case required && s == "":
		add(field, "is required")
	case utf8.RuneCountInString(s) > max:
		add(field, fmt.Sprintf("must be at most %d characters", max))
	}
}

// normalizeSkillList trims and de-duplicates skill names (case-insensitively),
// keeping the first spelling.
func normalizeSkillList(add func(string, string), field string, names []string) []string {
	if len(names) > maxSkills {
		add(field, fmt.Sprintf("at most %d skills", maxSkills))
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for i, n := range names {
		n = strings.Join(strings.Fields(n), " ")
		if n == "" {
			add(fmt.Sprintf("%s[%d]", field, i), "must not be empty")
			continue
		}
		if utf8.RuneCountInString(n) > maxSkillName {
			add(fmt.Sprintf("%s[%d]", field, i), fmt.Sprintf("must be at most %d characters", maxSkillName))
			continue
		}
		if key := NormalizeName(n); !seen[key] {
			seen[key] = true
			out = append(out, n)
		}
	}
	return out
}

// NormalizeName is the uniqueness key for skill and organization names.
func NormalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func joinCategories(cs []Category) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = string(c)
	}
	return strings.Join(s, ", ")
}

// Content hashes cover exactly what matching reads, so part 6 can cache
// scores by hash. Presentation-only fields (location, url, auto_include,
// position) are excluded.

func highlightHash(h Highlight) []byte {
	return hashJSON(struct {
		Text        string   `json:"text"`
		MetricValue *float64 `json:"metric_value"`
		MetricUnit  string   `json:"metric_unit"`
		Skills      []string `json:"skills"`
	}{h.Text, h.MetricValue, h.MetricUnit, sortedSkills(h.Skills)})
}

func evidenceHash(e Evidence, organization string) []byte {
	hs := make([]string, len(e.Highlights))
	for i, h := range e.Highlights {
		hs[i] = fmt.Sprintf("%x", h.ContentHash)
	}
	var start, end string
	if e.Start != nil {
		start = e.Start.String()
	}
	if e.End != nil {
		end = e.End.String()
	}
	return hashJSON(struct {
		Kind         string         `json:"kind"`
		Category     Category       `json:"category"`
		Title        string         `json:"title"`
		Summary      string         `json:"summary"`
		Start        string         `json:"start"`
		End          string         `json:"end"`
		IsCurrent    bool           `json:"is_current"`
		Details      map[string]any `json:"details"`
		Organization string         `json:"organization"`
		Skills       []string       `json:"skills"`
		Highlights   []string       `json:"highlights"`
	}{e.Kind, e.Category, e.Title, e.Summary, start, end, e.IsCurrent, e.Details, organization, sortedSkills(e.Skills), hs})
}

func sortedSkills(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = NormalizeName(n)
	}
	sort.Strings(out)
	return out
}

func hashJSON(v any) []byte {
	b, err := json.Marshal(v) // map keys are sorted, so this is canonical
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return sum[:]
}
