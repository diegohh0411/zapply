package posting

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/diegohh0411/zapply/internal/model/extraction"
)

func TestExtractionRequestFitsTheModel(t *testing.T) {
	typed := ExtractionRequest("cloudflare/clairic", "posting")
	if typed.Fields["salary_min"].Type != extraction.Number || extraction.Validate(typed) != nil {
		t.Errorf("clairic: %+v", typed.Fields["salary_min"])
	}
	plain := ExtractionRequest("scaledown/extract", "posting")
	if plain.Fields["salary_min"].Type != extraction.Text || extraction.Validate(plain) != nil {
		t.Errorf("extract: %+v", plain.Fields["salary_min"])
	}
}

func TestFromExtraction(t *testing.T) {
	typed := ExtractionRequest("cloudflare/clairic", "posting")
	p := FromExtraction(typed, `{
	 "title":{"type":"text","value":"Software Engineer","confidence":0.98},
	 "company":{"type":"text","value":"Apple","confidence":0.2},
	 "work_mode":{"type":"text","value":"Hybrid (3 days in office)","confidence":0.9},
	 "employment_type":{"type":"text","value":"Full-time"},
	 "salary_min":{"type":"number","value":181100,"confidence":0.97},
	 "salary_max":{"type":"number","value":318400,"confidence":0.97},
	 "salary_currency":{"type":"text","value":"USD"},
	 "salary_period":{"type":"text","value":"per year"},
	 "location":{"type":"text","value":null,"guess":"Cupertino"},
	 "unasked":{"type":"text","value":"x"}}`)
	if p.Title != "Software Engineer" || p.Company != "" || p.WorkMode != "hybrid" || p.EmploymentType != "full_time" || p.Location != "" {
		t.Errorf("text fields: %+v", p)
	}
	if p.SalaryMin == nil || *p.SalaryMin != 181100 || *p.SalaryMax != 318400 || p.SalaryCurrency != "USD" || p.SalaryPeriod != "year" {
		t.Errorf("salary: %+v", p)
	}

	plain := ExtractionRequest("scaledown/extract", "posting")
	p = FromExtraction(plain, `{"salary_min":{"type":"text","value":"$120k"},"salary_max":{"type":"text","value":"150,000"},"salary_currency":{"type":"text","value":"$"},"salary_period":{"type":"text","value":"/hr"}}`)
	if p.SalaryMin == nil || *p.SalaryMin != 120000 || *p.SalaryMax != 150000 || p.SalaryCurrency != "" || p.SalaryPeriod != "hour" {
		t.Errorf("text salary: %+v", p)
	}
	if p := FromExtraction(plain, `not json`); p.Title != "" || p.SalaryMin != nil {
		t.Errorf("garbage: %+v", p)
	}
}

func TestFromExtractionDescription(t *testing.T) {
	body := strings.Repeat("Build models that run on device. ", 10)
	page := "[Back to search results](x)\n\n[Submit Resume](y)\n\n" + body + "\n\n[Submit Resume](y)"
	r := ExtractionRequest("cloudflare/clairic", page)
	answer := func(v string) string {
		b, _ := json.Marshal(v)
		return `{"description":{"type":"text","value":` + string(b) + `}}`
	}
	if p := FromExtraction(r, answer(body)); p.Description != strings.TrimSpace(body) {
		t.Errorf("cleaned description dropped: %q", p.Description)
	}
	if p := FromExtraction(r, answer("A summary.")); p.Description != "" {
		t.Errorf("short description kept: %q", p.Description)
	}
	if p := FromExtraction(r, answer(page+page)); p.Description != "" {
		t.Errorf("description longer than the page kept")
	}
}

