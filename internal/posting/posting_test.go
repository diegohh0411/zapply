package posting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const linkedInPage = `<html><head>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"BreadcrumbList"}</script>
<script type="application/ld+json">
{"@context":"http://schema.org","@type":"JobPosting","title":"Senior Product Designer",
 "hiringOrganization":{"@type":"Organization","name":"Linear &amp; Co"},
 "employmentType":["FULL_TIME"],
 "jobLocation":{"@type":"Place","address":{"@type":"PostalAddress","addressLocality":"Mexico City","addressRegion":"CDMX","addressCountry":{"@type":"Country","name":"MX"}}},
 "baseSalary":{"@type":"MonetaryAmount","currency":"usd","value":{"@type":"QuantitativeValue","minValue":"120,000","maxValue":150000,"unitText":"YEAR"}}}
</script></head><body>…</body></html>`

func fakeFirecrawl(t *testing.T, status int, body any, check func(*http.Request, scrapeRequest)) *Reader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req scrapeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if r.URL.Path != "/v2/scrape" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if check != nil {
			check(r, req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "secret")
}

func TestReadJobPosting(t *testing.T) {
	r := fakeFirecrawl(t, 200, map[string]any{
		"success": true,
		"data": map[string]any{
			"markdown": "  # Senior Product Designer\n\nWe build Linear.  ",
			"rawHtml":  linkedInPage,
			"metadata": map[string]any{"statusCode": 200, "ogTitle": "Linear hiring Senior Product Designer | LinkedIn"},
		},
	}, func(r *http.Request, req scrapeRequest) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if req.URL != "https://www.linkedin.com/jobs/view/123" {
			t.Errorf("url = %q, want the normalized URL", req.URL)
		}
		if strings.Join(req.Formats, ",") != "markdown,rawHtml" {
			t.Errorf("formats = %v", req.Formats)
		}
	})
	p, err := r.Read(context.Background(), " https://www.linkedin.com/jobs/view/123/?utm_source=x ")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Senior Product Designer" || p.Company != "Linear & Co" || p.Location != "Mexico City, CDMX, MX" {
		t.Errorf("title/company/location = %q / %q / %q", p.Title, p.Company, p.Location)
	}
	if p.EmploymentType != "full_time" || p.WorkMode != "" {
		t.Errorf("employment/work mode = %q / %q", p.EmploymentType, p.WorkMode)
	}
	if p.SalaryMin == nil || *p.SalaryMin != 120000 || p.SalaryMax == nil || *p.SalaryMax != 150000 || p.SalaryCurrency != "USD" || p.SalaryPeriod != "year" {
		t.Errorf("salary = %v-%v %q %q", p.SalaryMin, p.SalaryMax, p.SalaryCurrency, p.SalaryPeriod)
	}
	if p.Description != "# Senior Product Designer\n\nWe build Linear." {
		t.Errorf("description = %q", p.Description)
	}
}

func TestReadFallsBackToPageTitle(t *testing.T) {
	r := fakeFirecrawl(t, 200, map[string]any{
		"success": true,
		"data":    map[string]any{"markdown": "Hello", "rawHtml": "<p>no data</p>", "metadata": map[string]any{"ogTitle": []any{"Staff Engineer"}}},
	}, nil)
	p, err := r.Read(context.Background(), "https://jobs.example.com/1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Staff Engineer" || p.Company != "" || p.SalaryMin != nil {
		t.Errorf("posting = %+v", p)
	}
}

func TestReadErrors(t *testing.T) {
	var nilReader *Reader
	if _, err := nilReader.Read(context.Background(), "https://example.com"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("unconfigured: %v", err)
	}
	ok := fakeFirecrawl(t, 200, map[string]any{"success": true}, nil)
	if _, err := ok.Read(context.Background(), "ftp://example.com/x"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("ftp: %v", err)
	}
	cases := map[string]*Reader{
		"failed":   fakeFirecrawl(t, 400, map[string]any{"success": false, "error": "URL is a private address"}, nil),
		"blocked":  fakeFirecrawl(t, 200, map[string]any{"success": true, "data": map[string]any{"blocked": true}}, nil),
		"site 404": fakeFirecrawl(t, 200, map[string]any{"success": true, "data": map[string]any{"metadata": map[string]any{"statusCode": 404}}}, nil),
		"timeout":  fakeFirecrawl(t, 408, map[string]any{"success": false}, nil),
	}
	for name, r := range cases {
		var unreadable *UnreadableError
		if _, err := r.Read(context.Background(), "https://example.com/job"); !errors.As(err, &unreadable) || unreadable.Reason == "" {
			t.Errorf("%s: %v", name, err)
		}
	}
	down := fakeFirecrawl(t, 502, map[string]any{}, nil)
	if _, err := down.Read(context.Background(), "https://example.com/job"); err == nil || errors.As(err, new(*UnreadableError)) {
		t.Errorf("scraper down should be an internal error: %v", err)
	}
}

func TestJobPostingShapes(t *testing.T) {
	page := `<script type="application/ld+json">{"@graph":[{"@type":"WebPage"},{"@type":["Thing","JobPosting"],
	  "title":"  Backend   Engineer ","hiringOrganization":"Acme","jobLocationType":"TELECOMMUTE",
	  "employmentType":"CONTRACTOR","jobLocation":[{"address":"Remote, Earth"}],
	  "baseSalary":{"currency":"EUR","value":{"value":40,"unitText":"HOUR"}}}]}</script>`
	p := fromJobPosting(findJobPosting(page))
	if p.Title != "Backend Engineer" || p.Company != "Acme" || p.WorkMode != "remote" || p.EmploymentType != "contract" || p.Location != "Remote, Earth" {
		t.Errorf("posting = %+v", p)
	}
	if *p.SalaryMin != 40 || *p.SalaryMax != 40 || p.SalaryPeriod != "hour" || p.SalaryCurrency != "EUR" {
		t.Errorf("salary = %v-%v %s %s", *p.SalaryMin, *p.SalaryMax, p.SalaryCurrency, p.SalaryPeriod)
	}

	weekly := `<script type="application/ld+json">{"@type":"JobPosting","baseSalary":{"currency":"USD","value":{"minValue":900,"unitText":"WEEK"}}}</script>`
	if p := fromJobPosting(findJobPosting(weekly)); p.SalaryMin != nil {
		t.Errorf("a weekly salary has no job period and should be dropped: %+v", p)
	}
	if findJobPosting(`<script type="application/ld+json">{not json</script><p>x</p>`) != nil {
		t.Error("malformed JSON-LD should be skipped")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("añb", 2); got != "a" {
		t.Errorf("truncate = %q, want no split character", got)
	}
}
