// Package posting reads a job posting's page into job fields. Pages are
// fetched through a Firecrawl v2 scrape API: in development, the Firecrawl
// Local container in deploy/compose.yaml. Fields come from the page's
// schema.org JobPosting data, which job boards and applicant tracking systems
// publish for search engines; the page's markdown becomes the description.
package posting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/internal/joburl"
)

// ErrNotConfigured answers when no scrape API is configured.
var ErrNotConfigured = errors.New("posting reader is not configured")

// ErrInvalidURL is a URL that is not http(s).
var ErrInvalidURL = errors.New("not an http(s) URL")

// UnreadableError is a page the scraper could not read: blocked, missing, or
// too slow. Reason is safe to show.
type UnreadableError struct{ Reason string }

func (e *UnreadableError) Error() string { return "posting unreadable: " + e.Reason }

// Posting is what a page said. Empty fields were not found. Enum fields use
// the job API's values (WorkMode, EmploymentType, SalaryPeriod).
type Posting struct {
	Title          string
	Company        string
	Location       string
	WorkMode       string
	EmploymentType string
	SalaryMin      *int64
	SalaryMax      *int64
	SalaryCurrency string
	SalaryPeriod   string
	// Description is the page as markdown, capped at the job description limit.
	Description string
}

const (
	// maxDescription matches the job description limit (internal/jobs).
	maxDescription = 100_000
	// maxResponse bounds a scrape response: the raw HTML of a heavy job page
	// is a few MB.
	maxResponse = 16 << 20
	// scrapeTimeout is what the scraper may spend on a page; the HTTP client
	// waits a little longer for its answer.
	scrapeTimeout = 30 * time.Second
)

// Reader reads postings through a Firecrawl v2 API.
type Reader struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a reader for the Firecrawl API at baseURL, or nil when baseURL
// is empty. token is sent as a bearer token when set.
func New(baseURL, token string) *Reader {
	if baseURL == "" {
		return nil
	}
	return &Reader{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: scrapeTimeout + 10*time.Second},
	}
}

type scrapeRequest struct {
	URL     string   `json:"url"`
	Formats []string `json:"formats"`
	Timeout int      `json:"timeout"`
}

type scrapeResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		Markdown string         `json:"markdown"`
		RawHTML  string         `json:"rawHtml"`
		Metadata map[string]any `json:"metadata"`
		Warning  string         `json:"warning"`
		Blocked  bool           `json:"blocked"`
	} `json:"data"`
}

// Read scrapes rawURL and returns what its page says.
func (r *Reader) Read(ctx context.Context, rawURL string) (Posting, error) {
	if r == nil {
		return Posting{}, ErrNotConfigured
	}
	target, err := joburl.Normalize(rawURL)
	if err != nil {
		return Posting{}, ErrInvalidURL
	}
	body, err := json.Marshal(scrapeRequest{URL: target, Formats: []string{"markdown", "rawHtml"}, Timeout: int(scrapeTimeout / time.Millisecond)})
	if err != nil {
		return Posting{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v2/scrape", bytes.NewReader(body))
	if err != nil {
		return Posting{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	res, err := r.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Posting{}, ctx.Err()
		}
		return Posting{}, fmt.Errorf("scrape: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return Posting{}, fmt.Errorf("scrape: read response: %w", err)
	}
	if len(raw) > maxResponse {
		return Posting{}, &UnreadableError{Reason: "the page is too large"}
	}
	var sr scrapeResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return Posting{}, fmt.Errorf("scrape: status %d, undecodable response: %w", res.StatusCode, err)
	}
	switch {
	case res.StatusCode == http.StatusRequestTimeout || res.StatusCode == http.StatusGatewayTimeout:
		return Posting{}, &UnreadableError{Reason: "the page took too long to load"}
	case res.StatusCode >= 500 && sr.Error == "":
		return Posting{}, fmt.Errorf("scrape: status %d", res.StatusCode)
	case !sr.Success || res.StatusCode >= 400:
		reason := sr.Error
		if reason == "" {
			reason = fmt.Sprintf("the scraper answered %d", res.StatusCode)
		}
		return Posting{}, &UnreadableError{Reason: reason}
	case sr.Data.Blocked:
		return Posting{}, &UnreadableError{Reason: "the site refused to load the page"}
	}
	if code, ok := sr.Data.Metadata["statusCode"].(float64); ok && code >= 400 {
		return Posting{}, &UnreadableError{Reason: fmt.Sprintf("the site answered %d", int(code))}
	}

	p := fromJobPosting(findJobPosting(sr.Data.RawHTML))
	if p.Title == "" {
		p.Title = metaString(sr.Data.Metadata, "ogTitle")
	}
	p.Description = truncate(strings.TrimSpace(sr.Data.Markdown), maxDescription)
	return p, nil
}

func metaString(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		if len(v) > 0 {
			s, _ := v[0].(string)
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