func TestAmountReadsWhatTextModelsWrite(t *testing.T) {
	for _, c := range []struct {
		in     string
		lo, hi int64 // 0: none
	}{
		{"$150,400", 150400, 150400},
		{"150,400 USD", 150400, 150400},
		{"USD150,400", 150400, 150400},
		{"$150,400 - $277,600", 150400, 277600},
		{"USD 150,400 and 277,600", 150400, 277600},
		{"$120K–$214.3K", 120000, 214300},
		{"100-120k", 100000, 120000},
		{"£50k", 50000, 50000},
		{"1.2M", 1200000, 1200000},
		{"85 000", 85000, 85000},
		{"$85k per year", 85000, 85000},
		{"$150.400,00", 150400, 150400},
		{"€45.000", 45000, 45000},
		{"1.200", 1200, 1200},
		{"45,5 €", 46, 46},
		{"$45.50/hr", 46, 46},
		{"30 mil pesos", 30000, 30000},
		{"2 millones", 2000000, 2000000},
		{"MXN 30,000 monthly", 30000, 30000},
		{"$3,000 a month", 3000, 3000},
		{"1.500M", 1500000, 1500000},
		{"3+ years, $100k", 100000, 100000},
		{"ID98062 pays $90,000", 90000, 90000},
		{"40 hours, 10% bonus", 0, 0},
		{"Not specified", 0, 0},
		{"$0", 0, 0},
		{"9999999999999999999", 0, 0},
	} {
		check := func(highest bool, want int64) {
			got := amount(c.in, highest)
			if (want == 0) != (got == nil) || (got != nil && *got != want) {
				t.Errorf("amount(%q, highest=%v) = %v, want %d", c.in, highest, got, want)
			}
		}
		check(false, c.lo)
		check(true, c.hi)
	}
}

