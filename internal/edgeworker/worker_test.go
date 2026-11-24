package edgeworker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	guestFragment = `<div class="top-card-layout__card"><h2 class="top-card-layout__title">Senior Go Engineer</h2></div>
<div class="show-more-less-html__markup"><p>Build <strong>distributed</strong> systems.</p><ul><li>Go</li><li>Postgres</li></ul></div>
<script>window.x = "authwall"</script>`
	viewPage = `<html><head><title>Acme hiring Senior Go Engineer | LinkedIn</title>
<meta property="og:title" content="Acme hiring Senior Go Engineer"><meta property="og:site_name" content="LinkedIn">
<script type="application/ld+json">{"@type":"JobPosting","title":"Senior Go Engineer"}</script></head><body><p>x</p></body></html>`
)

// fakeRenderer returns a fixed page and counts its calls.
type fakeRenderer struct {
	calls atomic.Int32
	urls  []string
	page  Rendered
	err   error
}

func (r *fakeRenderer) Render(_ context.Context, url string) (Rendered, error) {
	r.calls.Add(1)
	r.urls = append(r.urls, url)
	return r.page, r.err
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testWorker reads LinkedIn from base and refuses nothing about addresses (the
// test servers are on loopback); the guard has its own tests.
func testWorker(base string, r Renderer) *Worker {
	w := New(nil, Options{Guard: &Guard{allowPrivate: true}, Renderer: r, Log: discard()})
	w.linkedinBase = base
	w.minDelay, w.maxDelay = 0, 0
	w.sleepFor = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return w
}

// linkedin serves the guest endpoints, recording what it was asked.
type linkedin struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
}

func newLinkedIn(t *testing.T, handler http.HandlerFunc) *linkedin {
	t.Helper()
	l := &linkedin{}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.requests = append(l.requests, r)
		l.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(l.Close)
	return l
}

func TestHandleLinkedInGuestEndpointWithoutABrowser(t *testing.T) {
	li := newLinkedIn(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jobs-guest/jobs/api/jobPosting/3812345678":
			io.WriteString(w, guestFragment)
		case "/jobs/view/3812345678":
			io.WriteString(w, viewPage)
		default:
			http.NotFound(w, r)
		}
	})
	chrome := &fakeRenderer{}
	w := testWorker(li.URL, chrome)

	res := w.Handle(t.Context(), Job{ID: "j1", URL: "https://www.linkedin.com/jobs/view/senior-go-engineer-at-acme-3812345678?trk=public_jobs"})
	if res.Error != "" || res.StatusCode != 200 {
		t.Fatalf("result = %+v", res)
	}
	for _, want := range []string{"Build **distributed** systems.", "Postgres"} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("markdown lacks %q:\n%s", want, res.Markdown)
		}
	}
	if strings.Contains(res.Markdown, "authwall") || strings.Contains(res.Markdown, "window.x") {
		t.Errorf("script text leaked into the markdown:\n%s", res.Markdown)
	}
	if !strings.Contains(res.RawHTML, "application/ld+json") {
		t.Errorf("raw HTML should be the public page with its JSON-LD: %s", res.RawHTML)
	}
	if res.Metadata["ogTitle"] != "Acme hiring Senior Go Engineer" || res.Metadata["ogSiteName"] != "LinkedIn" {
		t.Errorf("metadata = %v", res.Metadata)
	}
	if chrome.calls.Load() != 0 {
		t.Error("Chrome was used although the guest endpoint answered")
	}
	if len(li.requests) != 2 || li.requests[0].URL.Path != "/jobs-guest/jobs/api/jobPosting/3812345678" {
		t.Fatalf("requests = %v", li.requests)
	}
	for _, r := range li.requests {
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "Mozilla/5.0 (Macintosh") || strings.Contains(ua, "Go-http-client") {
			t.Errorf("User-Agent = %q", ua)
		}
		if r.Header.Get("Accept-Language") == "" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("headers = %v", r.Header)
		}
	}
}

func TestHandleLinkedInGuestFragmentAloneWhenTheViewPageIsRefused(t *testing.T) {
	li := newLinkedIn(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/jobs-guest/") {
			io.WriteString(w, guestFragment)
			return
		}
		w.WriteHeader(999)
	})
	res := testWorker(li.URL, &fakeRenderer{}).Handle(t.Context(), Job{URL: "https://www.linkedin.com/jobs/view/3812345678"})
	if res.Error != "" || !strings.Contains(res.Markdown, "distributed") || res.Metadata["ogTitle"] != "Senior Go Engineer" {
		t.Errorf("result = %+v", res)
	}
}

