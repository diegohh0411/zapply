package edgeworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	// renderTimeout bounds one page load, inside the job's own timeout.
	renderTimeout = 30 * time.Second
	// settle is how long a page may run its scripts after the body is ready.
	settle = 1500 * time.Millisecond
	// maxRenderedHTML bounds what is read back from the browser.
	maxRenderedHTML = 8 << 20
)

// Chrome renders pages in headless Chrome. One browser process serves every
// job, but each page gets a browser context of its own, like a fresh incognito
// window: no cookies, storage or cache carry over between jobs, and nothing is
// ever logged in.
type Chrome struct {
	path      string
	noSandbox bool
	guard     *Guard
	log       *slog.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	browser context.Context
	profile string
	proxy   *guardedProxy
}

// NewChrome returns a renderer. path is the Chrome or Chromium binary ("" finds
// one on PATH). noSandbox turns off Chrome's own sandbox, which a container
// that forbids user namespaces needs; the container then is the sandbox.
func NewChrome(path string, noSandbox bool, g *Guard, log *slog.Logger) *Chrome {
	if log == nil {
		log = slog.Default()
	}
	return &Chrome{path: path, noSandbox: noSandbox, guard: g, log: log}
}

// start returns the running browser, launching it (again) when needed.
func (c *Chrome) start() (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.browser != nil && c.browser.Err() == nil {
		return c.browser, nil
	}
	c.stopLocked()
	proxy, err := startGuardedProxy(c.guard, c.log)
	if err != nil {
		return nil, fmt.Errorf("start guarded proxy: %w", err)
	}
	profile, err := os.MkdirTemp("", "zapply-edge-chrome-")
	if err != nil {
		proxy.Close()
		return nil, err
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.UserDataDir(profile),
		chromedp.WindowSize(1366, 768),
		chromedp.UserAgent(browserUA),
		chromedp.Flag("lang", "en-US"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		// Every connection leaves through the guarded proxy, which checks the
		// address actually dialled (DNS rebinding, WebSockets). <-loopback>
		// stops Chrome from bypassing it for localhost.
		chromedp.ProxyServer(proxy.URL()),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		// WebRTC's UDP would skip the proxy; only proxied traffic is allowed.
		chromedp.Flag("force-webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		chromedp.Flag("webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		chromedp.WSURLReadTimeout(30*time.Second),
	)
	if c.path != "" {
		opts = append(opts, chromedp.ExecPath(c.path))
	}
	if c.noSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	browser, cancelBrowser := chromedp.NewContext(alloc)
	// The first call starts the browser, bound to this context (which has no
	// timeout, so it lives until Close).
	if err := chromedp.Do(browser); err != nil {
		cancelBrowser()
		cancelAlloc()
		_ = os.RemoveAll(profile)
		proxy.Close()
		return nil, fmt.Errorf("start Chrome: %w", err)
	}
	c.browser, c.profile, c.proxy = browser, profile, proxy
	c.cancel = func() { cancelBrowser(); cancelAlloc() }
	return browser, nil
}

func (c *Chrome) stopLocked() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.profile != "" {
		_ = os.RemoveAll(c.profile)
	}
	if c.proxy != nil {
		c.proxy.Close()
	}
	c.cancel, c.browser, c.profile, c.proxy = nil, nil, "", nil
}

// Close stops the browser.
func (c *Chrome) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

// Render loads rawURL in a new browser context and returns the page after its
// scripts have had a moment to run. Every request the page makes, including
// redirects and sub-resources, is checked by the guard before it is sent, and
// every connection is dialled through the guarded proxy, which checks the
// address really connected to.
func (c *Chrome) Render(ctx context.Context, rawURL string) (Rendered, error) {
	browser, err := c.start()
	if err != nil {
		return Rendered{}, err
	}
	jobCtx, cancelJob := context.WithTimeout(browser, renderTimeout)
	defer cancelJob()
	stop := context.AfterFunc(ctx, cancelJob)
	defer stop()
	tab, cancelTab := chromedp.NewContext(jobCtx, chromedp.WithNewBrowserContext())
	defer cancelTab()

	var (
		mu      sync.Mutex
		refused error
		hosts   = map[string]bool{} // host -> allowed, so a page's many requests resolve once
	)
	check := func(reqURL string) error {
		u, err := url.Parse(reqURL)
		if err != nil {
			return err
		}
		switch u.Scheme {
		case "data", "blob", "about":
			return nil
		}
		mu.Lock()
		ok, seen := hosts[u.Host]
		mu.Unlock()
		if seen && ok {
			return nil
		}
		lookup, cancel := context.WithTimeout(jobCtx, 5*time.Second)
		defer cancel()
		err = c.guard.CheckURL(lookup, reqURL)
		mu.Lock()
		hosts[u.Host] = err == nil
		if err != nil && errors.Is(err, ErrRefused) && refused == nil {
			refused = err
		}
		mu.Unlock()
		return err
	}
	// Every request the page makes is held until it passes the guard. The
	// listener runs in the browser's message loop, so answering happens in a
	// goroutine.
	if err := chromedp.Listen(tab, func(ev any) {
		e, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		go func() {
			if err := check(e.Request.URL); err != nil {
				_, _ = chromedp.Call(tab, fetch.FailRequest, fetch.FailRequestParams{RequestID: e.RequestID, ErrorReason: network.ErrorReasonBlockedByClient})
				return
			}
			_, _ = chromedp.Call(tab, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: e.RequestID})
		}()
	}); err != nil {
		return Rendered{}, err
	}

	page, err := c.load(tab, rawURL)
	mu.Lock()
	refusedErr := refused
	mu.Unlock()
	switch {
	case refusedErr != nil && err != nil:
		return Rendered{}, refusedErr
	case ctx.Err() != nil:
		return Rendered{}, ctx.Err()
	case err != nil:
		return Rendered{}, err
	}
	return page, nil
}

// load navigates the tab and reads the page back.
func (c *Chrome) load(tab context.Context, rawURL string) (Rendered, error) {
	if _, err := chromedp.Call(tab, network.Enable, network.EnableParams{}); err != nil {
		return Rendered{}, err
	}
	if _, err := chromedp.Call(tab, fetch.Enable, fetch.EnableParams{}); err != nil {
		return Rendered{}, err
	}
	if _, err := chromedp.Call(tab, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{Width: 1366, Height: 768, DeviceScaleFactor: 1}); err != nil {
		return Rendered{}, err
	}
	if _, err := chromedp.Call(tab, emulation.SetUserAgentOverride, emulation.SetUserAgentOverrideParams{UserAgent: browserUA, AcceptLanguage: "en-US,en;q=0.9", Platform: "MacIntel"}); err != nil {
		return Rendered{}, err
	}
	resp, err := chromedp.Run(tab, chromedp.NavigateResponse(rawURL))
	if err != nil {
		return Rendered{}, err
	}
	status := 0
	if resp != nil {
		status = int(resp.Status)
	}
	// Scripts get a moment to fill in the page.
	if err := chromedp.Do(tab, chromedp.WaitReady(chromedp.CSS("body")), chromedp.Sleep(settle)); err != nil {
		return Rendered{}, err
	}
	loc, err := chromedp.Run(tab, chromedp.Location())
	if err != nil {
		return Rendered{}, err
	}
	html, err := chromedp.Run(tab, chromedp.OuterHTML(chromedp.CSS("html")))
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{HTML: truncate(html, maxRenderedHTML), URL: loc, Status: status}, nil
}

var _ Renderer = (*Chrome)(nil)
