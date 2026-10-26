package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/diegohh0411/zapply/internal/events"
	"github.com/diegohh0411/zapply/internal/relay"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/testnats"
)

var errCrash = errors.New("simulated crash")

type published struct {
	Subject string
	events.Event
}

// recorder is a Publisher that keeps every message in publish order.
type recorder struct {
	mu   sync.Mutex
	msgs []published
}

func (r *recorder) Publish(subject string, data []byte) error {
	var e events.Event
	if err := json.Unmarshal(data, &e); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, published{subject, e})
	return nil
}

func (r *recorder) FlushTimeout(time.Duration) error { return nil }

func (r *recorder) all() []published {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]published(nil), r.msgs...)
}

type env struct {
	db    *testdb.DB
	admin *pgx.Conn
	pool  *pgxpool.Pool
}

func setup(t *testing.T) env {
	t.Helper()
	d := testdb.New(t)
	pool, err := store.Connect(context.Background(), d.RelayURL)
	if err != nil {
		t.Fatalf("connect relay: %v", err)
	}
	t.Cleanup(pool.Close)
	return env{db: d, admin: d.Admin(t), pool: pool}
}

func (e env) newRelay(pub relay.Publisher, batch int) *relay.Relay {
	return relay.New(e.pool, pub, relay.Config{Batch: batch, Interval: 10 * time.Millisecond},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (e env) newUser(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := e.admin.Exec(context.Background(), `INSERT INTO app_user (id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// insertEvent writes an outbox row the way the library service does.
func insertEvent(t *testing.T, x execer, uid uuid.UUID, topic string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	payload, _ := json.Marshal(map[string]any{"id": id})
	if _, err := x.Exec(context.Background(),
		`INSERT INTO outbox (id, user_id, topic, payload) VALUES ($1, $2, $3, $4)`,
		id, uid, topic, payload); err != nil {
		t.Fatal(err)
	}
	return id
}

func step(t *testing.T, r *relay.Relay) int {
	t.Helper()
	n, err := r.Step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	return n
}

func eventID(t *testing.T, p published) uuid.UUID {
	t.Helper()
	var d struct{ ID uuid.UUID }
	if err := json.Unmarshal(p.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.ID
}

func TestPublishesWithSubjectAndPayload(t *testing.T) {
	e := setup(t)
	uid := e.newUser(t)
	id := insertEvent(t, e.admin, uid, "evidence.created")
	rec := &recorder{}
	if n := step(t, e.newRelay(rec, 10)); n != 1 {
		t.Fatalf("published %d, want 1", n)
	}
	msgs := rec.all()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	m := msgs[0]
	if m.Subject != events.Subject(uid, "evidence.created") || m.Topic != "evidence.created" || m.Seq < 1 || eventID(t, m) != id {
		t.Fatalf("unexpected message %+v", m)
	}
	var seq int64
	if err := e.admin.QueryRow(context.Background(),
		`SELECT seq FROM outbox WHERE id = $1 AND published_at IS NOT NULL`, id).Scan(&seq); err != nil || seq != m.Seq {
		t.Fatalf("stored seq = %d (%v), want %d", seq, err, m.Seq)
	}
	if n := step(t, e.newRelay(rec, 10)); n != 0 {
		t.Fatalf("republished %d events", n)
	}
}

// A transaction that commits after a later one still gets delivered, with a
// seq above everything published before it.
func TestOutOfOrderCommits(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.newUser(t)
	rec := &recorder{}
	r := e.newRelay(rec, 10)

	slow, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	early := insertEvent(t, slow, uid, "evidence.created") // lower id, commits last
	other, err := pgx.Connect(ctx, e.db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	late := insertEvent(t, other, uid, "evidence.updated")

	if n := step(t, r); n != 1 {
		t.Fatalf("first batch published %d, want 1", n)
	}
	if err := slow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := step(t, r); n != 1 {
		t.Fatalf("second batch published %d, want 1", n)
	}

	msgs := rec.all()
	if len(msgs) != 2 || eventID(t, msgs[0]) != late || eventID(t, msgs[1]) != early {
		t.Fatalf("delivery order wrong: %+v", msgs)
	}
	if msgs[1].Seq <= msgs[0].Seq {
		t.Fatalf("seq %d after %d: a client resuming from %d would miss it", msgs[1].Seq, msgs[0].Seq, msgs[0].Seq)
	}
}

func TestReplicasNeverInterleave(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	users := []uuid.UUID{e.newUser(t), e.newUser(t), e.newUser(t)}
	const total = 300
	want := map[uuid.UUID]bool{}
	for i := range total {
		want[insertEvent(t, e.admin, users[i%len(users)], "evidence.updated")] = true
	}

	rec := &recorder{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sent := 0
	for range 3 {
		r := e.newRelay(rec, 7)
		wg.Go(func() {
			for {
				mu.Lock()
				done := sent >= total
				mu.Unlock()
				if done {
					return
				}
				n, err := r.Step(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				sent += n
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	msgs := rec.all()
	if len(msgs) != total {
		t.Fatalf("published %d messages, want %d", len(msgs), total)
	}
	for i, m := range msgs {
		if !want[eventID(t, m)] {
			t.Fatalf("message %d delivered twice or unknown", i)
		}
		delete(want, eventID(t, m))
		if i > 0 && m.Seq <= msgs[i-1].Seq {
			t.Fatalf("message %d has seq %d after %d", i, m.Seq, msgs[i-1].Seq)
		}
	}
}

func TestCrashBeforeCommitRedelivers(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.newUser(t)
	for range 3 {
		insertEvent(t, e.admin, uid, "skill.created")
	}
	rec := &recorder{}
	r := e.newRelay(rec, 10)
	r.SetBeforeCommit(func() error { return errCrash })
	if _, err := r.Step(ctx); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v, want simulated crash", err)
	}
	var pending int
	if err := e.admin.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&pending); err != nil || pending != 3 {
		t.Fatalf("pending = %d (%v), want 3 after rollback", pending, err)
	}

	r.SetBeforeCommit(nil)
	if n := step(t, r); n != 3 {
		t.Fatalf("redelivered %d, want 3", n)
	}
	msgs := rec.all()
	if len(msgs) != 6 {
		t.Fatalf("got %d messages, want 3 + 3 duplicates", len(msgs))
	}
	for i := range 3 {
		if eventID(t, msgs[i]) != eventID(t, msgs[i+3]) || msgs[i+3].Seq <= msgs[2].Seq {
			t.Fatalf("redelivery %d = %+v, first delivery %+v", i, msgs[i+3], msgs[i])
		}
	}
}

func TestRolledBackWritesAreNeverPublished(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.newUser(t)
	tx, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insertEvent(t, tx, uid, "evidence.created")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if n := step(t, e.newRelay(rec, 10)); n != 0 || len(rec.all()) != 0 {
		t.Fatalf("published %d events from a rolled-back write", n)
	}
}

func TestPruneKeepsRetentionWindowAndPending(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.newUser(t)
	old := insertEvent(t, e.admin, uid, "evidence.created")
	recent := insertEvent(t, e.admin, uid, "evidence.updated")
	r := e.newRelay(&recorder{}, 10)
	step(t, r)
	pending := insertEvent(t, e.admin, uid, "evidence.deleted")
	if _, err := e.admin.Exec(ctx,
		`UPDATE outbox SET published_at = now() - interval '25 hours' WHERE id = $1;`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec(ctx,
		`UPDATE outbox SET created_at = now() - interval '25 hours' WHERE id = $1`, pending); err != nil {
		t.Fatal(err)
	}

	n, err := r.Prune(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want 1", n, err)
	}
	rows, err := e.admin.Query(ctx, `SELECT id FROM outbox`)
	if err != nil {
		t.Fatal(err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, id := range left {
		got[id] = true
	}
	if len(got) != 2 || !got[recent] || !got[pending] {
		t.Fatalf("left %v, want recent and pending", left)
	}
}

func TestRelayRoleIsOutboxOnly(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, q := range []string{
		`SELECT count(*) FROM app_user`,
		`SELECT count(*) FROM evidence`,
		`SELECT count(*) FROM profile`,
		`SELECT count(*) FROM identity`,
	} {
		var n int
		err := e.pool.QueryRow(ctx, q).Scan(&n)
		if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: err = %v, want permission denied", q, err)
		}
	}
	uid := e.newUser(t)
	_, err := e.pool.Exec(ctx, `INSERT INTO outbox (id, user_id, topic, payload) VALUES ($1, $2, 't', '{}')`,
		uuid.Must(uuid.NewV7()), uid)
	if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("relay insert: err = %v, want permission denied", err)
	}
	if _, err := store.Connect(ctx, e.db.AdminURL); !errors.Is(err, store.ErrUnsafeRole) {
		t.Errorf("Connect as superuser: err = %v, want ErrUnsafeRole", err)
	}
}

// The API reads its own events back for replay, and nothing else.
func TestAppReadsOnlyItsOwnOutbox(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice, bob := e.newUser(t), e.newUser(t)
	insertEvent(t, e.admin, alice, "evidence.created")
	insertEvent(t, e.admin, bob, "evidence.created")
	insertEvent(t, e.admin, bob, "evidence.updated")
	step(t, e.newRelay(&recorder{}, 10))

	err := e.db.Store.InTenant(ctx, alice, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE seq IS NOT NULL`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("alice sees %d events, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE outbox SET seq = NULL, published_at = NULL`, `DELETE FROM outbox`, `SELECT nextval('outbox_seq')`} {
		err := e.db.Store.InTenant(ctx, alice, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: err = %v, want permission denied", q, err)
		}
	}
}

func TestDeliversThroughNATS(t *testing.T) {
	e := setup(t)
	ns := testnats.New(t)
	sub := ns.Connect(t)
	alice, bob := e.newUser(t), e.newUser(t)

	msgs := make(chan *nats.Msg, 10)
	s, err := sub.ChanSubscribe(events.UserSubjects(alice), msgs)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Unsubscribe()
	if err := sub.Flush(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.newRelay(ns.Connect(t), 10).Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	insertEvent(t, e.admin, bob, "evidence.created")
	id := insertEvent(t, e.admin, alice, "evidence.updated")

	select {
	case m := <-msgs:
		var ev events.Event
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			t.Fatal(err)
		}
		if m.Subject != events.Subject(alice, "evidence.updated") || ev.Topic != "evidence.updated" ||
			eventID(t, published{Event: ev}) != id {
			t.Fatalf("unexpected message %s %+v", m.Subject, ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event delivered")
	}
	select {
	case m := <-msgs:
		t.Fatalf("alice received another event: %s", m.Subject)
	case <-time.After(200 * time.Millisecond):
	}
}

// With NATS down the batch rolls back instead of being buffered and sent
// twice later.
func TestNATSDownLeavesRowsPending(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ns := testnats.New(t)
	nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectBufSize(-1))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ns.Shutdown()
	ns.WaitForShutdown()

	uid := e.newUser(t)
	insertEvent(t, e.admin, uid, "evidence.created")
	if _, err := e.newRelay(nc, 10).Step(ctx); err == nil {
		t.Fatal("step succeeded with NATS down")
	}
	var pending int
	if err := e.admin.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending = %d (%v), want 1", pending, err)
	}
}