func TestCurrencyNeedsARealCode(t *testing.T) {
	for in, want := range map[string]string{
		"USD": "USD", "usd": "USD", "per year": "", "in MXN": "MXN", "the USD": "USD", "$": "",
		"€": "EUR", "US$": "USD", "Canadian dollars": "CAD", "euros": "EUR", "dollars": "",
	} {
		if got := currency(in); got != want {
			t.Errorf("currency(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromExtractionTextRange(t *testing.T) {
	plain := ExtractionRequest("scaledown/extract", "posting")
	p := FromExtraction(plain, `{"salary_min":{"type":"text","value":"$150,400 - $277,600"},"salary_max":{"type":"text","value":"$150,400 - $277,600"},"salary_currency":{"type":"text","value":"US dollars"},"salary_period":{"type":"text","value":"annual"}}`)
	if p.SalaryMin == nil || *p.SalaryMin != 150400 || *p.SalaryMax != 277600 || p.SalaryCurrency != "USD" || p.SalaryPeriod != "year" {
		t.Errorf("range in one answer: %+v", p)
	}
	typed := ExtractionRequest("cloudflare/clairic", "posting")
	if p := FromExtraction(typed, `{"salary_min":{"type":"number","value":0},"salary_max":{"type":"number","value":0}}`); p.SalaryMin != nil {
		t.Errorf("zero salary kept: %+v", p)
	}
}

// linkedInMarkdown is a LinkedIn guest posting as the scraper reads it.
const linkedInMarkdown = `## [Software Quality Engineer](https://mx.linkedin.com/jobs/view/4493416980?trk=public_jobs_topcard-title)

#### [Tenarai - LATAM](https://uy.linkedin.com/company/tenarai-latam) Mexico City, Mexico 2 hours ago Be among the first 25 applicants

Apply Save

## Join or sign in to save this job

Email or phone

Password

[Forgot password?](https://www.linkedin.com/uas/request-password-reset) Sign in

**About NCR Atleos** NCR Atleos is a global leader in ATM operations and software solutions, delivering innovative financial technology to retail and financial institution clients worldwide.

**What You’ll Do**

- **Test Design & Execution:** Design, develop, and execute comprehensive test plans and test cases for ATM software applications.

- **Core Qualifications:**
- Bachelor’s degree in Computer Science, Engineering, or equivalent practical experience.
- Ability to pass all applicable background screening tests.

Show more Show less

- ### Seniority level
  
  Mid-Senior level

Referrals increase your chances of interviewing at Tenarai - LATAM by 2x`

func TestFromExtractionDescriptionBetweenAnchors(t *testing.T) {
	r := ExtractionRequest("scaledown/extract", linkedInMarkdown)
	p := FromExtraction(r, `{
	 "description_start":{"type":"text","value":"NCR Atleos is a global leader in ATM operations and software solutions"},
	 "description_end":{"type":"text","value":"Ability to pass all applicable background screening tests."},
	 "description":{"type":"text","value":"`+strings.Repeat("whole page ", 30)+`"}}`)
	if !strings.HasPrefix(p.Description, "**About NCR Atleos** NCR Atleos") {
		t.Errorf("start: %q", p.Description)
	}
	if !strings.HasSuffix(p.Description, "- Ability to pass all applicable background screening tests.") {
		t.Errorf("end: %q", p.Description)
	}
	for _, junk := range []string{"Sign in", "Show more", "Seniority", "Referrals"} {
		if strings.Contains(p.Description, junk) {
			t.Errorf("kept %q: %q", junk, p.Description)
		}
	}

	// Anchors the page does not hold fall back to the model's copy.
	p = FromExtraction(r, `{
	 "description_start":{"type":"text","value":"A sentence that is nowhere in the page"},
	 "description_end":{"type":"text","value":"Ability to pass all applicable background screening tests."},
	 "description":{"type":"text","value":"`+strings.Repeat("cleaned text ", 30)+`"}}`)
	if !strings.HasPrefix(p.Description, "cleaned text") {
		t.Errorf("fallback: %q", p.Description)
	}
}

func TestBetweenIgnoresMarkdownAndCase(t *testing.T) {
	page := "Header\n\n## About **the** role\nWe build [ATMs](https://x.example) daily.\n\nApply now\nFooter"
	got, problems := between(page, "About the role", "we build ATMs daily")
	if got != "## About **the** role\nWe build [ATMs](https://x.example) daily." || problems != nil {
		t.Errorf("between = %q, %v", got, problems)
	}
	if got, problems := between(page, "About", "ATMs"); got != "" || len(problems) != 2 {
		t.Errorf("short anchors matched: %q, %v", got, problems)
	}
}

// The markers must each be one sentence the page holds exactly once, in order.
func TestBetweenNeedsUniqueOrderedMarkers(t *testing.T) {
	page := "Intro\nWe build the platform for lenders across Latin America.\nWe build the platform for lenders across Latin America.\n" +
		"Build models that run on device every day.\nAbout you: five years of Go experience.\nSite footer text."
	cases := []struct {
		name, first, last string
		want              string // a problem containing this
	}{
		{"first repeated", "We build the platform for lenders across Latin America", "Five years of Go experience", "appears 2 times"},
		{"last missing", "Build models that run on device every day", "Something the page never says at all", "is not in the page"},
		{"first too short", "Intro", "Five years of Go experience", "has 5 letters or digits"},
		{"last before first", "About you: five years of Go experience", "Build models that run on device every day", "comes before"},
	}
	for _, c := range cases {
		got, problems := between(page, c.first, c.last)
		if got != "" || len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), c.want) {
			t.Errorf("%s: between = %q, %v; want a problem containing %q", c.name, got, problems, c.want)
		}
	}
	got, problems := between(page, "Build models that run on device every day", "five years of Go experience")
	if problems != nil || got != "Build models that run on device every day.\nAbout you: five years of Go experience." {
		t.Errorf("unique markers: %q, %v", got, problems)
	}
}

func TestCheckReportsMarkersForRetry(t *testing.T) {
	page := "Apply now\n\nBuild models that run on device every day.\nWe ship a fast API.\n\nApply now\n\nSimilar jobs: Build models that run on device every day."
	r := ExtractionRequest("scaledown/extract", page)
	answers := func(first, last string) string {
		a, _ := json.Marshal(first)
		z, _ := json.Marshal(last)
		return `{"description_start":{"type":"text","value":` + string(a) + `},"description_end":{"type":"text","value":` + string(z) + `}}`
	}
	if p := Check(r, answers("Build models that run on device every day", "We ship a fast API.")); len(p) == 0 || !strings.Contains(p[0], "appears 2 times") {
		t.Errorf("repeated marker not reported: %v", p)
	}
	if p := Check(r, answers("Apply now", "We ship a fast API.")); len(p) == 0 {
		t.Errorf("short marker not reported: %v", p)
	}
	if p := Check(r, `{"description_start":{"type":"text","value":null},"description_end":{"type":"text","value":"x"}}`); p != nil {
		t.Errorf("missing markers retried: %v", p)
	}
	if p := Check(r, `{"unrelated":{"type":"text","value":"x"}}`); p != nil {
		t.Errorf("no markers retried: %v", p)
	}
	if p := Check(r, `not json`); p != nil {
		t.Errorf("garbage retried: %v", p)
	}
}

func TestExtractionRequestCarriesRejections(t *testing.T) {
	plain := ExtractionRequest("scaledown/extract", "posting")
	fed := ExtractionRequest("scaledown/extract", "posting", `description_end "AWS" has 3 letters or digits`)
	for _, name := range []string{"description_start", "description_end"} {
		if plain.Fields[name].Instructions == fed.Fields[name].Instructions {
			t.Errorf("%s: rejection not carried", name)
		}
	}
	if !strings.Contains(fed.Fields["description_end"].Instructions, `has 3 letters or digits`) {
		t.Errorf("rejection text lost: %q", fed.Fields["description_end"].Instructions)
	}
	if plain.Fields["title"] != fed.Fields["title"] {
		t.Error("rejection leaked into other fields")
	}
}
