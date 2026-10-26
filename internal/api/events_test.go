package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/events"
	"github.com/diegohh0411/zapply/internal/relay"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/testnats"
)

type eventEnv struct {
	t     *testing.T
	db    *testdb.DB
	nats  *testnats.Server
	url   string
	http  *http.Client
	drain chan struct{}
}

type eventOpts struct {
	heartbeat  time.Duration
	maxStreams int
	noHub      bool
	noRelay    bool
	http2      bool // serve TLS with HTTP/2
	writeTO    time.Duration
}

func newEventEnv(t *testing.T, o eventOpts) *eventEnv {
	t.Helper()
	d := testdb.New(t)
	ns := testnats.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &eventEnv{t: t, db: d, nats: ns, drain: make(chan struct{})}

	deps := api.Deps{
		Store:        d.Store,
		Verifier:     auth.NewDev(secret, "authenticated"),
		Logger:       log,
		Drain:        e.drain,
		Heartbeat:    o.heartbeat,
		WriteTimeout: o.writeTO,
	}
	if !o.noHub {
		hub, err := events.Connect(ns.ClientURL(), events.HubConfig{MaxStreamsPerUser: o.maxStreams}, log)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(hub.Close)
		deps.Events = hub
	}
	srv := httptest.NewUnstartedServer(api.Handler(deps))
	if o.http2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	e.url, e.http = srv.URL, srv.Client()

	if !o.noRelay {
		pool, err := store.Connect(context.Background(), d.RelayURL)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = relay.New(pool, ns.Connect(t), relay.Config{Interval: 10 * time.Millisecond}, log).Run(ctx)
		}()
		t.Cleanup(func() { cancel(); <-done; pool.Close() })
	}
	return e
}

