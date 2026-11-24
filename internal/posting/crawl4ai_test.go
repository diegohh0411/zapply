package posting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// crawl4aiServer answers POST /crawl with status and body, and hands each
// decoded request (and its Authorization header) to check.
func crawl4aiServer(t *testing.T, status int, body any, check func(auth string, req map[string]any)) *Crawl4AI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/crawl" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if check != nil {
			check(r.Header.Get("Authorization"), req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return NewCrawl4AI(srv.URL+"/", "token")
}

func result(fields map[string]any) map[string]any {
	r := map[string]any{"success": true, "status_code": 200, "html": "<html></html>", "markdown": map[string]any{"raw_markdown": "# Engineer"}, "metadata": map[string]any{}}
	for k, v := range fields {
		r[k] = v
	}
	return map[string]any{"success": true, "results": []any{r}}
}

func TestCrawl4AIRequestShape(t *testing.T) {
	c := crawl4aiServer(t, 200, result(nil), func(auth string, req map[string]any) {
		if auth != "Bearer token" {
			t.Errorf("Authorization = %q", auth)
		}
		urls, _ := req["urls"].([]any)
		if len(urls) != 1 || urls[0] != "https://example.com/job" {
			t.Errorf("urls = %v", req["urls"])
		}
		browser := req["browser_config"].(map[string]any)
		if browser["type"] != "BrowserConfig" {
			t.Errorf("browser type = %v", browser["type"])
		}
		bp := browser["params"].(map[string]any)
		if bp["headless"] != true || bp["enable_stealth"] != true || bp["user_agent_mode"] != "random" || bp["viewport_width"] == nil {
			t.Errorf("browser params = %v", bp)
		}
		crawler := req["crawler_config"].(map[string]any)
		if crawler["type"] != "CrawlerRunConfig" {
			t.Errorf("crawler type = %v", crawler["type"])
		}
		cp := crawler["params"].(map[string]any)
		if cp["cache_mode"] != "bypass" || cp["page_timeout"] != float64(30000) {
			t.Errorf("crawler params = %v", cp)
		}
		// The server answers 400 to these from a request body.
		for _, forbidden := range []string{"magic", "simulate_user", "override_navigator", "js_code"} {
			if _, ok := cp[forbidden]; ok {
				t.Errorf("crawler params include %s, which the server refuses", forbidden)
			}
		}
	})
	if _, err := c.Scrape(context.Background(), "https://example.com/job"); err != nil {
		t.Fatal(err)
	}
}

func TestCrawl4AINoTokenSendsNoAuthorization(t *testing.T) {
	c := crawl4aiServer(t, 200, result(nil), func(auth string, _ map[string]any) {
		if auth != "" {
			t.Errorf("Authorization = %q", auth)
		}
	})
	c.token = ""
	if _, err := c.Scrape(context.Background(), "https://example.com/job"); err != nil {
		t.Fatal(err)
	}
}

func TestCrawl4AIMapsAPage(t *testing.T) {
	c := crawl4aiServer(t, 200, result(map[string]any{
		"html":     `<script type="application/ld+json">{"@type":"JobPosting"}</script>`,
		"metadata": map[string]any{"title": "Engineer", "og:title": "Engineer at Acme", "og:description": "Build things"},
	}), nil)
	page, err := c.Scrape(context.Background(), "https://example.com/job")
	if err != nil {
		t.Fatal(err)
	}
	if page.Markdown != "# Engineer" || !strings.Contains(page.RawHTML, "JobPosting") {
		t.Errorf("page = %+v", page)
	}
	if metaString(page.Metadata, "ogTitle") != "Engineer at Acme" || metaString(page.Metadata, "title") != "Engineer" || metaString(page.Metadata, "ogDescription") != "Build things" {
		t.Errorf("metadata = %v", page.Metadata)
	}
	if page.Metadata["statusCode"] != float64(200) {
		t.Errorf("statusCode = %v", page.Metadata["statusCode"])
	}
}

func TestCrawl4AIMarkdownAsAString(t *testing.T) {
	c := crawl4aiServer(t, 200, result(map[string]any{"markdown": "plain"}), nil)
	page, err := c.Scrape(context.Background(), "https://example.com/job")
	if err != nil || page.Markdown != "plain" {
		t.Fatalf("page = %+v, err = %v", page, err)
	}
}