func TestHandleLinkedInBlockedFallsBackToTheBrowser(t *testing.T) {
	const url = "https://www.linkedin.com/jobs/view/3812345678"
	rendered := Rendered{HTML: "<html><head><title>Job | LinkedIn</title></head><body><h1>Senior Go Engineer</h1><p>" + strings.Repeat("Real description. ", 300) + "</p></body></html>", URL: url, Status: 200}
	for name, handler := range map[string]http.HandlerFunc{
		"status 999":   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(999) },
		"rate limited": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) },
		"forbidden":    func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
		"authwall":     func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/authwall?trk=x", http.StatusFound) },
		"login redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/uas/login?session_redirect=x", http.StatusSeeOther)
		},
		"not a posting": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html><body>Sign in</body></html>") },
	} {
		t.Run(name, func(t *testing.T) {
			li := newLinkedIn(t, handler)
			chrome := &fakeRenderer{page: rendered}
			res := testWorker(li.URL, chrome).Handle(t.Context(), Job{URL: url})
			if chrome.calls.Load() != 1 || chrome.urls[0] != url {
				t.Fatalf("browser calls = %d %v", chrome.calls.Load(), chrome.urls)
			}
			if res.Error != "" || res.StatusCode != 200 || !strings.Contains(res.Markdown, "Real description.") || res.Metadata["title"] != "Job | LinkedIn" {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestHandleLinkedInBlockedEverywhereIsReportedAsABlock(t *testing.T) {
	li := newLinkedIn(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(999) })
	for name, page := range map[string]Rendered{
		"authwall url":  {HTML: "<html><body>x</body></html>", URL: "https://www.linkedin.com/authwall?trk=x", Status: 200},
		"login text":    {HTML: "<html><body><h1>Join LinkedIn</h1><p>Sign in to view this job</p></body></html>", URL: "https://www.linkedin.com/jobs/view/1", Status: 200},
		"challenge":     {HTML: "<html><body><h1>Just a moment...</h1></body></html>", URL: "https://example.com/", Status: 200},
		"browser 999":   {HTML: "<html><body>nope</body></html>", URL: "https://www.linkedin.com/jobs/view/1", Status: 999},
		"short captcha": {HTML: "<html><body><p>Please solve this CAPTCHA</p></body></html>", URL: "https://example.com/", Status: 200},
	} {
		t.Run(name, func(t *testing.T) {
			res := testWorker(li.URL, &fakeRenderer{page: page}).Handle(t.Context(), Job{URL: "https://www.linkedin.com/jobs/view/3812345678"})
			if res.StatusCode != 401 || res.Error == "" || res.Markdown != "" {
				t.Errorf("result = %+v, want a status 401 failure", res)
			}
		})
	}
}

func TestHandleLinkedInGonePostingNeedsNoBrowser(t *testing.T) {
	li := newLinkedIn(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	chrome := &fakeRenderer{}
	res := testWorker(li.URL, chrome).Handle(t.Context(), Job{URL: "https://www.linkedin.com/jobs/view/3812345678"})
	if res.StatusCode != 404 || res.Error == "" || chrome.calls.Load() != 0 {
		t.Errorf("result = %+v, browser calls = %d", res, chrome.calls.Load())
	}
}

func TestHandleOtherHostsGoStraightToTheBrowser(t *testing.T) {
	li := newLinkedIn(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("LinkedIn asked for %s", r.URL) })
	chrome := &fakeRenderer{page: Rendered{HTML: `<html><head><meta property="og:title" content="Role"></head><body><h1>Role</h1><p>Do things.</p></body></html>`, URL: "https://jobs.example.com/1", Status: 200}}
	res := testWorker(li.URL, chrome).Handle(t.Context(), Job{URL: "https://jobs.example.com/1"})
	if res.Error != "" || res.StatusCode != 200 || !strings.Contains(res.Markdown, "Do things.") || res.Metadata["ogTitle"] != "Role" {
		t.Errorf("result = %+v", res)
	}
	// A LinkedIn link that is not a posting is not read as one.
	chrome.page.URL = "https://www.linkedin.com/company/acme"
	if res := testWorker(li.URL, chrome).Handle(t.Context(), Job{URL: "https://www.linkedin.com/company/acme"}); chrome.calls.Load() != 2 {
		t.Errorf("result = %+v, browser calls = %d", res, chrome.calls.Load())
	}
}

func TestHandleBrowserFailures(t *testing.T) {
	cases := map[string]struct {
		r          Renderer
		wantStatus int
	}{
		"the site answered 404": {&fakeRenderer{page: Rendered{HTML: "<html><body>gone</body></html>", URL: "https://example.com/", Status: 404}}, 404},
		"the browser failed":    {&fakeRenderer{err: errors.New("chrome crashed")}, 0},
		"no browser":            {nil, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := testWorker("http://unused", tc.r).Handle(t.Context(), Job{URL: "https://example.com/"})
			if res.Error == "" || res.StatusCode != tc.wantStatus {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

// Other users' URLs run on this machine's network: nothing private is opened.
func TestHandleRefusesPrivateAddresses(t *testing.T) {
	li := newLinkedIn(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("a request reached the network: %s", r.URL) })
	chrome := &fakeRenderer{}
	w := New(nil, Options{Guard: &Guard{Resolver: fakeResolver{"intranet.example": {"10.0.0.7"}, "www.linkedin.com": {"13.107.42.14"}}}, Renderer: chrome, Log: discard()})
	w.linkedinBase = li.URL
	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://127.0.0.1:8080/admin",
		"http://[::1]/",
		"http://192.168.1.1/",
		"https://intranet.example/wiki",
		"file:///etc/passwd",
		"gopher://example.com/",
	} {
		res := w.Handle(t.Context(), Job{URL: url})
		if !strings.HasPrefix(res.Error, "refused:") || res.StatusCode != 0 || res.Markdown != "" {
			t.Errorf("%s: result = %+v", url, res)
		}
	}
	if chrome.calls.Load() != 0 {
		t.Error("the browser was asked to open a refused address")
	}

	// A LinkedIn-looking page cannot redirect the plain fetch somewhere private either.
	redirecting := newLinkedIn(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	w.linkedinBase = redirecting.URL
	w.guard = &Guard{Resolver: fakeResolver{"www.linkedin.com": {"13.107.42.14"}}}
	if _, err := w.fetch(t.Context(), "http://169.254.169.254/"); !errors.Is(err, ErrRefused) {
		t.Errorf("fetch of a private address: %v", err)
	}
}

func TestFetchChecksEveryRedirectHop(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	hop := newLinkedIn(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
	})
	// The first hop is allowed (public in this fake resolver) and reaches the
	// hop server; its redirect to loopback must be refused before any request.
	w := New(nil, Options{Guard: &Guard{Resolver: fakeResolver{"first.example": {"93.184.216.34"}}}, Log: discard()})
	w.http = &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, hop.Listener.Addr().String())
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	_, err := w.fetch(t.Context(), "http://first.example/")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("fetch = %v, want the redirect refused", err)
	}
	if len(hop.requests) != 1 {
		t.Errorf("hop requests = %d", len(hop.requests))
	}
	if hits.Load() != 0 {
		t.Error("the redirect target was requested")
	}
}

type fakeAPI struct {
	mu       sync.Mutex
	jobs     []*Job // handed out one per claim, then nil
	claimErr []error
	claims   int
	results  map[string]Result
	postErrs []error
	posted   chan string
}

func (a *fakeAPI) Claim(ctx context.Context) (*Job, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claims++
	if len(a.claimErr) > 0 {
		err := a.claimErr[0]
		a.claimErr = a.claimErr[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(a.jobs) > 0 {
		j := a.jobs[0]
		a.jobs = a.jobs[1:]
		return j, nil
	}
	return nil, ctx.Err()
}

func (a *fakeAPI) PostResult(_ context.Context, id string, r Result) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.postErrs) > 0 {
		err := a.postErrs[0]
		a.postErrs = a.postErrs[1:]
		if err != nil {
			return err
		}
	}
	if a.results == nil {
		a.results = map[string]Result{}
	}
	a.results[id] = r
	select {
	case a.posted <- id:
	default:
	}
	return nil
}

func TestRunClaimsReadsAndPosts(t *testing.T) {
	api := &fakeAPI{
		jobs:   []*Job{{ID: "a", URL: "https://example.com/a"}, {ID: "b", URL: "http://10.0.0.1/b"}},
		posted: make(chan string, 8),
		// Errors back off and recover; a result that fails once is retried.
		claimErr: []error{errors.New("boom"), ErrUnauthorized},
		postErrs: []error{errors.New("503")},
	}
	chrome := &fakeRenderer{page: Rendered{HTML: "<html><body><p>Body of the page.</p></body></html>", URL: "https://example.com/a", Status: 200}}
	w := testWorker("http://unused", chrome)
	w.guard = &Guard{Resolver: fakeResolver{"example.com": {"93.184.216.34"}}}
	w.api = api
	var slept []time.Duration
	w.sleepFor = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return ctx.Err() }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-api.posted:
		case <-time.After(5 * time.Second):
			t.Fatal("results were not posted")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := api.results["a"]; r.Error != "" || !strings.Contains(r.Markdown, "Body of the page.") {
		t.Errorf("result a = %+v", r)
	}
	if r := api.results["b"]; !strings.HasPrefix(r.Error, "refused:") {
		t.Errorf("result b = %+v", r)
	}
	if chrome.calls.Load() != 1 {
		t.Errorf("browser calls = %d, want 1 (the private address is not opened)", chrome.calls.Load())
	}
	// The unauthorized claim waits at least 30 seconds before the next try; a
	// plain error backs off from a second.
	var sawShort, sawLong bool
	for _, d := range slept {
		sawShort = sawShort || (d >= time.Second && d < 2*time.Second)
		sawLong = sawLong || d >= authBackoffMin
	}
	if !sawShort || !sawLong {
		t.Errorf("sleeps = %v", slept)
	}
}

func TestRunStopsOnCancelWithoutAJob(t *testing.T) {
	api := &fakeAPI{}
	w := testWorker("http://unused", nil)
	w.api = api
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := w.Run(ctx); err != nil || api.claims != 0 {
		t.Errorf("run = %v after %d claims", err, api.claims)
	}
}

// A shutdown signal lets the job in hand finish and post, then stops.
func TestRunFinishesTheJobInHandOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	api := &fakeAPI{jobs: []*Job{{ID: "a", URL: "https://example.com/a"}}, posted: make(chan string, 1)}
	chrome := &fakeRenderer{page: Rendered{HTML: "<html><body><p>Page.</p></body></html>", Status: 200, URL: "https://example.com/a"}}
	w := testWorker("http://unused", &cancelingRenderer{fakeRenderer: chrome, cancel: cancel})
	w.guard = &Guard{allowPrivate: true}
	w.api = api
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if r, ok := api.results["a"]; !ok || r.Error != "" {
		t.Errorf("result = %+v, posted = %v", r, ok)
	}
}

type cancelingRenderer struct {
	*fakeRenderer
	cancel context.CancelFunc
}

func (r *cancelingRenderer) Render(ctx context.Context, url string) (Rendered, error) {
	r.cancel() // the shutdown signal arrives mid-job
	return r.fakeRenderer.Render(ctx, url)
}

func TestClient(t *testing.T) {
	var claims atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer zedge_ok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v1/edge/jobs/claim":
			if claims.Add(1) == 1 {
				io.WriteString(w, `{"id":"j1","url":"https://example.com/"}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/edge/jobs/j1/result":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"markdown":"hi"`) {
				t.Errorf("body = %s", body)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL+"/", "zedge_ok")
	if j, err := c.Claim(t.Context()); err != nil || j == nil || j.ID != "j1" {
		t.Fatalf("claim = %+v, %v", j, err)
	}
	if j, err := c.Claim(t.Context()); err != nil || j != nil {
		t.Errorf("empty claim = %+v, %v", j, err)
	}
	if err := c.PostResult(t.Context(), "j1", Result{Markdown: "hi"}); err != nil {
		t.Error(err)
	}
	if err := c.PostResult(t.Context(), "gone", Result{}); !errors.Is(err, ErrJobGone) {
		t.Errorf("result for another job = %v", err)
	}
	if _, err := NewClient(srv.URL, "zedge_bad").Claim(t.Context()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("bad token = %v", err)
	}
}
