package posting

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
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
		"description":     {Type: extraction.Text, Instructions: descriptionInstructions},
	}}
}

// descriptionInstructions asks for the posting's own text, cleaned of the
// page around it, not a summary: it is what matching reads.
const descriptionInstructions = "The full job description exactly as the posting words it, as markdown: " +
	"the role overview, responsibilities, qualifications, compensation, benefits and the employer's statements. " +
	"Keep every sentence, heading and list item of the description itself, word for word; do not summarize or rephrase. " +
	"Leave out everything that is not part of the description: navigation, breadcrumbs, back links, apply or submit buttons, " +
	"share and save icons, images, cookie banners, related or similar jobs, search links and site footers."

// minDescription is the shortest cleaned description kept: anything shorter
// is more likely a summary or a fragment than the posting, so the page's
// text stays instead.
const minDescription = 200

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
	// num reads a salary bound: typed models answer a number; the rest answer
	// text, which is parsed. A text answer holding a whole range ("$150,400 -
	// $277,600") gives its lowest amount for a minimum and its highest for a
	// maximum.
	num := func(name string, highest bool) *int64 {
		a, ok := answers[name]
		if !ok || (a.Confidence != nil && *a.Confidence < minConfidence) {
			return nil
		}
		if a.Type == extraction.Number {
			var f float64
			if json.Unmarshal(a.Value, &f) != nil {
				return nil
			}
			return positive(number(f))
		}
		return amount(str(name), highest)
	}
	p := Posting{
		Title:          limit(str("title"), 300),
		Company:        limit(str("company"), 300),
		Location:       limit(str("location"), 300),
		WorkMode:       workMode(str("work_mode")),
		EmploymentType: employmentWords(str("employment_type")),
		SalaryMin:      num("salary_min", false),
		SalaryMax:      num("salary_max", true),
		SalaryCurrency: currency(str("salary_currency")),
		SalaryPeriod:   period(str("salary_period")),
		Description:    description(answers, r.State),
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

// description keeps the model's cleaned description when it is plausibly the
// posting's own text: long enough, and not longer than what it was read from.
func description(answers map[string]extraction.Answer, state string) string {
	a, ok := answers["description"]
	if !ok || a.Type != extraction.Text || (a.Confidence != nil && *a.Confidence < minConfidence) {
		return ""
	}
	var s string
	_ = json.Unmarshal(a.Value, &s)
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < minDescription || n > utf8.RuneCountInString(state) || n > maxDescription {
		return ""
	}
	return s
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
var currencySymbols = map[string]string{"€": "EUR", "£": "GBP", "¥": "JPY", "₹": "INR", "US$": "USD", "C$": "CAD", "CA$": "CAD", "A$": "AUD", "AU$": "AUD", "MX$": "MXN", "MXN$": "MXN"}

// currencyCodes are the ISO 4217 codes a job posting is likely to use. A
// three-letter word is only a currency when it is one of these: "PER" and
// "THE" are not.
var currencyCodes = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(`USD EUR GBP MXN CAD AUD NZD CHF JPY CNY INR BRL ARS CLP COP PEN UYU
		SEK NOK DKK PLN CZK HUF RON BGN TRY ILS AED SAR QAR ZAR EGP NGN KES SGD HKD KRW TWD THB VND PHP IDR MYR UAH`) {
		m[c] = true
	}
	return m
}()

// currencyNames maps how postings spell currencies out, most specific first.
var currencyNames = [][2]string{
	{"canadian dollar", "CAD"}, {"australian dollar", "AUD"}, {"new zealand dollar", "NZD"},
	{"us dollar", "USD"}, {"u.s. dollar", "USD"}, {"american dollar", "USD"}, {"united states dollar", "USD"},
	{"mexican peso", "MXN"}, {"pesos mexicanos", "MXN"}, {"peso mexicano", "MXN"},
	{"euro", "EUR"}, {"pound", "GBP"}, {"sterling", "GBP"},
}

var isoCode = regexp.MustCompile(`\b[A-Z]{3}\b`)

func currency(s string) string {
	s = strings.TrimSpace(s)
	if c, ok := currencySymbols[s]; ok {
		return c
	}
	for _, m := range isoCode.FindAllString(strings.ToUpper(s), -1) {
		if currencyCodes[m] {
			return m
		}
	}
	lower := strings.ToLower(s)
	for _, n := range currencyNames {
		if strings.Contains(lower, n[0]) {
			return n[1]
		}
	}
	return ""
}

// moneyToken is one amount in running text: thousands grouped by a comma,
// dot, space or apostrophe ("150,400", "150.400", "85 000", "1'200"), or plain
// digits with an optional decimal part ("45.5", "1,5"), then an optional scale
// word ("120k", "1.2M", "30 mil", "2 million"). The scale must end at a word
// boundary so "30,000 monthly" does not read as millions.
var moneyToken = regexp.MustCompile(`(?i)(\d{1,3}(?:[ ,.\x{00a0}\x{202f}'’]\d{3})+(?:[.,]\d{1,2})?|\d+(?:[.,]\d+)?)(?:(k|mm|m)\b|\s*(mil|millones|millon|millions?|thousand)\b)?`)

// notMoney marks numbers followed by a unit that is not a currency: "3+
// years", "40 hours", "10%".
var notMoney = regexp.MustCompile(`(?i)^\s*(?:\+|%|(?:years?|yrs?|months?|weeks?|days?|hours?)\b)`)

// rangeJoin is what may sit between the two ends of a range.
var rangeJoin = regexp.MustCompile(`(?i)^[\s$€£¥₹~]*(?:-|–|—|to|and|a|y)?[\s$€£¥₹~]*$`)

// amountsIn reads every salary-like amount in text, in order. Both ends of a
// range share a scale written once: "100-120k" is 100,000 to 120,000.
func amountsIn(text string) []float64 {
	type found struct {
		value float64
		scale float64
		start int
		end   int
	}
	var all []found
	for _, m := range moneyToken.FindAllStringSubmatchIndex(text, -1) {
		if !standsAlone(text[:m[0]]) {
			continue // part of a code: ID98062, Q4
		}
		if notMoney.MatchString(text[m[1]:]) {
			continue
		}
		scale := 1.0
		if m[4] >= 0 || m[6] >= 0 {
			word := strings.ToLower(text[max(m[4], m[6]):max(m[5], m[7])])
			switch word {
			case "k", "mil", "thousand":
				scale = 1_000
			default:
				scale = 1_000_000
			}
		}
		v, ok := parseDigits(text[m[2]:m[3]], scale > 1)
		if !ok {
			continue
		}
		all = append(all, found{value: v, scale: scale, start: m[0], end: m[1]})
	}
	for i := 0; i+1 < len(all); i++ {
		a, b := &all[i], all[i+1]
		if a.scale == 1 && b.scale > 1 && a.value <= b.value && rangeJoin.MatchString(text[a.end:b.start]) {
			a.scale = b.scale
		}
	}
	out := make([]float64, 0, len(all))
	for _, f := range all {
		out = append(out, f.value*f.scale)
	}
	return out
}

// standsAlone reports whether an amount starting after before is not the tail
// of a word: "USD150" is an amount, "ID98062" and "Q4" are not.
func standsAlone(before string) bool {
	end := len(before)
	start := end
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(before[:start])
		if !unicode.IsLetter(r) {
			break
		}
		start -= size
	}
	return start == end || currencyCodes[strings.ToUpper(before[start:end])]
}

