// Package posting reads a job posting's page into job fields. Pages are
// fetched through a Scraper (Firecrawl's API today; any provider that returns
// markdown and raw HTML fits). Fields come from the page's schema.org
// JobPosting data, which job boards and applicant tracking systems publish for
// search engines; the page's markdown becomes the description. Fields the page
// does not publish are read by an extraction model (Extraction).
package posting

import (
	"context"
	"errors"
	"strings"
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
)

// Page is what a scraper read: the page as markdown, its raw HTML (for
// JSON-LD), and its metadata (og:title, the site's status code).
type Page struct {
	Markdown string
	RawHTML  string
	Metadata map[string]any
}

// Scraper fetches a page. Implementations return *UnreadableError for pages
// they could not read, and a plain error when the scraper itself failed.
type Scraper interface {
	Scrape(ctx context.Context, url string) (Page, error)
}

// Reader reads postings through a Scraper.
type Reader struct{ scraper Scraper }

// New returns a reader over s, or nil when s is nil.
func New(s Scraper) *Reader {
	if s == nil {
		return nil
	}
	return &Reader{scraper: s}
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
	page, err := r.scraper.Scrape(ctx, target)
	if err != nil {
		return Posting{}, err
	}
	p := fromJobPosting(findJobPosting(page.RawHTML))
	if p.Title == "" {
		p.Title = metaString(page.Metadata, "ogTitle")
	}
	p.Description = truncate(strings.TrimSpace(page.Markdown), maxDescription)
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
