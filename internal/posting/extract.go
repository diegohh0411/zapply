package posting

import (
	"math"
	"regexp"
	"strings"

	"github.com/diegohh0411/zapply/internal/model/extraction"
)

// maxState bounds the page text sent for extraction, about 15k tokens: a
// posting's details sit well within it, and the reservation scales with it.
const maxState = 60_000

// fields are what the extraction model reads from a posting's text. Text
// answers are spans of the page, so enum fields ask for the words that state
// them and normalize those here.
var fields = map[string]extraction.Field{
	"title":           {Type: extraction.Text, Instructions: "The title of the job being advertised, without the company name or location."},
	"company":         {Type: extraction.Text, Instructions: "The name of the company that is hiring for this job."},
	"location":        {Type: extraction.Text, Instructions: "Where the job is based, as written: city, region and country. Not a work arrangement such as Remote."},
	"work_mode":       {Type: extraction.Text, Instructions: "The words stating where the work is done: remote, hybrid, or on-site in an office."},
	"employment_type": {Type: extraction.Text, Instructions: "The words stating the employment type, such as full-time, part-time, contract, temporary, internship or freelance."},
	"salary_min":      {Type: extraction.Number, Integer: true, Instructions: "The lowest pay in the advertised salary or pay range, or the only figure when one is given."},
	"salary_max":      {Type: extraction.Number, Integer: true, Instructions: "The highest pay in the advertised salary or pay range, or the only figure when one is given."},
	"salary_currency": {Type: extraction.Text, Instructions: "The currency of the advertised pay: its ISO code or symbol, such as USD, EUR, MXN, $ or €."},
	"salary_period":   {Type: extraction.Text, Instructions: "The words stating how often the advertised pay is earned, such as per year, annually, per month or per hour."},
}

// ExtractionRequest asks model for a posting's fields from its page text.
func ExtractionRequest(model, page string) extraction.Request {
	return extraction.Request{Model: model, State: truncate(strings.TrimSpace(page), maxState), Fields: fields}
}

// Extracted is what an extraction model read, with its certainty per field
// (0 to 1), keyed by the job API's field names.
type Extracted struct {
	Posting
	Certainty map[string]float64
}

// FromAnswers maps extraction answers to job fields. Answers that do not
// normalize to a job value are dropped rather than guessed.
func FromAnswers(answers map[string]extraction.Answer) Extracted {
	out := Extracted{Certainty: map[string]float64{}}
	set := func(name string, value *string, v string) {
		if v == "" {
			return
		}
		*value = v
		out.Certainty[name] = answers[name].Certainty()
	}
	text := func(name string) string { return clean(answers[name].Text()) }
	set("title", &out.Title, text("title"))
	set("company", &out.Company, text("company"))
	set("location", &out.Location, text("location"))
	set("work_mode", &out.WorkMode, workMode(text("work_mode")))
	set("employment_type", &out.EmploymentType, employment(text("employment_type")))
	set("salary_period", &out.SalaryPeriod, period(text("salary_period")))
	set("salary_currency", &out.SalaryCurrency, currency(text("salary_currency")))

	lo, hasLo := amount(answers, "salary_min")
	hi, hasHi := amount(answers, "salary_max")
	if hasLo && !hasHi {
		hi = lo
	}
	if hasHi && !hasLo {
		lo = hi
	}
	if (hasLo || hasHi) && *lo <= *hi {
		out.SalaryMin, out.SalaryMax = lo, hi
		c := min(certainty(answers, "salary_min", hasLo), certainty(answers, "salary_max", hasHi))
		out.Certainty["salary_min"], out.Certainty["salary_max"] = c, c
	}
	return out
}

func certainty(answers map[string]extraction.Answer, name string, ok bool) float64 {
	if !ok {
		return 1
	}
	return answers[name].Certainty()
}

func amount(answers map[string]extraction.Answer, name string) (*int64, bool) {
	a, ok := answers[name]
	if !ok {
		return nil, false
	}
	f := a.Number()
	if f <= 0 || f > math.MaxInt64/2 {
		return nil, false
	}
	n := int64(math.Round(f))
	return &n, true
}

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }

var nonLetters = regexp.MustCompile(`[^a-z]+`)

// words lowercases s and joins its letters with single spaces.
func words(s string) string {
	return strings.TrimSpace(nonLetters.ReplaceAllString(strings.ToLower(s), " "))
}

func workMode(s string) string {
	w := " " + words(s) + " "
	switch {
	case strings.Contains(w, " hybrid "):
		return "hybrid"
	case strings.Contains(w, " remote ") || strings.Contains(w, " work from home ") || strings.Contains(w, " wfh ") || strings.Contains(w, " telecommute"):
		return "remote"
	case strings.Contains(w, " on site ") || strings.Contains(w, " onsite ") || strings.Contains(w, " in office ") || strings.Contains(w, " office based "):
		return "onsite"
	}
	return ""
}

func employment(s string) string {
	w := strings.ReplaceAll(words(s), " ", "")
	switch {
	case strings.Contains(w, "fulltime") || w == "permanent":
		return "full_time"
	case strings.Contains(w, "parttime"):
		return "part_time"
	case strings.Contains(w, "intern"):
		return "internship"
	case strings.Contains(w, "freelance"):
		return "freelance"
	case strings.Contains(w, "contract"):
		return "contract"
	case strings.Contains(w, "temporary") || strings.Contains(w, "temp") || strings.Contains(w, "seasonal"):
		return "temporary"
	}
	return ""
}

func period(s string) string {
	w := " " + words(s) + " "
	switch {
	case strings.Contains(w, " year") || strings.Contains(w, " annual") || strings.Contains(w, " yr ") || strings.Contains(w, " pa ") || strings.Contains(w, " p a "):
		return "year"
	case strings.Contains(w, " month") || strings.Contains(w, " mo "):
		return "month"
	case strings.Contains(w, " hour") || strings.Contains(w, " hr "):
		return "hour"
	}
	return ""
}

var isoCurrency = regexp.MustCompile(`\b([A-Z]{3})\b`)

// symbols maps unambiguous currency symbols. A bare $ is left out: it names
// a dozen currencies.
var symbols = map[string]string{"€": "EUR", "£": "GBP", "¥": "JPY", "₹": "INR", "US$": "USD", "C$": "CAD", "A$": "AUD", "MX$": "MXN", "R$": "BRL"}

func currency(s string) string {
	if m := isoCurrency.FindStringSubmatch(strings.ToUpper(s)); m != nil && m[1] != "PER" {
		return m[1]
	}
	for sym, code := range symbols {
		if strings.Contains(s, sym) {
			return code
		}
	}
	return ""
}