// parseDigits reads digits with grouping and a decimal mark in either
// convention. A single separator followed by exactly three digits is a
// thousands mark ("1.200" is 1200) unless a scale follows ("1.500M").
func parseDigits(tok string, scaled bool) (float64, bool) {
	tok = strings.NewReplacer(" ", "", "\u00a0", "", "\u202f", "", "'", "", "’", "").Replace(tok)
	dot, comma := strings.Count(tok, "."), strings.Count(tok, ",")
	switch {
	case dot > 0 && comma > 0:
		// The mark that comes last is the decimal one.
		if strings.LastIndex(tok, ".") > strings.LastIndex(tok, ",") {
			tok = strings.ReplaceAll(tok, ",", "")
		} else {
			tok = strings.ReplaceAll(strings.ReplaceAll(tok, ".", ""), ",", ".")
		}
	case dot+comma > 1:
		tok = strings.NewReplacer(".", "", ",", "").Replace(tok)
	case dot+comma == 1:
		i := strings.IndexAny(tok, ".,")
		if len(tok)-i-1 == 3 && i > 0 && tok[0] != '0' && !scaled {
			tok = tok[:i] + tok[i+1:]
		} else {
			tok = strings.Replace(tok, ",", ".", 1)
		}
	}
	f, err := strconv.ParseFloat(tok, 64)
	return f, err == nil
}

// amount reads a salary bound from an extraction model's text answer: the
// lowest amount it states, or the highest. Nothing when it states none.
func amount(s string, highest bool) *int64 {
	var pick *float64
	for _, v := range amountsIn(s) {
		v := v
		if pick == nil || (highest && v > *pick) || (!highest && v < *pick) {
			pick = &v
		}
	}
	if pick == nil {
		return nil
	}
	return positive(number(*pick))
}

// positive drops a zero amount: no posting offers a salary of 0, a model
// answering 0 means it found none.
func positive(n *int64) *int64 {
	if n == nil || *n <= 0 {
		return nil
	}
	return n
}
