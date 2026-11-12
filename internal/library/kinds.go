package library

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

type Category string

const (
	Education Category = "education"
	Paid      Category = "paid"
	Unpaid    Category = "unpaid"
)

var Categories = []Category{Education, Paid, Unpaid}

type Form string

const (
	Engagement  Form = "engagement"
	Credential  Form = "credential"
	Output      Form = "output"
	Recognition Form = "recognition"
)

// FieldType is the type of one `details` field.
type FieldType string

const (
	Text     FieldType = "string"
	Enum     FieldType = "enum"
	Integer  FieldType = "integer"
	Number   FieldType = "number"
	TextList FieldType = "string_list"
)

type Field struct {
	Name     string
	Type     FieldType
	Required bool
	Values   []string // for Enum
}

// Kind is one entry of the fixed catalog. The evidence_kind table mirrors
// Name, Form, Categories and ChildOnly; a test keeps them in sync.
type Kind struct {
	Name       string
	Form       Form
	Categories []Category
	ChildOnly  bool
	Fields     []Field
}

func text(name string) Field                   { return Field{Name: name, Type: Text} }
func required(f Field) Field                   { f.Required = true; return f }
func enum(name string, values ...string) Field { return Field{Name: name, Type: Enum, Values: values} }
func integer(name string) Field                { return Field{Name: name, Type: Integer} }
func number(name string) Field                 { return Field{Name: name, Type: Number} }
func textList(name string) Field               { return Field{Name: name, Type: TextList} }

var any3 = []Category{Education, Paid, Unpaid}

var workMode = enum("work_mode", "onsite", "remote", "hybrid")

// Kinds is the fixed catalog, in display order.
var Kinds = []Kind{
	{Name: "degree_program", Form: Engagement, Categories: []Category{Education}, Fields: []Field{
		required(enum("degree_type", "secondary", "associate", "bachelor", "master", "doctorate", "diploma", "other")),
		text("field_of_study"),
		required(enum("status", "completed", "in_progress", "incomplete")),
		text("grade"),
		text("honors"),
	}},
	{Name: "course", Form: Engagement, Categories: []Category{Education}, Fields: []Field{
		enum("format", "course", "bootcamp", "workshop", "mooc", "training"),
		text("provider_url"),
		number("hours"),
	}},
	{Name: "job", Form: Engagement, Categories: []Category{Paid}, Fields: []Field{
		enum("employment_type", "full_time", "part_time", "temporary", "seasonal"),
		workMode,
	}},
	{Name: "freelance", Form: Engagement, Categories: []Category{Paid}, Fields: []Field{workMode}},
	{Name: "business", Form: Engagement, Categories: []Category{Paid}, Fields: []Field{
		enum("role", "founder", "co_founder", "owner", "partner"),
		text("stage"),
		integer("team_size"),
	}},
	{Name: "internship", Form: Engagement, Categories: any3, Fields: []Field{workMode}},
	{Name: "apprenticeship", Form: Engagement, Categories: []Category{Paid, Education}, Fields: []Field{text("trade")}},
	{Name: "military_service", Form: Engagement, Categories: []Category{Paid}, Fields: []Field{text("branch"), text("rank")}},
	{Name: "volunteering", Form: Engagement, Categories: []Category{Unpaid}, Fields: []Field{text("cause")}},
	{Name: "membership", Form: Engagement, Categories: []Category{Unpaid, Education}, Fields: []Field{
		enum("role", "member", "officer", "board", "chair"),
	}},
	{Name: "career_break", Form: Engagement, Categories: []Category{Unpaid}, Fields: []Field{text("label")}},
	{Name: "assignment", Form: Engagement, Categories: any3, ChildOnly: true, Fields: []Field{text("client")}},
	{Name: "license", Form: Credential, Categories: []Category{Education}, Fields: []Field{
		required(text("issuer")), text("number"), text("jurisdiction"),
	}},
	{Name: "certification", Form: Credential, Categories: []Category{Education}, Fields: []Field{
		required(text("issuer")), text("credential_id"), text("verify_url"), text("score"),
	}},
	{Name: "clearance", Form: Credential, Categories: []Category{Education}, Fields: []Field{
		required(text("level")), text("issuer"),
	}},
	{Name: "project", Form: Output, Categories: any3, Fields: []Field{text("role")}},
	{Name: "publication", Form: Output, Categories: any3, Fields: []Field{
		required(enum("publication_type", "journal", "conference", "book", "chapter", "preprint", "thesis", "article", "report")),
		text("venue"), textList("authors"), text("doi"),
	}},
	{Name: "presentation", Form: Output, Categories: any3, Fields: []Field{
		required(text("event")), enum("format", "talk", "poster", "workshop", "panel"),
	}},
	{Name: "patent", Form: Output, Categories: any3, Fields: []Field{
		text("number"), text("office"), enum("status", "filed", "granted"),
	}},
	{Name: "creative_work", Form: Output, Categories: any3, Fields: []Field{
		enum("medium", "exhibition", "performance", "film", "recording", "portfolio", "other"), text("venue"),
	}},
	{Name: "award", Form: Recognition, Categories: any3, Fields: []Field{required(text("issuer"))}},
	{Name: "funding", Form: Recognition, Categories: any3, Fields: []Field{
		required(enum("funding_type", "grant", "scholarship", "fellowship")),
		required(text("funder")), number("amount"), text("currency"),
	}},
}

