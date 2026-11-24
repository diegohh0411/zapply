// Package jobsearch finds postings across every company through a job search
// provider. TheirStack indexes postings from LinkedIn, job boards and ATSs,
// and charges one API credit per posting it returns, so a search narrowed to
// what was discovered since the last one costs only what is new.
package jobsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TheirStackURL is TheirStack's API.
const TheirStackURL = "https://api.theirstack.com"

// Query is a saved search.
type Query struct {
	// Titles match a posting whose title contains every word of any of them.
	Titles []string
	// Locations match a posting whose location contains any of them; empty
	// matches anywhere.
	Locations []string
	// Country is an ISO country code, or "".
	Country    string
	RemoteOnly bool
	// Since asks only for postings the provider discovered at or after it.
	Since time.Time
	// Limit bounds how many postings, and so credits, one search may return.
	Limit int
}

// Posting is one opening the provider found.
type Posting struct {
	ID           string
	Title        string
	Company      string
	Location     string
	URL          string
	Remote       bool
	PostedAt     *time.Time
	DiscoveredAt time.Time
}

// Result is what a search returned and what it cost.
type Result struct {
	Postings []Posting
	// Credits is what the provider charged for the call.
	Credits int
}

// Error is a search the provider refused or that failed. Charged tells
// whether it may have cost credits: false when the provider answered without
// running it.
type Error struct {
	Reason  string
	Charged bool
}

func (e *Error) Error() string { return "job search: " + e.Reason }

// TheirStack searches TheirStack's job search API.
type TheirStack struct {
	BaseURL string
	HTTP    *http.Client
}

// maxAgeDays bounds how old a newly discovered posting may be: TheirStack
// needs a posted-date filter, and a month-old posting it only now found is not
// an early chance to apply.
const maxAgeDays = 30

type searchRequest struct {
	JobTitleOr           []string  `json:"job_title_or"`
	PostedAtMaxAgeDays   int       `json:"posted_at_max_age_days"`
	DiscoveredAtGte      string    `json:"discovered_at_gte"`
	JobCountryCodeOr     []string  `json:"job_country_code_or,omitempty"`
	JobLocationPatternOr []string  `json:"job_location_pattern_or,omitempty"`
	WorkplaceTypesOr     []string  `json:"workplace_types_or,omitempty"`
	OrderBy              []orderBy `json:"order_by"`
	Limit                int       `json:"limit"`
	Page                 int       `json:"page"`
	IncludeTotalResults  bool      `json:"include_total_results"`
}

type orderBy struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc"`
}

type searchResponse struct {
	Data []struct {
		ID            json.Number `json:"id"`
		JobTitle      string      `json:"job_title"`
		URL           string      `json:"url"`
		FinalURL      string      `json:"final_url"`
		SourceURL     string      `json:"source_url"`
		DatePosted    string      `json:"date_posted"`
		DiscoveredAt  string      `json:"discovered_at"`
		Company       string      `json:"company"`
		CompanyObject *struct {
			Name string `json:"name"`
		} `json:"company_object"`
		Location      string `json:"location"`
		ShortLocation string `json:"short_location"`
		Remote        bool   `json:"remote"`
	} `json:"data"`
}

// Search runs q with the API key key.
func (t *TheirStack) Search(ctx context.Context, key string, q Query) (Result, error) {
	body := searchRequest{
		JobTitleOr:         q.Titles,
		PostedAtMaxAgeDays: maxAgeDays,
		DiscoveredAtGte:    q.Since.UTC().Format(time.RFC3339),
		OrderBy:            []orderBy{{Field: "discovered_at", Desc: false}},
		Limit:              q.Limit,
	}
	if q.Country != "" {
		body.JobCountryCodeOr = []string{strings.ToUpper(q.Country)}
	}
	for _, l := range q.Locations {
		body.JobLocationPatternOr = append(body.JobLocationPatternOr, "(?i)"+regexp.QuoteMeta(l))
	}
	if q.RemoteOnly {
		body.WorkplaceTypesOr = []string{"remote"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	base := t.BaseURL
	if base == "" {
		base = TheirStackURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/jobs/search", bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		// The search may have run and been charged.
		return Result{}, &Error{Reason: "the job search provider did not answer", Charged: true}
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return Result{}, &Error{Reason: "the job search provider's answer was cut off", Charged: true}
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return Result{}, &Error{Reason: "the job search provider rejected its key"}
	case res.StatusCode == http.StatusPaymentRequired:
		return Result{}, &Error{Reason: "the job search provider is out of credits"}
	case res.StatusCode == http.StatusTooManyRequests:
		return Result{}, &Error{Reason: "the job search provider is rate limiting searches"}
	case res.StatusCode >= 400 && res.StatusCode < 500:
		return Result{}, &Error{Reason: fmt.Sprintf("the job search provider refused the search (%d)", res.StatusCode)}
	case res.StatusCode >= 500:
		return Result{}, &Error{Reason: fmt.Sprintf("the job search provider failed (%d)", res.StatusCode), Charged: true}
	}
	var sr searchResponse
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&sr); err != nil {
		return Result{}, &Error{Reason: "the job search provider's answer was unreadable", Charged: true}
	}
	out := Result{Postings: make([]Posting, 0, len(sr.Data)), Credits: len(sr.Data)}
	if n, err := strconv.Atoi(res.Header.Get("X-Credits-Charged")); err == nil && n >= 0 {
		out.Credits = n
	}
	for _, d := range sr.Data {
		p := Posting{ID: d.ID.String(), Title: strings.TrimSpace(d.JobTitle), Company: strings.TrimSpace(d.Company), Remote: d.Remote}
		if p.Company == "" && d.CompanyObject != nil {
			p.Company = strings.TrimSpace(d.CompanyObject.Name)
		}
		p.Location = strings.TrimSpace(d.Location)
		if p.Location == "" {
			p.Location = strings.TrimSpace(d.ShortLocation)
		}
		for _, u := range []string{d.FinalURL, d.URL, d.SourceURL} {
			if strings.HasPrefix(u, "https://") {
				p.URL = u
				break
			}
		}
		if t, err := time.Parse(time.RFC3339, d.DiscoveredAt); err == nil {
			p.DiscoveredAt = t
		} else if t, err := time.Parse("2006-01-02T15:04:05", d.DiscoveredAt); err == nil {
			p.DiscoveredAt = t.UTC()
		}
		if t, err := time.Parse("2006-01-02", d.DatePosted); err == nil {
			p.PostedAt = &t
		}
		if p.ID == "" || p.Title == "" || p.URL == "" {
			continue
		}
		out.Postings = append(out.Postings, p)
	}
	return out, nil
}

// ErrNotConfigured answers when the owner has not set up a search provider.
var ErrNotConfigured = errors.New("job search is not configured")
