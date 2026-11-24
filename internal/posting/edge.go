package posting

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/store"
)

const (
	// edgeWait is how long Scrape waits for a worker to read the page.
	edgeWait = 45 * time.Second
	// edgePoll is how often the job row is read while waiting.
	edgePoll = 500 * time.Millisecond
	// edgeSlack keeps a job claimable a little past what the caller waits, so
	// the worker's last seconds are not wasted; the caller expires it anyway.
	edgeSlack = 15 * time.Second
)

// EdgeQueue is the job queue edge workers pull from. It lives in Postgres, so
// any API replica can enqueue a job and any other can serve the worker.
type EdgeQueue interface {
	// EnqueueScrapeJob reports ok=false when no live worker may open host.
	EnqueueScrapeJob(ctx context.Context, url, host string, ttlSeconds int) (id uuid.UUID, ok bool, err error)
	// ScrapeJobResult reads a job; a finished job's result is returned once.
	// With cancel, a job still pending is expired.
	ScrapeJobResult(ctx context.Context, id uuid.UUID, cancel bool) (store.ScrapeOutcome, error)
}

// Edge reads pages through edge workers: browsers on residential machines that
// pull jobs from the API, for sites that block datacenter addresses.
type Edge struct {
	queue EdgeQueue
	wait  time.Duration
	poll  time.Duration
}

// NewEdge returns a scraper over the edge job queue.
func NewEdge(q EdgeQueue) *Edge { return &Edge{queue: q, wait: edgeWait, poll: edgePoll} }

// Scrape queues url and waits for a worker's answer. When no worker may open
// the host, none claims the job in time or one fails internally, the error is
// plain, so the chain falls back to the next provider. A worker that reached
// the site but was refused (a block page, a 404) makes the page unreadable.
func (e *Edge) Scrape(ctx context.Context, rawURL string) (Page, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return Page{}, ErrInvalidURL
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	id, ok, err := e.queue.EnqueueScrapeJob(ctx, rawURL, host, int((e.wait+edgeSlack)/time.Second))
	if err != nil {
		return Page{}, fmt.Errorf("edge: queue job: %w", err)
	}
	if !ok {
		return Page{}, errors.New("edge: no worker is available for " + host)
	}
	deadline := time.NewTimer(e.wait)
	defer deadline.Stop()
	tick := time.NewTicker(e.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			e.cancel(id)
			return Page{}, ctx.Err()
		case <-deadline.C:
			// A worker may have answered since the last poll: cancel returns
			// that result instead of discarding it.
			out, err := e.queue.ScrapeJobResult(context.WithoutCancel(ctx), id, true)
			if err != nil {
				return Page{}, fmt.Errorf("edge: %w", err)
			}
			if out.Status == "done" || out.Status == "failed" {
				return edgePage(out)
			}
			return Page{}, errors.New("edge: no worker answered in time")
		case <-tick.C:
			out, err := e.queue.ScrapeJobResult(ctx, id, false)
			if err != nil {
				if ctx.Err() != nil {
					e.cancel(id)
					return Page{}, ctx.Err()
				}
				return Page{}, fmt.Errorf("edge: %w", err)
			}
			switch out.Status {
			case "done", "failed":
				return edgePage(out)
			case "expired":
				return Page{}, errors.New("edge: no worker answered in time")
			}
		}
	}
}

// cancel expires a job nobody waits for any more, so its result is not kept.
func (e *Edge) cancel(id uuid.UUID) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_, _ = e.queue.ScrapeJobResult(ctx, id, true)
}

func edgePage(out store.ScrapeOutcome) (Page, error) {
	status := 0
	if out.StatusCode != nil {
		status = *out.StatusCode
	}
	if out.Status == "failed" {
		msg := ""
		if out.Error != nil {
			msg = cleanReason(*out.Error)
		}
		switch {
		case status >= 400 && msg != "":
			return Page{}, &UnreadableError{Reason: msg}
		case status >= 400:
			return Page{}, &UnreadableError{Reason: fmt.Sprintf("the site answered %d", status)}
		case msg == "":
			msg = "the worker failed"
		}
		return Page{}, errors.New("edge: " + msg)
	}
	if strings.TrimSpace(out.Markdown) == "" && strings.TrimSpace(out.RawHTML) == "" {
		return Page{}, &UnreadableError{Reason: "the page has no readable content"}
	}
	meta := make(map[string]any, len(out.Metadata)+1)
	for k, v := range out.Metadata {
		meta[k] = v
	}
	if status > 0 {
		meta["statusCode"] = float64(status)
	}
	return Page{Markdown: out.Markdown, RawHTML: out.RawHTML, Metadata: meta}, nil
}

// cleanReason shortens worker-supplied text and drops control characters.
func cleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), 200)
}
