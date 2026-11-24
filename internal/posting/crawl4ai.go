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
)

// crawl4aiTimeout is the page timeout sent to Crawl4AI. The server clamps
// timeouts to 60s unless its operator raises CRAWL4AI_MAX_TIMEOUT_MS.
const crawl4aiTimeout = 30 * time.Second

// Crawl4AI scrapes through the REST API of the official Crawl4AI Docker server
// (image unclecode/crawl4ai, port 11235), which renders pages in a stealth
// Chromium. It has no hosted default: baseURL is the server you run.
type Crawl4AI struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewCrawl4AI returns a scraper for the Crawl4AI server at baseURL. token is
// sent as a bearer token when set (the server's CRAWL4AI_API_TOKEN, or a JWT it
// minted).
func NewCrawl4AI(baseURL, token string) *Crawl4AI {
	return &Crawl4AI{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: crawl4aiTimeout + 20*time.Second},
	}
}

// typed is Crawl4AI's {"type": ..., "params": ...} envelope for configs.
type typed struct {
	Type   string         `json:"type"`
	Params map[string]any `json:"params"`
}

type crawl4aiRequest struct {
	URLs          []string `json:"urls"`
	BrowserConfig typed    `json:"browser_config"`
	CrawlerConfig typed    `json:"crawler_config"`
}

// crawl4aiRequestFor builds the request for url. The server accepts only an
// allowlist of fields from a request body (js_code, magic, simulate_user and
// override_navigator are refused with a 400), so stealth comes from the
// browser's own enable_stealth patches and a random realistic user agent.
func crawl4aiRequestFor(url string) crawl4aiRequest {
	return crawl4aiRequest{
		URLs: []string{url},
		BrowserConfig: typed{Type: "BrowserConfig", Params: map[string]any{
			"headless":            true,
			"enable_stealth":      true,
			"user_agent_mode":     "random",
			"viewport_width":      1366,
			"viewport_height":     768,
			"java_script_enabled": true,
		}},
		CrawlerConfig: typed{Type: "CrawlerRunConfig", Params: map[string]any{
			"cache_mode":               "bypass",
			"page_timeout":             int(crawl4aiTimeout / time.Millisecond),
			"wait_until":               "domcontentloaded",
			"delay_before_return_html": 1.5,
			"remove_overlay_elements":  true,
			"remove_consent_popups":    true,
		}},
	}
}

type crawl4aiResponse struct {
	Detail  any `json:"detail"`
	Results []struct {
		Success      bool            `json:"success"`
		StatusCode   int             `json:"status_code"`
		ErrorMessage string          `json:"error_message"`
		HTML         string          `json:"html"`
		Markdown     json.RawMessage `json:"markdown"`
		Metadata     map[string]any  `json:"metadata"`
	} `json:"results"`
}

// markdownText returns the page markdown, which Crawl4AI sends as a string or
// as an object holding raw_markdown (and its fit and citation variants).
func markdownText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Raw string `json:"raw_markdown"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Raw
	}
	return ""
}

// Scrape reads url as markdown and raw HTML.
func (c *Crawl4AI) Scrape(ctx context.Context, url string) (Page, error) {
	body, err := json.Marshal(crawl4aiRequestFor(url))
	if err != nil {
		return Page{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/crawl", bytes.NewReader(body))
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Page{}, ctx.Err()
		}
		return Page{}, fmt.Errorf("crawl4ai: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return Page{}, fmt.Errorf("crawl4ai: read response: %w", err)
	}
	if len(raw) > maxResponse {
		return Page{}, &UnreadableError{Reason: "the page is too large"}
	}
	var cr crawl4aiResponse
	decodeErr := json.Unmarshal(raw, &cr)
	detail, _ := cr.Detail.(string)
	switch {
	case res.StatusCode == http.StatusGatewayTimeout || res.StatusCode == http.StatusRequestTimeout:
		return Page{}, &UnreadableError{Reason: "the page took too long to load"}
	case res.StatusCode == http.StatusBadRequest && strings.HasPrefix(detail, "URL blocked"):
		// The server's SSRF guard refused the address: nothing to read there.
		return Page{}, &UnreadableError{Reason: "the scraper refused to open this address"}
	case res.StatusCode != http.StatusOK:
		// 401/403 (token), 400 (rejected config), 429/503 (busy) and 5xx are the
		// scraper's own failures, not the page's.
		return Page{}, fmt.Errorf("crawl4ai: status %d: %s", res.StatusCode, truncate(detail, 200))
	case decodeErr != nil:
		return Page{}, fmt.Errorf("crawl4ai: undecodable response: %w", decodeErr)
	case len(cr.Results) == 0:
		return Page{}, errors.New("crawl4ai: no result for the page")
	}
	r := cr.Results[0]
	if !r.Success {
		// error_message carries browser internals (net::ERR_*): keep it out of
		// the reason a member sees.
		if strings.Contains(strings.ToLower(r.ErrorMessage), "timeout") {
			return Page{}, &UnreadableError{Reason: "the page took too long to load"}
		}
		return Page{}, &UnreadableError{Reason: "the page could not be loaded"}
	}
	if r.StatusCode >= 400 {
		return Page{}, &UnreadableError{Reason: fmt.Sprintf("the site answered %d", r.StatusCode)}
	}
	md := markdownText(r.Markdown)
	if strings.TrimSpace(md) == "" && strings.TrimSpace(r.HTML) == "" {
		return Page{}, &UnreadableError{Reason: "the page has no readable content"}
	}
	return Page{Markdown: md, RawHTML: r.HTML, Metadata: crawl4aiMetadata(r.Metadata, r.StatusCode)}, nil
}

// crawl4aiMetadata returns Crawl4AI's page metadata under the keys the
// Firecrawl provider produces (ogTitle, ogDescription, statusCode), alongside
// everything the server sent (title, description, og:* ...) unchanged.
func crawl4aiMetadata(in map[string]any, status int) map[string]any {
	m := make(map[string]any, len(in)+3)
	for k, v := range in {
		m[k] = v
	}
	for from, to := range map[string]string{"og:title": "ogTitle", "og:description": "ogDescription", "og:site_name": "ogSiteName", "og:url": "ogUrl", "og:image": "ogImage"} {
		if v, ok := in[from]; ok && v != nil {
			m[to] = v
		}
	}
	if status > 0 {
		m["statusCode"] = float64(status)
	}
	return m
}
