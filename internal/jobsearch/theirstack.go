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
	// CountryCode is the posting's ISO 3166-1 alpha-2 country, when known.
	CountryCode string
	// Seniority is TheirStack's guess (junior, mid_level, senior, staff,
	// c_level), a hint rather than a label.
	Seniority   string
	Description string
}

// Result is what a search returned and what it cost.
type Result struct {
	// Postings are those usable as alerts: with an id, a title and an https link.
	Postings []Posting
	// Returned counts every posting the provider returned (and charged for),
	// usable or not.
	Returned int
	// Newest is the latest discovery time among everything returned, usable or
	// not; zero when none.
	Newest time.Time
	// Credits is what the provider charged for the call.
	Credits int
}

// Error is a search the provider refused or that failed. Unknown means its
// cost cannot be known (no answer arrived, so the search may have run);
// otherwise Credits is what the provider said it charged.
type Error struct {
	Reason  string
	Unknown bool
	Credits int
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
		CountryCode   string `json:"country_code"`
		Seniority     string `json:"seniority"`
		Description   string `json:"description"`
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
		return Result{}, &Error{Reason: "the job search provider did not answer", Unknown: true}
	}
	defer res.Body.Close()
	// Once the provider answers, it says what it charged; no header is nothing.
	charged := 0
	if n, err := strconv.Atoi(res.Header.Get("X-Credits-Charged")); err == nil && n >= 0 {
		charged = n
	}
	fail := func(reason string) (Result, error) { return Result{}, &Error{Reason: reason, Credits: charged} }
	payload, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return fail("the job search provider's answer was cut off")
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fail("the job search provider rejected its key")
	case res.StatusCode == http.StatusPaymentRequired:
		return fail("the job search provider is out of credits")
	case res.StatusCode == http.StatusTooManyRequests:
		return fail("the job search provider is rate limiting searches")
	case res.StatusCode >= 400 && res.StatusCode < 500:
		return fail(fmt.Sprintf("the job search provider refused the search (%d)", res.StatusCode))
	case res.StatusCode >= 500:
		return fail(fmt.Sprintf("the job search provider failed (%d)", res.StatusCode))
	}
	var sr searchResponse
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&sr); err != nil {
		return fail("the job search provider's answer was unreadable")
	}
	out := Result{Postings: make([]Posting, 0, len(sr.Data)), Returned: len(sr.Data), Credits: len(sr.Data)}
	if res.Header.Get("X-Credits-Charged") != "" {
		out.Credits = charged
	}
	for _, d := range sr.Data {
		p := Posting{ID: d.ID.String(), Title: strings.TrimSpace(d.JobTitle), Company: strings.TrimSpace(d.Company), Remote: d.Remote,
			Seniority: strings.TrimSpace(d.Seniority), Description: strings.TrimSpace(d.Description)}
		if c := strings.ToUpper(strings.TrimSpace(d.CountryCode)); len(c) == 2 {
			p.CountryCode = c
		}
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
		p.DiscoveredAt = discoveredAt(d.DiscoveredAt)
		if p.DiscoveredAt.After(out.Newest) {
			out.Newest = p.DiscoveredAt
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

// discoveredAt reads TheirStack's discovery time, with or without a zone (UTC).
func discoveredAt(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999", s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// ErrNotConfigured answers when the owner has not set up a search provider.
var ErrNotConfigured = errors.New("job search is not configured")

// Balance is the team's TheirStack API credits.
type Balance struct {
	// Credits are those left to spend.
	Credits int
	// Used are those spent in the current billing cycle.
	Used int
	// Expires is when the earliest of them expire, if known.
	Expires *time.Time
}

// Balance reads the team's API credit balance; it costs no credits.
func (t *TheirStack) Balance(ctx context.Context, key string) (Balance, error) {
	base := t.BaseURL
	if base == "" {
		base = TheirStackURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v0/billing/credit-balance", nil)
	if err != nil {
		return Balance{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return Balance{}, &Error{Reason: "the job search provider did not answer"}
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return Balance{}, &Error{Reason: "the job search provider rejected its key"}
	case res.StatusCode != http.StatusOK:
		return Balance{}, &Error{Reason: fmt.Sprintf("the job search provider failed (%d)", res.StatusCode)}
	}
	var b struct {
		APICredits         int        `json:"api_credits"`
		UsedAPICredits     int        `json:"used_api_credits"`
		EarliestExpiration *time.Time `json:"earliest_expiration"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&b); err != nil {
		return Balance{}, &Error{Reason: "the job search provider's answer was unreadable"}
	}
	return Balance{Credits: b.APICredits, Used: b.UsedAPICredits, Expires: b.EarliestExpiration}, nil
}
