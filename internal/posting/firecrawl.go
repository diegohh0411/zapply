package posting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FirecrawlCloud is Firecrawl's hosted API.
const FirecrawlCloud = "https://api.firecrawl.dev"

const (
	// maxResponse bounds a scrape response: the raw HTML of a heavy job page
	// is a few MB.
	maxResponse = 16 << 20
	// scrapeTimeout is what Firecrawl may spend on a page; the HTTP client
	// waits a little longer for its answer.
	scrapeTimeout = 30 * time.Second
)

// Firecrawl scrapes through a Firecrawl v2 API: Firecrawl's hosted API, a
// self-hosted server or Firecrawl Local.
type Firecrawl struct {
	baseURL string
	key     string
	http    *http.Client
}

// NewFirecrawl returns a scraper for the Firecrawl API at baseURL (Firecrawl's
// hosted API when empty). key is sent as a bearer token when set.
func NewFirecrawl(baseURL, key string) *Firecrawl {
	if baseURL == "" {
		baseURL = FirecrawlCloud
	}
	return &Firecrawl{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
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

// Scrape reads url as markdown and raw HTML.
func (f *Firecrawl) Scrape(ctx context.Context, url string) (Page, error) {
	body, err := json.Marshal(scrapeRequest{URL: url, Formats: []string{"markdown", "rawHtml"}, Timeout: int(scrapeTimeout / time.Millisecond)})
	if err != nil {
		return Page{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/v2/scrape", bytes.NewReader(body))
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.key != "" {
		req.Header.Set("Authorization", "Bearer "+f.key)
	}
	res, err := f.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Page{}, ctx.Err()
		}
		return Page{}, fmt.Errorf("scrape: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return Page{}, fmt.Errorf("scrape: read response: %w", err)
	}
	if len(raw) > maxResponse {
		return Page{}, &UnreadableError{Reason: "the page is too large"}
	}
	var sr scrapeResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return Page{}, fmt.Errorf("scrape: status %d, undecodable response: %w", res.StatusCode, err)
	}
	switch {
	case res.StatusCode == http.StatusRequestTimeout || res.StatusCode == http.StatusGatewayTimeout:
		return Page{}, &UnreadableError{Reason: "the page took too long to load"}
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusPaymentRequired:
		// The scraper's key or credits, not the page: a server-side failure.
		return Page{}, fmt.Errorf("scrape: status %d: %s", res.StatusCode, sr.Error)
	case res.StatusCode >= 500 && sr.Error == "":
		return Page{}, fmt.Errorf("scrape: status %d", res.StatusCode)
	case !sr.Success || res.StatusCode >= 400:
		reason := sr.Error
		if reason == "" {
			reason = fmt.Sprintf("the scraper answered %d", res.StatusCode)
		}
		return Page{}, &UnreadableError{Reason: reason}
	case sr.Data.Blocked:
		return Page{}, &UnreadableError{Reason: "the site refused to load the page"}
	}
	if code, ok := sr.Data.Metadata["statusCode"].(float64); ok && code >= 400 {
		return Page{}, &UnreadableError{Reason: fmt.Sprintf("the site answered %d", int(code))}
	}
	page := Page{Markdown: sr.Data.Markdown, RawHTML: sr.Data.RawHTML, Metadata: sr.Data.Metadata}
	if n, ok := sr.Data.Metadata["creditsUsed"].(float64); ok && n > 0 && n < 1000 {
		page.Credits = int(n)
	}
	return page, nil
}
