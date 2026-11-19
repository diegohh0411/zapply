package posting

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// findJobPosting returns the first schema.org JobPosting in the page's
// JSON-LD scripts, or nil.
func findJobPosting(page string) map[string]any {
	z := html.NewTokenizer(strings.NewReader(page))
	inLD := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return nil
		case html.StartTagToken:
			name, hasAttr := z.TagName()
			inLD = false
			if string(name) != "script" {
				continue
			}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if string(k) == "type" && strings.EqualFold(strings.TrimSpace(string(v)), "application/ld+json") {
					inLD = true
				}
			}
		case html.TextToken:
			if !inLD {
				continue
			}
			inLD = false
			var doc any
			if json.Unmarshal(z.Text(), &doc) != nil {
				continue
			}
			if p := jobPostingIn(doc, 0); p != nil {
				return p
			}
		case html.EndTagToken:
			inLD = false
		}
	}
}

// jobPostingIn walks a JSON-LD value: a node, a list of nodes, or a node
// with an @graph.
func jobPostingIn(v any, depth int) map[string]any {
	if depth > 4 {
		return nil
	}
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if p := jobPostingIn(item, depth+1); p != nil {
				return p
			}
		}
	case map[string]any:
		if hasType(t["@type"], "JobPosting") {
			return t
		}
		if g, ok := t["@graph"]; ok {
			return jobPostingIn(g, depth+1)
		}
	}
	return nil
}

func hasType(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return t == want || strings.HasSuffix(t, "/"+want)
	case []any:
		for _, item := range t {
			if hasType(item, want) {
				return true
			}
		}
	}
	return false
}

// fromJobPosting maps a JobPosting node to job fields. A nil node gives an
// empty Posting.
func fromJobPosting(n map[string]any) Posting {
	if n == nil {
		return Posting{}
	}
	p := Posting{
		Title:          text(n["title"]),
		Company:        name(n["hiringOrganization"]),
		Location:       location(n["jobLocation"]),
		EmploymentType: employmentType(n["employmentType"]),
	}
	if strings.EqualFold(text(n["jobLocationType"]), "TELECOMMUTE") {
		p.WorkMode = "remote"
	}
	p.SalaryMin, p.SalaryMax, p.SalaryCurrency, p.SalaryPeriod = salary(n["baseSalary"])
	return p
}

// text is a string value, or the first of a list, unescaped and trimmed.
func text(v any) string {
	switch t := v.(type) {
	case string:
		return strings.Join(strings.Fields(html.UnescapeString(t)), " ")
	case []any:
		for _, item := range t {
			if s := text(item); s != "" {
				return s
			}
		}
	}
	return ""
}

// name reads an Organization or Place: a string, or a node with a name.
func name(v any) string {
	switch t := v.(type) {
	case map[string]any:
		return text(t["name"])
	case []any:
		for _, item := range t {
			if s := name(item); s != "" {
				return s
			}
		}
		return ""
	default:
		return text(v)
	}
}

// location reads the first jobLocation: "Mexico City, CDMX, Mexico".
func location(v any) string {
	if list, ok := v.([]any); ok {
		for _, item := range list {
			if s := location(item); s != "" {
				return s
			}
		}
		return ""
	}
	place, ok := v.(map[string]any)
	if !ok {
		return text(v)
	}
	addr, ok := place["address"].(map[string]any)
	if !ok {
		if s := text(place["address"]); s != "" {
			return s
		}
		return text(place["name"])
	}
	var parts []string
	for _, k := range []string{"addressLocality", "addressRegion", "addressCountry"} {
		s := name(addr[k])
		if s == "" {
			continue
		}
		dup := false
		for _, p := range parts {
			dup = dup || strings.EqualFold(p, s)
		}
		if !dup {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

var employmentTypes = map[string]string{
	"FULL_TIME": "full_time", "FULLTIME": "full_time",
	"PART_TIME": "part_time", "PARTTIME": "part_time",
	"CONTRACTOR": "contract", "CONTRACT": "contract",
	"TEMPORARY": "temporary",
	"INTERN":    "internship", "INTERNSHIP": "internship",
	"FREELANCE": "freelance",
}

func employmentType(v any) string {
	if list, ok := v.([]any); ok {
		for _, item := range list {
			if s := employmentType(item); s != "" {
				return s
			}
		}
		return ""
	}
	key := strings.ToUpper(strings.NewReplacer("-", "_", " ", "_").Replace(text(v)))
	return employmentTypes[key]
}

var salaryPeriods = map[string]string{"YEAR": "year", "MONTH": "month", "HOUR": "hour"}

// salary reads a MonetaryAmount. A period the job API has no value for
// (week, day) drops the salary rather than mislabel it.
func salary(v any) (lo, hi *int64, currency, period string) {
	amount, ok := v.(map[string]any)
	if !ok {
		return nil, nil, "", ""
	}
	currency = strings.ToUpper(text(amount["currency"]))
	value := amount["value"]
	unit := ""
	if q, ok := value.(map[string]any); ok {
		unit = text(q["unitText"])
		lo, hi = number(q["minValue"]), number(q["maxValue"])
		if lo == nil && hi == nil {
			lo = number(q["value"])
		}
	} else {
		lo = number(value)
	}
	if unit == "" {
		unit = text(amount["unitText"])
	}
	if lo == nil && hi == nil {
		return nil, nil, "", ""
	}
	if hi == nil {
		hi = lo
	}
	if lo == nil {
		lo = hi
	}
	if len(currency) != 3 {
		currency = ""
	}
	period, known := salaryPeriods[strings.ToUpper(unit)]
	if unit != "" && !known {
		return nil, nil, "", ""
	}
	return lo, hi, currency, period
}

// number reads a non-negative amount given as a number or a numeric string.
func number(v any) *int64 {
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(t), ",", ""), 64); err != nil {
			return nil
		}
	default:
		return nil
	}
	if f < 0 || math.IsNaN(f) || f > math.MaxInt64/2 {
		return nil
	}
	n := int64(math.Round(f))
	return &n
}
