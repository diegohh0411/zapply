package edgeworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// API is the zapply server as the worker uses it; *Client implements it.
type API interface {
	Claim(ctx context.Context) (*Job, error)
	PostResult(ctx context.Context, id string, r Result) error
}

// Rendered is a page a browser loaded.
type Rendered struct {
	HTML   string
	URL    string // after redirects
	Status int    // the document's HTTP status; 0 when unknown
}

// Renderer loads a page in a browser. Every call must start from nothing: no
// cookies, storage or cache from an earlier page.
type Renderer interface {
	Render(ctx context.Context, url string) (Rendered, error)
}

const (
	// jobTimeout bounds one job, a little under what the API waits for it.
	jobTimeout = 40 * time.Second

	backoffBase = time.Second
	backoffMax  = time.Minute
	// authBackoffMin is the slowest retry once the server refuses the token:
	// retrying fast cannot help, and the owner may be rotating it.
	authBackoffMin = 30 * time.Second
	authBackoffMax = 5 * time.Minute
)

// Worker claims scrape jobs and reads them, one at a time.
type Worker struct {
	api    API
	guard  *Guard
	http   *http.Client
	render Renderer
	log    *slog.Logger

	// linkedinBase is replaced in tests.
	linkedinBase string
	// minDelay and maxDelay bound the random pause after each job, so a worker
	// never hits a site in a burst.
	minDelay, maxDelay time.Duration
	// sleepFor is replaced in tests; it returns early with ctx's error.
	sleepFor func(ctx context.Context, d time.Duration) error
}

// Options configure a Worker.
type Options struct {
	Guard    *Guard
	Renderer Renderer // nil: pages that need a browser fail
	Log      *slog.Logger
	// MinDelay and MaxDelay bound the pause after each job (default 2s to 6s).
	MinDelay, MaxDelay time.Duration
}

// New returns a worker over api.
func New(api API, o Options) *Worker {
	g := o.Guard
	if g == nil {
		g = NewGuard()
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	w := &Worker{api: api, guard: g, http: newHTTPClient(g), render: o.Renderer, log: log,
		linkedinBase: "https://www.linkedin.com", minDelay: o.MinDelay, maxDelay: o.MaxDelay, sleepFor: sleep}
	if w.minDelay == 0 && w.maxDelay == 0 {
		w.minDelay, w.maxDelay = 2*time.Second, 6*time.Second
	}
	return w
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) error { return w.sleepFor(ctx, d) }

// pause is a random duration between the configured bounds.
func (w *Worker) pause() time.Duration {
	if w.maxDelay <= w.minDelay {
		return w.minDelay
	}
	return w.minDelay + time.Duration(rand.Int64N(int64(w.maxDelay-w.minDelay)))
}

// backoff is the wait after n consecutive failures: exponential, capped, with
// up to 20% jitter so workers do not retry in step.
func backoff(n int, min, max time.Duration) time.Duration {
	d := min
	for i := 1; i < n && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}

// Run claims and reads jobs until ctx is cancelled. A job in progress when
// ctx is cancelled is finished and posted first.
func (w *Worker) Run(ctx context.Context) error {
	failures := 0
	for ctx.Err() == nil {
		start := time.Now()
		job, err := w.api.Claim(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			failures++
			wait := backoff(failures, backoffBase, backoffMax)
			if errors.Is(err, ErrUnauthorized) {
				wait = backoff(failures, authBackoffMin, authBackoffMax)
			}
			w.log.Error("claim failed", "error", err, "retry_in", wait.Round(time.Millisecond))
			_ = w.sleep(ctx, wait)
			continue
		}
		failures = 0
		if job == nil {
			// The server holds a claim open; one that returns at once (it is
			// draining) should not become a busy loop.
			if time.Since(start) < 500*time.Millisecond {
				_ = w.sleep(ctx, time.Second)
			}
			continue
		}
		w.process(ctx, *job)
		_ = w.sleep(ctx, w.pause())
	}
	return nil
}

// process reads one job and posts the result. It ignores ctx's cancellation:
// shutdown waits for the job in hand rather than dropping it.
func (w *Worker) process(ctx context.Context, job Job) {
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobTimeout)
	defer cancel()
	started := time.Now()
	res := w.Handle(jctx, job)
	w.log.Info("job read", "job", job.ID, "host", hostOf(job.URL), "status_code", res.StatusCode, "error", res.Error, "took", time.Since(started).Round(time.Millisecond))
	for attempt := 1; ; attempt++ {
		err := w.api.PostResult(jctx, job.ID, res)
		switch {
		case err == nil:
			return
		case errors.Is(err, ErrJobGone):
			w.log.Warn("result not accepted: the job expired or is not ours", "job", job.ID)
			return
		case errors.Is(err, ErrUnauthorized) || attempt >= 3 || jctx.Err() != nil:
			w.log.Error("result not delivered", "job", job.ID, "error", err)
			return
		}
		w.log.Warn("result delivery failed; retrying", "job", job.ID, "error", err)
		_ = w.sleep(context.WithoutCancel(ctx), backoff(attempt, backoffBase, backoffMax))
	}
}

func hostOf(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// Handle reads one job's page. It never returns an error: what went wrong is
// the result. A result with a status code means the site refused or lacks the
// page; one with only an error means the worker could not read it.
func (w *Worker) Handle(ctx context.Context, job Job) Result {
	if err := w.guard.CheckURL(ctx, job.URL); err != nil {
		return Result{Error: truncate("refused: "+err.Error(), 400)}
	}
	if id := linkedinJobID(job.URL); id != "" {
		res, err := w.readLinkedIn(ctx, id)
		switch {
		case err == nil:
			return res
		case errors.Is(err, ErrRefused):
			return Result{Error: truncate("refused: "+err.Error(), 400)}
		}
		w.log.Info("falling back to the browser", "job", job.ID, "reason", err)
	}
	return w.viaBrowser(ctx, job.URL)
}

func (w *Worker) viaBrowser(ctx context.Context, rawURL string) Result {
	if w.render == nil {
		return Result{Error: "this worker has no browser"}
	}
	page, err := w.render.Render(ctx, rawURL)
	if err != nil {
		if errors.Is(err, ErrRefused) {
			return Result{Error: truncate("refused: "+err.Error(), 400)}
		}
		return Result{Error: truncate(fmt.Sprintf("browser: %v", err), 400)}
	}
	md := toMarkdown(page.HTML)
	switch {
	case page.Status >= 400 && page.Status != 999:
		return Result{StatusCode: page.Status, Error: fmt.Sprintf("the site answered %d", page.Status)}
	case page.Status == 999 || blockedResponse(page.Status, page.URL) || blockedText(md):
		return Result{StatusCode: 401, Error: "the site asked for a login or a human check"}
	}
	meta := metadata(page.HTML)
	if meta == nil {
		meta = map[string]any{}
	}
	status := page.Status
	if status == 0 {
		status = 200
	}
	return Result{Markdown: md, RawHTML: truncate(page.HTML, maxHTML), Metadata: meta, StatusCode: status}
}
