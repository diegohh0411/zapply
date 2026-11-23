package edgeworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// browserUA is what a current desktop Chrome sends. Headless Chrome is
	// given the same, so the two paths look alike.
	browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	maxFetchBody = 8 << 20
	maxRedirects = 3
	fetchTimeout = 20 * time.Second
	dialTimeout  = 10 * time.Second
)

// fetched is a page read over plain HTTP.
type fetched struct {
	Status int
	URL    string // after redirects
	Body   string
}

// newHTTPClient returns a client that keeps no cookies, follows no redirect on
// its own (each hop is checked by fetch) and refuses non-public addresses at
// connection time.
func newHTTPClient(g *Guard) *http.Client {
	d := &net.Dialer{Timeout: dialTimeout, Control: g.DialControl}
	return &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			DialContext:         d.DialContext,
			TLSHandshakeTimeout: dialTimeout,
			// A proxy from the environment would dial for us, past the guard.
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// fetch GETs rawURL with the headers of a browser opening a link, following up
// to maxRedirects redirects that each pass the guard. A redirect into a login
// or authwall page is reported as it is: the caller decides it is a block.
func (w *Worker) fetch(ctx context.Context, rawURL string) (fetched, error) {
	cur := rawURL
	for hop := 0; ; hop++ {
		if err := w.guard.CheckURL(ctx, cur); err != nil {
			return fetched{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
		if err != nil {
			return fetched{}, err
		}
		for k, v := range map[string]string{
			"User-Agent":                browserUA,
			"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
			"Accept-Language":           "en-US,en;q=0.9",
			"Sec-Fetch-Dest":            "document",
			"Sec-Fetch-Mode":            "navigate",
			"Sec-Fetch-Site":            "none",
			"Sec-Fetch-User":            "?1",
			"Upgrade-Insecure-Requests": "1",
		} {
			req.Header.Set(k, v)
		}
		res, err := w.http.Do(req)
		if err != nil {
			return fetched{}, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, maxFetchBody))
		res.Body.Close()
		if err != nil {
			return fetched{}, err
		}
		if res.StatusCode >= 300 && res.StatusCode < 400 && res.Header.Get("Location") != "" {
			next, err := res.Request.URL.Parse(res.Header.Get("Location"))
			if err != nil {
				return fetched{}, err
			}
			if isLoginURL(next) || hop >= maxRedirects {
				return fetched{Status: res.StatusCode, URL: next.String(), Body: string(body)}, nil
			}
			cur = next.String()
			continue
		}
		return fetched{Status: res.StatusCode, URL: cur, Body: string(body)}, nil
	}
}

var loginPath = regexp.MustCompile(`(?i)/(authwall|login|uas/|checkpoint|signup|sign-in|signin)`)

// isLoginURL reports a sign-in or sign-up wall's address.
func isLoginURL(u *url.URL) bool {
	return loginPath.MatchString(u.Path)
}

// blockedResponse reports whether an HTTP answer is the site refusing an
// anonymous reader: a throttled or blocked status, an unresolved redirect, or
// an address that is a login or authwall page.
func blockedResponse(status int, finalURL string) bool {
	switch {
	case status == 401 || status == 403 || status == 429 || status == 999 || status == 503:
		return true
	case status >= 300 && status < 400:
		return true
	}
	u, err := url.Parse(finalURL)
	return err == nil && isLoginURL(u)
}

// blockedText reports whether a rendered page's text is a login wall or bot
// challenge standing in for the posting. Those pages are short; a real posting
// that merely mentions signing in is not.
func blockedText(markdown string) bool {
	if len(markdown) > 3000 {
		return false
	}
	text := strings.ToLower(markdown)
	for _, marker := range []string{
		"authwall", "sign in to view", "join linkedin", "sign in or join", "just a moment",
		"verify you are human", "attention required", "access denied", "captcha", "unusual traffic",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// linkedinJobID extracts the posting ID of a LinkedIn job link: a
// /jobs/view/<slug>-<id> path, or the currentJobId of a search or collection
// page. It returns "" for any other URL.
func linkedinJobID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host != "linkedin.com" && !strings.HasSuffix(host, ".linkedin.com") {
		return ""
	}
	if m := viewPath.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	if id := u.Query().Get("currentJobId"); jobID.MatchString(id) {
		return id
	}
	return ""
}

var (
	viewPath = regexp.MustCompile(`^/(?:comm/)?jobs/view/(?:[^/?#]*-)?(\d{6,20})/?$`)
	jobID    = regexp.MustCompile(`^\d{6,20}$`)
)

var errBlocked = errors.New("blocked by the site")

// readLinkedIn reads a LinkedIn posting without a browser: the guest posting
// fragment LinkedIn serves to logged-out visitors, then the public view page
// for its structured data. errBlocked means the site wanted a login or
// refused this address, and a browser should try; any other error is final.
func (w *Worker) readLinkedIn(ctx context.Context, id string) (Result, error) {
	guest, err := w.fetch(ctx, w.linkedinBase+"/jobs-guest/jobs/api/jobPosting/"+id)
	if err != nil {
		return Result{}, err
	}
	switch {
	case guest.Status == 404 || guest.Status == 410:
		return Result{StatusCode: 404, Error: "the posting no longer exists"}, nil
	case guest.Status != 200 || blockedResponse(guest.Status, guest.URL) || !strings.Contains(guest.Body, "show-more-less-html") && !strings.Contains(guest.Body, "top-card-layout"):
		// Anything but the posting itself: a login wall, a throttle, or a
		// fragment without the posting in it.
		return Result{}, fmt.Errorf("%w: guest posting answered %d", errBlocked, guest.Status)
	}
	res := Result{Markdown: toMarkdown(guest.Body), RawHTML: truncate(guest.Body, maxHTML), StatusCode: 200, Metadata: metadata(guest.Body)}
	if title := textOfClass(guest.Body, "top-card-layout__title"); title != "" {
		if _, ok := res.Metadata["ogTitle"]; !ok {
			res.Metadata["ogTitle"] = title
		}
	}
	// The public page carries the JSON-LD the API reads fields from. It is a
	// bonus: when it is refused the guest fragment stands alone.
	if err := w.sleep(ctx, w.pause()/2); err != nil {
		return Result{}, err
	}
	view, err := w.fetch(ctx, w.linkedinBase+"/jobs/view/"+id)
	if err == nil && view.Status == 200 && !blockedResponse(view.Status, view.URL) {
		res.RawHTML = truncate(view.Body, maxHTML)
		for k, v := range metadata(view.Body) {
			res.Metadata[k] = v
		}
	}
	return res, nil
}
