package posting

import (
	"strings"
	"testing"

	"github.com/diegohh0411/zapply/internal/model/extraction"
)

func answers(t *testing.T, raw string) map[string]extraction.Answer {
	t.Helper()
	a, err := extraction.Parse(ExtractionRequest("m", "page"), []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestExtractionRequest(t *testing.T) {
	r := ExtractionRequest("cloudflare/clairic", "  "+strings.Repeat("é", maxState)+"  ")
	if err := extraction.Validate(r); err != nil {
		t.Fatal(err)
	}
	if len(r.State) > maxState || len(r.Fields) != 9 {
		t.Errorf("state %d bytes, %d fields", len(r.State), len(r.Fields))
	}
}

func TestFromAnswers(t *testing.T) {
	x := FromAnswers(answers(t, `{
	 "title":{"type":"text","value":"Senior  Product\nDesigner","confidence":0.97,"p_null":0.01},
	 "company":{"type":"text","value":"Linear","confidence":0.99},
	 "location":{"type":"text","value":"Mexico City, Mexico","confidence":0.9},
	 "work_mode":{"type":"text","value":"Hybrid (3 days in office)","confidence":0.8},
	 "employment_type":{"type":"text","value":"Full-Time","confidence":0.95},
	 "salary_min":{"type":"number","value":120000.4,"confidence":0.9},
	 "salary_max":{"type":"number","value":150000,"confidence":0.7},
	 "salary_currency":{"type":"text","value":"USD","confidence":0.9},
	 "salary_period":{"type":"text","value":"per year","confidence":0.9}}`))
	want := Posting{Title: "Senior Product Designer", Company: "Linear", Location: "Mexico City, Mexico", WorkMode: "hybrid",
		EmploymentType: "full_time", SalaryCurrency: "USD", SalaryPeriod: "year"}
	got := x.Posting
	got.SalaryMin, got.SalaryMax = nil, nil
	if got != want {
		t.Errorf("posting = %+v", got)
	}
	if *x.SalaryMin != 120000 || *x.SalaryMax != 150000 || x.Certainty["salary_min"] != 0.7 {
		t.Errorf("salary = %d-%d at %v", *x.SalaryMin, *x.SalaryMax, x.Certainty["salary_min"])
	}
	if x.Certainty["title"] < 0.96 || len(x.Certainty) != 9 {
		t.Errorf("certainty = %v", x.Certainty)
	}
}

func TestFromAnswersDropsWhatItCannotNormalize(t *testing.T) {
	x := FromAnswers(answers(t, `{
	 "work_mode":{"type":"text","value":"Flexible","confidence":0.9},
	 "employment_type":{"type":"text","value":"Great team","confidence":0.9},
	 "salary_min":{"type":"number","value":200000},
	 "salary_max":{"type":"number","value":100000},
	 "salary_currency":{"type":"text","value":"$"},
	 "salary_period":{"type":"text","value":"per week"}}`))
	if x.WorkMode != "" || x.EmploymentType != "" || x.SalaryMin != nil || x.SalaryCurrency != "" || x.SalaryPeriod != "" || len(x.Certainty) != 0 {
		t.Errorf("guessed: %+v", x)
	}
	one := FromAnswers(answers(t, `{"salary_max":{"type":"number","value":45,"confidence":0.8}}`))
	if *one.SalaryMin != 45 || *one.SalaryMax != 45 {
		t.Errorf("a single figure fills both ends: %+v", one)
	}
}

func TestNormalizers(t *testing.T) {
	for in, want := range map[string]string{"Remote - US": "remote", "On-site": "onsite", "Work from home": "remote", "Office": ""} {
		if got := workMode(in); got != want {
			t.Errorf("workMode(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{"Part time": "part_time", "Internship": "internship", "Contractor": "contract", "Permanent": "full_time", "Fixed-term temporary": "temporary"} {
		if got := employment(in); got != want {
			t.Errorf("employment(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{"annually": "year", "/hr": "hour", "a month": "month", "p.a.": "year"} {
		if got := period(in); got != want {
			t.Errorf("period(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{"€": "EUR", "mxn": "MXN", "US$": "USD", "$": ""} {
		if got := currency(in); got != want {
			t.Errorf("currency(%q) = %q", in, got)
		}
	}
}