func TestCrawl4AIReadsThroughTheReader(t *testing.T) {
	c := crawl4aiServer(t, 200, result(map[string]any{"metadata": map[string]any{"og:title": "Staff Engineer"}}), nil)
	p, err := New(c).Read(context.Background(), "https://example.com/job")
	if err != nil || p.Title != "Staff Engineer" || p.Description != "# Engineer" {
		t.Fatalf("posting = %+v, err = %v", p, err)
	}
}

func TestCrawl4AIUnreadablePages(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   any
		reason string
	}{
		"site 404":        {200, result(map[string]any{"status_code": 404}), "the site answered 404"},
		"site 403":        {200, result(map[string]any{"status_code": 403}), "the site answered 403"},
		"site 500":        {200, result(map[string]any{"status_code": 500}), "the site answered 500"},
		"crawl failed":    {200, result(map[string]any{"success": false, "error_message": "net::ERR_NAME_NOT_RESOLVED at http://10.0.0.1"}), "the page could not be loaded"},
		"crawl timed out": {200, result(map[string]any{"success": false, "error_message": "Page.goto: Timeout 30000ms exceeded"}), "the page took too long to load"},
		"empty":           {200, result(map[string]any{"markdown": map[string]any{"raw_markdown": " "}, "html": ""}), "the page has no readable content"},
		"gateway timeout": {504, map[string]any{"detail": "Crawl exceeded the time limit"}, "the page took too long to load"},
		"address blocked": {400, map[string]any{"detail": "URL blocked (SSRF protection): URL blocked"}, "the scraper refused to open this address"},
	} {
		t.Run(name, func(t *testing.T) {
			c := crawl4aiServer(t, tc.status, tc.body, nil)
			_, err := c.Scrape(context.Background(), "https://example.com/job")
			var u *UnreadableError
			if !errors.As(err, &u) || u.Reason != tc.reason {
				t.Fatalf("err = %v, want unreadable %q", err, tc.reason)
			}
			if strings.Contains(u.Reason, "net::") || strings.Contains(u.Reason, "10.0.0.1") {
				t.Errorf("reason leaks scraper internals: %q", u.Reason)
			}
		})
	}
}

func TestCrawl4AIScraperFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   any
	}{
		"unauthorized":    {401, map[string]any{"detail": "Authentication required"}},
		"rejected config": {400, map[string]any{"detail": "Rejected config: field 'magic' is not permitted"}},
		"busy":            {503, map[string]any{"detail": "Server busy, retry later"}},
		"server error":    {500, map[string]any{"detail": "boom"}},
		"bad gateway":     {502, map[string]any{"detail": "crawl failed"}},
		"no results":      {200, map[string]any{"success": true, "results": []any{}}},
		"not json":        {200, "<html>proxy</html>"},
	} {
		t.Run(name, func(t *testing.T) {
			c := crawl4aiServer(t, tc.status, tc.body, nil)
			_, err := c.Scrape(context.Background(), "https://example.com/job")
			var u *UnreadableError
			if err == nil || errors.As(err, &u) {
				t.Fatalf("err = %v, want a plain scraper error", err)
			}
		})
	}
}

func TestCrawl4AIUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	_, err := NewCrawl4AI(srv.URL, "").Scrape(context.Background(), "https://example.com/job")
	var u *UnreadableError
	if err == nil || errors.As(err, &u) {
		t.Fatalf("err = %v, want a plain error", err)
	}
}

// TestCrawl4AIServer reads a page through a real Crawl4AI container. It runs
// only when CRAWL4AI_TEST_URL names one (and CRAWL4AI_TEST_TOKEN its token).
func TestCrawl4AIServer(t *testing.T) {
	base := os.Getenv("CRAWL4AI_TEST_URL")
	if base == "" {
		t.Skip("CRAWL4AI_TEST_URL not set")
	}
	c := NewCrawl4AI(base, os.Getenv("CRAWL4AI_TEST_TOKEN"))
	page, err := c.Scrape(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Markdown, "Example Domain") && !strings.Contains(page.Markdown, "documentation examples") {
		t.Errorf("markdown = %q", page.Markdown)
	}
	if metaString(page.Metadata, "title") != "Example Domain" || page.RawHTML == "" {
		t.Errorf("metadata = %v, html %d bytes", page.Metadata, len(page.RawHTML))
	}
	var u *UnreadableError
	if _, err := c.Scrape(context.Background(), "https://example.com/definitely-missing"); !errors.As(err, &u) {
		t.Errorf("missing page err = %v, want unreadable", err)
	}
}