var kindsByName = func() map[string]*Kind {
	m := make(map[string]*Kind, len(Kinds))
	for i := range Kinds {
		m[Kinds[i].Name] = &Kinds[i]
	}
	return m
}()

// LookupKind returns the catalog entry for name.
func LookupKind(name string) (*Kind, bool) {
	k, ok := kindsByName[name]
	return k, ok
}

func (k *Kind) Allows(c Category) bool { return slices.Contains(k.Categories, c) }

const maxTextField = 500

// ValidateDetails checks details against the kind's fields and returns a
// cleaned copy: strings trimmed, empty optional values dropped.
func (k *Kind) ValidateDetails(details map[string]any) (map[string]any, []FieldError) {
	var errs []FieldError
	out := map[string]any{}
	known := map[string]Field{}
	for _, f := range k.Fields {
		known[f.Name] = f
	}
	names := make([]string, 0, len(details))
	for n := range details {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, ok := known[n]; !ok {
			errs = append(errs, FieldError{"details." + n, fmt.Sprintf("unknown field for kind %s", k.Name)})
		}
	}
	for _, f := range k.Fields {
		path := "details." + f.Name
		v, present := details[f.Name]
		clean, err := f.check(v, present)
		switch {
		case err != "":
			errs = append(errs, FieldError{path, err})
		case clean != nil:
			out[f.Name] = clean
		case f.Required:
			errs = append(errs, FieldError{path, "is required"})
		}
	}
	return out, errs
}

// check validates one value; it returns (nil, "") for an absent or empty value.
func (f Field) check(v any, present bool) (any, string) {
	if !present || v == nil {
		return nil, ""
	}
	switch f.Type {
	case Text, Enum:
		s, ok := v.(string)
		if !ok {
			return nil, "must be a string"
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, ""
		}
		if len(s) > maxTextField {
			return nil, fmt.Sprintf("must be at most %d characters", maxTextField)
		}
		if f.Type == Enum && !slices.Contains(f.Values, s) {
			return nil, "must be one of " + strings.Join(f.Values, ", ")
		}
		return s, ""
	case Integer, Number:
		n, ok := v.(float64)
		if !ok {
			if jn, isNum := v.(json.Number); isNum {
				var err error
				if n, err = jn.Float64(); err != nil {
					return nil, "must be a number"
				}
				ok = true
			}
		}
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, "must be a number"
		}
		if n < 0 {
			return nil, "must not be negative"
		}
		n = *canonicalZero(&n)
		if f.Type == Integer {
			if n != math.Trunc(n) || n > math.MaxInt32 {
				return nil, "must be a whole number"
			}
			return int64(n), ""
		}
		return n, ""
	case TextList:
		items, ok := v.([]any)
		if !ok {
			return nil, "must be a list of strings"
		}
		var out []string
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return nil, "must be a list of strings"
			}
			if s = strings.TrimSpace(s); s != "" {
				if len(s) > maxTextField {
					return nil, fmt.Sprintf("items must be at most %d characters", maxTextField)
				}
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil, ""
		}
		return out, ""
	}
	return nil, "unsupported field type"
}

// JSONSchema describes the kind's details for API clients.
func (k *Kind) JSONSchema() map[string]any {
	props := map[string]any{}
	var req []string
	for _, f := range k.Fields {
		var p map[string]any
		switch f.Type {
		case Text:
			p = map[string]any{"type": "string", "maxLength": maxTextField}
		case Enum:
			p = map[string]any{"type": "string", "enum": f.Values}
		case Integer:
			p = map[string]any{"type": "integer", "minimum": 0}
		case Number:
			p = map[string]any{"type": "number", "minimum": 0}
		case TextList:
			p = map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": maxTextField}}
		}
		props[f.Name] = p
		if f.Required {
			req = append(req, f.Name)
		}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		s["required"] = req
	}
	return s
}

// FieldError is one validation failure, addressed by JSON path.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}
