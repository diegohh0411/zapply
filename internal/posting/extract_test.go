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