// published waits until the relay has published every outbox row.
func (e *eventEnv) published() {
	e.t.Helper()
	admin := e.db.Admin(e.t)
	for deadline := time.Now().Add(5 * time.Second); ; {
		var n int
		if err := admin.QueryRow(context.Background(),
			`SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%d events still unpublished", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *eventEnv) client(sub string, ttl time.Duration) *client {
	tok, err := auth.MintDev(secret, "authenticated", sub, ttl)
	if err != nil {
		e.t.Fatal(err)
	}
	return &client{t: e.t, base: e.url, token: tok, http: e.http}
}

type sseEvent struct {
	ID, Event, Data, Retry, Comment string
}

type sseStream struct {
	t   *testing.T
	res *http.Response
	ch  chan sseEvent
}

// open starts GET /v1/events. A non-200 response is returned with no events.
func open(t *testing.T, c *client, lastID string) *sseStream {
	t.Helper()
	req, _ := http.NewRequest("GET", c.base+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	hc := c.http
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &sseStream{t: t, res: res, ch: make(chan sseEvent, 100)}
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		close(s.ch)
		return s
	}
	go func() {
		defer close(s.ch)
		sc := bufio.NewScanner(res.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			k, v, _ := strings.Cut(line, ": ")
			switch {
			case line == "":
				if ev != (sseEvent{}) {
					s.ch <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
				s.ch <- sseEvent{Comment: strings.TrimPrefix(line, ": ")}
			case k == "id":
				ev.ID = v
			case k == "event":
				ev.Event = v
			case k == "data":
				ev.Data = v
			case k == "retry":
				ev.Retry = v
			}
		}
	}()
	return s
}

// next returns the next event, skipping heartbeats.
func (s *sseStream) next() sseEvent {
	s.t.Helper()
	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				s.t.Fatal("stream ended")
			}
			if ev.Comment == "" {
				return ev
			}
		case <-time.After(5 * time.Second):
			s.t.Fatal("no event within 5s")
		}
	}
}

func (s *sseStream) expect(name string) sseEvent {
	s.t.Helper()
	ev := s.next()
	if ev.Event != name {
		s.t.Fatalf("got event %+v, want %q", ev, name)
	}
	return ev
}

// quiet asserts no event arrives for a while.
func (s *sseStream) quiet(d time.Duration) {
	s.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				s.t.Fatal("stream ended")
			}
			if ev.Comment == "" {
				s.t.Fatalf("unexpected event %+v", ev)
			}
		case <-deadline:
			return
		}
	}
}

// ended waits for the server to close the stream and returns the last
// event it sent.
func (s *sseStream) ended(within time.Duration) sseEvent {
	s.t.Helper()
	var last sseEvent
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				return last
			}
			last = ev
		case <-deadline:
			s.t.Fatalf("stream still open after %v", within)
		}
	}
}

func newEvidence(c *client, title string) string {
	c.t.Helper()
	res := c.do("POST", "/v1/evidences", map[string]any{
		"category": "unpaid", "kind": "project", "title": title, "start": "2024",
	})
	if res.Status != http.StatusCreated {
		c.t.Fatalf("create evidence: %d %v", res.Status, res.Body)
	}
	return res.Body["id"].(string)
}

func dataID(t *testing.T, ev sseEvent) string {
	t.Helper()
	var d struct{ ID string }
	if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
		t.Fatalf("data %q: %v", ev.Data, err)
	}
	return d.ID
}

func TestEventsLiveThenResume(t *testing.T) {
	e := newEventEnv(t, eventOpts{})
	alice := e.client("alice", time.Hour)

	s := open(t, alice, "")
	if s.res.StatusCode != http.StatusOK || s.res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content-type %q", s.res.StatusCode, s.res.Header.Get("Content-Type"))
	}
	s.expect("ready")

	first := newEvidence(alice, "First")
	ev := s.expect("evidence.created")
	if dataID(t, ev) != first || ev.ID == "" {
		t.Fatalf("event %+v, want evidence %s", ev, first)
	}
	s.res.Body.Close()

	// Missed while disconnected, then replayed on resume.
	second := newEvidence(alice, "Second")
	third := newEvidence(alice, "Third")
	e.published()
	s = open(t, alice, ev.ID)
	a, b := s.expect("evidence.created"), s.expect("evidence.created")
	if dataID(t, a) != second || dataID(t, b) != third {
		t.Fatalf("replayed %s, %s; want %s, %s", dataID(t, a), dataID(t, b), second, third)
	}
	ready := s.expect("ready")
	if ready.ID != b.ID {
		t.Fatalf("ready id = %q, want %q", ready.ID, b.ID)
	}
	if n, _ := strconv.ParseInt(b.ID, 10, 64); n <= mustInt(t, a.ID) {
		t.Fatalf("ids not increasing: %s then %s", a.ID, b.ID)
	}

	// Live again after replay, with nothing replayed twice.
	fourth := newEvidence(alice, "Fourth")
	if got := dataID(t, s.expect("evidence.created")); got != fourth {
		t.Fatalf("live event for %s, want %s", got, fourth)
	}
	s.quiet(200 * time.Millisecond)
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEventsAreTenantScoped(t *testing.T) {
	e := newEventEnv(t, eventOpts{})
	alice, bob := e.client("alice", time.Hour), e.client("bob", time.Hour)
	as, bs := open(t, alice, ""), open(t, bob, "")
	as.expect("ready")
	bs.expect("ready")

	newEvidence(bob, "Bob's")
	bobEv := bs.expect("evidence.created")
	as.quiet(300 * time.Millisecond)

	// Bob's event id is not a valid cursor for Alice: no replay of his rows.
	s := open(t, alice, bobEv.ID)
	s.expect("reset")
	s.expect("ready")
	s.quiet(200 * time.Millisecond)
}

func TestEventsResetOnUnknownCursor(t *testing.T) {
	e := newEventEnv(t, eventOpts{})
	alice := e.client("alice", time.Hour)
	newEvidence(alice, "One")
	e.published()

	s := open(t, alice, "999999999")
	s.expect("reset")
	if ready := s.expect("ready"); ready.ID == "" {
		t.Fatal("ready after reset should carry the current position")
	}
}

// The relay publishes before it commits. A stream that subscribes after the
// publish and replays before the commit must still get the event: replay
// waits for the relay's lock.
func TestReplayWaitsForInFlightRelayBatch(t *testing.T) {
	e := newEventEnv(t, eventOpts{noRelay: true})
	ctx := context.Background()
	alice := e.client("alice", time.Hour)
	uid := userID(t, alice)
	admin := e.db.Admin(t)
	nc := e.nats.Connect(t)

	// A cursor event, already published.
	var cursor int64
	if err := admin.QueryRow(ctx, `
		INSERT INTO outbox (id, user_id, topic, payload, seq, published_at)
		VALUES ($1, $2, 'evidence.created', '{}', nextval('outbox_seq'), now()) RETURNING seq`,
		uuid.Must(uuid.NewV7()), uid).Scan(&cursor); err != nil {
		t.Fatal(err)
	}

	// Act as the relay: lock, publish to NATS, and hold the commit.
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, events.RelayLockKey); err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	payload, _ := json.Marshal(map[string]any{"id": id})
	var seq int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO outbox (id, user_id, topic, payload, seq, published_at)
		VALUES ($1, $2, 'evidence.updated', $3, nextval('outbox_seq'), now()) RETURNING seq`,
		id, uid, payload).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	msg, _ := json.Marshal(events.Event{Seq: seq, Topic: "evidence.updated", Data: payload})
	if err := nc.Publish(events.Subject(uid, "evidence.updated"), msg); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	var s *sseStream
	opened := make(chan struct{})
	go func() {
		s = open(t, alice, strconv.FormatInt(cursor, 10))
		close(opened)
	}()
	select {
	case <-opened:
		t.Fatal("replay did not wait for the in-flight batch")
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-opened

	ev := s.expect("evidence.updated")
	if dataID(t, ev) != id.String() || ev.ID != strconv.FormatInt(seq, 10) {
		t.Fatalf("event %+v, want %s at seq %d", ev, id, seq)
	}
	s.expect("ready")
}

func userID(t *testing.T, c *client) uuid.UUID {
	t.Helper()
	res := c.do("GET", "/v1/me", nil)
	id, err := uuid.Parse(res.Body["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEventsHeartbeat(t *testing.T) {
	e := newEventEnv(t, eventOpts{heartbeat: 30 * time.Millisecond})
	s := open(t, e.client("alice", time.Hour), "")
	s.expect("ready")
	select {
	case ev := <-s.ch:
		if ev.Comment != "ping" {
			t.Fatalf("got %+v, want a ping", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat")
	}
}

func TestEventsEndOnDrain(t *testing.T) {
	e := newEventEnv(t, eventOpts{})
	streams := []*sseStream{open(t, e.client("alice", time.Hour), ""), open(t, e.client("bob", time.Hour), "")}
	for _, s := range streams {
		s.expect("ready")
	}
	close(e.drain)
	var wg sync.WaitGroup
	for _, s := range streams {
		wg.Go(func() {
			if last := s.ended(2 * time.Second); last.Retry != "1000" {
				t.Errorf("last message %+v, want retry: 1000", last)
			}
		})
	}
	wg.Wait()
}

func TestEventsEndAtTokenExpiry(t *testing.T) {
	e := newEventEnv(t, eventOpts{})
	s := open(t, e.client("alice", 2*time.Second), "")
	s.expect("ready")
	s.ended(4 * time.Second)
}

func TestEventsRequestErrors(t *testing.T) {
	e := newEventEnv(t, eventOpts{maxStreams: 1, noRelay: true})
	alice := e.client("alice", time.Hour)

	if s := open(t, alice, "abc"); s.res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad Last-Event-ID: %d, want 400", s.res.StatusCode)
	}
	first := open(t, alice, "")
	first.expect("ready")
	if s := open(t, alice, ""); s.res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second stream: %d, want 429", s.res.StatusCode)
	}
	// Bob has his own allowance.
	open(t, e.client("bob", time.Hour), "").expect("ready")

	req, _ := http.NewRequest("GET", e.url+"/v1/events", nil)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %v %v, want 401", res.StatusCode, err)
	}

	first.res.Body.Close()
	// The slot frees once the handler notices the client left.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := open(t, alice, "")
		if s.res.StatusCode == http.StatusOK {
			break
		}
		s.res.Body.Close()
		if time.Now().After(deadline) {
			t.Fatalf("slot not freed: %d", s.res.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestEventsUnavailableWithoutNATS(t *testing.T) {
	e := newEventEnv(t, eventOpts{noHub: true, noRelay: true})
	alice := e.client("alice", time.Hour)
	s := open(t, alice, "")
	if s.res.StatusCode != http.StatusServiceUnavailable || s.res.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q; want 503 with Retry-After", s.res.StatusCode, s.res.Header.Get("Retry-After"))
	}
	if res := alice.do("GET", "/v1/me", nil); res.Status != http.StatusOK {
		t.Fatalf("REST affected: /v1/me = %d", res.Status)
	}
}

func TestEventsEndWhenNATSDisconnects(t *testing.T) {
	e := newEventEnv(t, eventOpts{noRelay: true})
	s := open(t, e.client("alice", time.Hour), "")
	s.expect("ready")
	e.nats.Shutdown()
	s.ended(3 * time.Second)
}

// holdRelayLock takes the relay lock as if a batch were in flight, until the
// returned function commits.
func (e *eventEnv) holdRelayLock() func() {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.db.Admin(e.t).Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, events.RelayLockKey); err != nil {
		e.t.Fatal(err)
	}
	return func() { _ = tx.Commit(ctx) }
}

func (e *eventEnv) cursor(c *client) string {
	e.t.Helper()
	newEvidence(c, "Cursor")
	e.published()
	var seq int64
	if err := e.db.Admin(e.t).QueryRow(context.Background(), `SELECT max(seq) FROM outbox`).Scan(&seq); err != nil {
		e.t.Fatal(err)
	}
	return strconv.FormatInt(seq, 10)
}

// A token that expires while replay waits for the relay lock gets no data.
func TestEventsTokenExpiresDuringReplay(t *testing.T) {
	e := newEventEnv(t, eventOpts{})
	alice := e.client("alice", 2*time.Second)
	cursor := e.cursor(alice)
	newEvidence(alice, "Missed") // would be replayed
	e.published()

	release := e.holdRelayLock()
	defer release()
	start := time.Now()
	s := open(t, alice, cursor)
	if s.res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 once the token expired during replay", s.res.StatusCode)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %v; should end at expiry", d)
	}
}

func TestEventsDrainDuringReplay(t *testing.T) {
	e := newEventEnv(t, eventOpts{})
	alice := e.client("alice", time.Hour)
	cursor := e.cursor(alice)
	release := e.holdRelayLock()
	defer release()

	go func() { time.Sleep(200 * time.Millisecond); close(e.drain) }()
	s := open(t, alice, cursor)
	if s.res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 when draining during replay", s.res.StatusCode)
	}
}

// The write deadline is cleared after each write. Left armed, HTTP/2 resets
// an idle stream when it passes, before the next heartbeat.
func TestEventsIdleHTTP2StreamOutlivesWriteTimeout(t *testing.T) {
	e := newEventEnv(t, eventOpts{http2: true, writeTO: 100 * time.Millisecond, heartbeat: 400 * time.Millisecond, noRelay: true})
	s := open(t, e.client("alice", time.Hour), "")
	if s.res.ProtoMajor != 2 {
		t.Fatalf("proto %s, want HTTP/2", s.res.Proto)
	}
	s.expect("ready")
	for range 2 {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				t.Fatal("stream ended while idle")
			}
			if ev.Comment != "ping" {
				t.Fatalf("got %+v, want a ping", ev)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no heartbeat")
		}
	}
}
