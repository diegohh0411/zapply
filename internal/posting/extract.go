package posting

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/internal/model/extraction"
)

// minConfidence drops answers the model itself doubts; the field stays empty
// for the person to fill in.
const minConfidence = 0.5

// maxState keeps an extraction request inside its budget with room for the
// field specs.
const maxState = extraction.RequestBudget - 16<<10

// ExtractionRequest asks model for the job fields a posting's text states.
// Models without typed numbers are asked for salary as text, parsed here, so
// the same fields work on every extraction model.
func ExtractionRequest(model, text string) extraction.Request {
	amount := func(which string) extraction.Field {
		instructions := "The " + which + " base salary or pay the posting offers, as a number without currency"
		if extraction.ProfileOf(model).Number {
			return extraction.Field{Type: extraction.Number, Integer: true, Instructions: instructions}
		}
		return extraction.Field{Type: extraction.Text, Instructions: instructions}
	}
	return extraction.Request{Model: model, State: truncate(strings.TrimSpace(text), maxState), Fields: map[string]extraction.Field{
		"title":           {Type: extraction.Text, Instructions: "The job title, as the posting writes it"},
		"company":         {Type: extraction.Text, Instructions: "The company hiring for this job"},
		"location":        {Type: extraction.Text, Instructions: "Where the job is based: city, region and country"},
		"work_mode":       {Type: extraction.Text, Instructions: "Whether the job is remote, hybrid or on-site"},
		"employment_type": {Type: extraction.Text, Instructions: "The kind of employment: full-time, part-time, contract, temporary, internship or freelance"},
		"salary_min":      amount("lowest"),
		"salary_max":      amount("highest"),
		"salary_currency": {Type: extraction.Text, Instructions: "The currency of the salary, such as USD, EUR or MXN"},
		"salary_period":   {Type: extraction.Text, Instructions: "How often the salary is counted: per year, per month or per hour"},
	}}
}

// FromExtraction maps an extraction's answers onto job fields. Answers that
// do not fit a field's values (a work mode the job API has no value for, an
// amount that is not a number) are left out rather than guessed.
func FromExtraction(r extraction.Request, raw string) Posting {
	answers := extraction.Answers(r, raw)
	str := func(name string) string {
		a, ok := answers[name]
		if !ok || a.Type != extraction.Text || (a.Confidence != nil && *a.Confidence < minConfidence) {
			return ""
		}
		var s string
		_ = json.Unmarshal(a.Value, &s)
		return strings.Join(strings.Fields(s), " ")
	}
	num := func(name string) *int64 {
		a, ok := answers[name]
		if !ok || (a.Confidence != nil && *a.Confidence < minConfidence) {
			return nil
		}
		if a.Type == extraction.Number {
			var f float64
			if json.Unmarshal(a.Value, &f) != nil {
				return nil
			}
			return number(f)
		}
		return amount(str(name))
	}
	p := Posting{
		Title:          limit(str("title"), 300),
		Company:        limit(str("company"), 300),
		Location:       limit(str("location"), 300),
		WorkMode:       workMode(str("work_mode")),
		EmploymentType: employmentWords(str("employment_type")),
		SalaryMin:      num("salary_min"),
		SalaryMax:      num("salary_max"),
		SalaryCurrency: currency(str("salary_currency")),
		SalaryPeriod:   period(str("salary_period")),
	}
	if p.SalaryMin == nil && p.SalaryMax == nil {
		p.SalaryCurrency, p.SalaryPeriod = "", ""
		return p
	}
	if p.SalaryMin == nil {
		p.SalaryMin = p.SalaryMax
	}
	if p.SalaryMax == nil {
		p.SalaryMax = p.SalaryMin
	}
	if *p.SalaryMin > *p.SalaryMax {
		p.SalaryMin, p.SalaryMax = p.SalaryMax, p.SalaryMin
	}
	return p
}

func limit(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return ""
}

func workMode(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "hybrid"):
		return "hybrid"
	case strings.Contains(s, "remote"), strings.Contains(s, "work from home"), strings.Contains(s, "telecommut"):
		return "remote"
	case strings.Contains(s, "on-site"), strings.Contains(s, "onsite"), strings.Contains(s, "on site"), strings.Contains(s, "in office"), strings.Contains(s, "in-office"):
		return "onsite"
	}
	return ""
}

func employmentWords(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "intern"):
		return "internship"
	case strings.Contains(s, "part"):
		return "part_time"
	case strings.Contains(s, "full"), strings.Contains(s, "permanent"):
		return "full_time"
	case strings.Contains(s, "freelance"):
		return "freelance"
	case strings.Contains(s, "contract"):
		return "contract"
	case strings.Contains(s, "temp"):
		return "temporary"
	}
	return ""
}

func period(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "hour"), strings.Contains(s, "/hr"):
		return "hour"
	case strings.Contains(s, "month"):
		return "month"
	case strings.Contains(s, "year"), strings.Contains(s, "annual"):
		return "year"
	}
	return ""
}

// currencySymbols maps unambiguous symbols; a bare "$" is left for the person.
var currencySymbols = map[string]string{"€": "EUR", "£": "GBP", "¥": "JPY", "₹": "INR", "US$": "USD", "C$": "CAD", "A$": "AUD", "MX$": "MXN"}

var isoCode = regexp.MustCompile(`\b[A-Z]{3}\b`)

func currency(s string) string {
	s = strings.TrimSpace(s)
	if c, ok := currencySymbols[s]; ok {
		return c
	}
	if m := isoCode.FindString(strings.ToUpper(s)); m != "" {
		return m
	}
	return ""
}

var amountPattern = regexp.MustCompile(`(\d[\d,.\s]*)\s*([kKmM])?`)

// amount reads the first amount in text: "$120,000", "120k", "1.2M".
func amount(s string) *int64 {
	m := amountPattern.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	digits := strings.NewReplacer(",", "", " ", "").Replace(strings.TrimSpace(m[1]))
	f, err := strconv.ParseFloat(strings.TrimRight(digits, "."), 64)
	if err != nil {
		return nil
	}
	switch strings.ToLower(m[2]) {
	case "k":
		f *= 1_000
	case "m":
		f *= 1_000_000
	}
	return number(f)
}
