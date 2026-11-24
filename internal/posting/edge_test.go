package posting

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/store"
)

// fakeQueue is an in-memory EdgeQueue whose job a test finishes by hand.
type fakeQueue struct {
	mu        sync.Mutex
	allow     bool
	enqueued  []string
	hosts     []string
	ttl       int
	outcome   store.ScrapeOutcome
	canceled  bool
	resultErr error
}

func (q *fakeQueue) EnqueueScrapeJob(_ context.Context, url, host string, ttl int) (uuid.UUID, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueued, q.hosts, q.ttl = append(q.enqueued, url), append(q.hosts, host), ttl
	return uuid.New(), q.allow, nil
}

func (q *fakeQueue) ScrapeJobResult(_ context.Context, _ uuid.UUID, cancel bool) (store.ScrapeOutcome, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.resultErr != nil {
		return store.ScrapeOutcome{}, q.resultErr
	}
	if cancel && (q.outcome.Status == "queued" || q.outcome.Status == "claimed") {
		q.canceled = true
		return store.ScrapeOutcome{Status: "expired"}, nil
	}
	return q.outcome, nil
}

func (q *fakeQueue) finish(o store.ScrapeOutcome) {
	q.mu.Lock()
	q.outcome = o
	q.mu.Unlock()
}

func newEdge(q *fakeQueue) *Edge {
	return &Edge{queue: q, wait: 400 * time.Millisecond, poll: 10 * time.Millisecond}
}

func intp(n int) *int       { return &n }
func strp(s string) *string { return &s }

func TestEdgeScrapeReturnsTheWorkersPage(t *testing.T) {
	q := &fakeQueue{allow: true, outcome: store.ScrapeOutcome{Status: "queued"}}
	e := newEdge(q)
	go func() {
		time.Sleep(50 * time.Millisecond)
		q.finish(store.ScrapeOutcome{Status: "done", StatusCode: intp(200), Markdown: "# Job", RawHTML: "<h1>Job</h1>", Metadata: map[string]any{"ogTitle": "Job"}})
	}()
	page, err := e.Scrape(t.Context(), "https://WWW.LinkedIn.com./jobs/view/1")
	if err != nil {
		t.Fatal(err)
	}
	if page.Markdown != "# Job" || page.RawHTML != "<h1>Job</h1>" || page.Metadata["ogTitle"] != "Job" || page.Metadata["statusCode"] != float64(200) {
		t.Errorf("page = %+v", page)
	}
	if q.hosts[0] != "www.linkedin.com" || q.ttl != int((400*time.Millisecond+edgeSlack)/time.Second) {
		t.Errorf("enqueued host %q ttl %d", q.hosts[0], q.ttl)
	}
}

func TestEdgeScrapeWithoutAWorkerFailsAtOnce(t *testing.T) {
	q := &fakeQueue{allow: false}
	start := time.Now()
	_, err := newEdge(q).Scrape(t.Context(), "https://example.com/job")
	var unreadable *UnreadableError
	if err == nil || errors.As(err, &unreadable) || time.Since(start) > 200*time.Millisecond {
		t.Errorf("err = %v after %v, want an immediate plain error", err, time.Since(start))
	}
}

func TestEdgeScrapeTimeoutIsAPlainErrorAndExpiresTheJob(t *testing.T) {
	q := &fakeQueue{allow: true, outcome: store.ScrapeOutcome{Status: "queued"}}
	_, err := newEdge(q).Scrape(t.Context(), "https://example.com/job")
	var unreadable *UnreadableError
	if err == nil || errors.As(err, &unreadable) {
		t.Fatalf("err = %v, want a plain error so the chain falls back", err)
	}
	if !q.canceled {
		t.Error("the job was left queued")
	}
}

// An answer that lands between the last poll and the deadline is used.
func TestEdgeScrapeTakesAnAnswerAtTheDeadline(t *testing.T) {
	q := &fakeQueue{allow: true, outcome: store.ScrapeOutcome{Status: "queued"}}
	e := newEdge(q)
	e.poll = time.Hour
	go func() {
		time.Sleep(100 * time.Millisecond)
		q.finish(store.ScrapeOutcome{Status: "done", Markdown: "late but in time"})
	}()
	page, err := e.Scrape(t.Context(), "https://example.com/job")
	if err != nil || page.Markdown != "late but in time" {
		t.Errorf("page = %+v, err = %v", page, err)
	}
}

func TestEdgeScrapeMapsWorkerFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		out        store.ScrapeOutcome
		unreadable string // the reason, or "" for a plain error
	}{
		"blocked page":    {store.ScrapeOutcome{Status: "failed", StatusCode: intp(999), Error: strp("login wall")}, "login wall"},
		"not found":       {store.ScrapeOutcome{Status: "failed", StatusCode: intp(404)}, "the site answered 404"},
		"control chars":   {store.ScrapeOutcome{Status: "failed", StatusCode: intp(403), Error: strp("blocked\x1b[31m\n")}, "blocked [31m"},
		"worker crashed":  {store.ScrapeOutcome{Status: "failed", Error: strp("chrome exited")}, ""},
		"failed no cause": {store.ScrapeOutcome{Status: "failed"}, ""},
		"empty page":      {store.ScrapeOutcome{Status: "done", StatusCode: intp(200), Markdown: " \n"}, "the page has no readable content"},
		"expired":         {store.ScrapeOutcome{Status: "expired"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			q := &fakeQueue{allow: true, outcome: tc.out}
			_, err := newEdge(q).Scrape(t.Context(), "https://example.com/job")
			var unreadable *UnreadableError
			switch {
			case err == nil:
				t.Fatal("no error")
			case tc.unreadable == "" && errors.As(err, &unreadable):
				t.Errorf("err = %v, want a plain error", err)
			case tc.unreadable != "" && (!errors.As(err, &unreadable) || unreadable.Reason != tc.unreadable):
				t.Errorf("err = %v, want unreadable %q", err, tc.unreadable)
			}
		})
	}
}

func TestEdgeScrapeStopsWhenTheCallerGivesUp(t *testing.T) {
	q := &fakeQueue{allow: true, outcome: store.ScrapeOutcome{Status: "queued"}}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if _, err := newEdge(q).Scrape(ctx, "https://example.com/job"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if !q.canceled {
		t.Error("the job was left queued")
	}
}

func TestEdgeSettings(t *testing.T) {
	ok := Settings{Providers: []ProviderConfig{{Kind: KindEdge}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("edge provider: %v", err)
	}
	for _, p := range []ProviderConfig{{Kind: KindEdge, BaseURL: "http://x:1"}, {Kind: KindEdge, KeyID: uuid.NewString()}} {
		if err := (Settings{Providers: []ProviderConfig{p}}).Validate(); err == nil {
			t.Errorf("%+v accepted", p)
		}
	}
}
