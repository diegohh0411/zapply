package alerts_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/notify"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/google/uuid"
)

type fakeBoards struct {
	mu     sync.Mutex
	boards map[string]ats.Board
}

func (f *fakeBoards) Fetch(_ context.Context, system, board string) (ats.Board, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.boards[system+"/"+board]
	if !ok {
		return ats.Board{}, ats.ErrNotFound
	}
	return b, nil
}

func (f *fakeBoards) set(key string, ps ...ats.Posting) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boards[key] = ats.Board{Company: "Acme Inc.", Postings: ps}
}

func posting(key, title, loc string) ats.Posting {
	return ats.Posting{Key: key, Title: title, Location: loc, URL: "https://jobs.lever.co/acme/" + key}
}

func TestWatchAndPoll(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	alice, err := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeBoards{boards: map[string]ats.Board{}}
	fake.set("lever/acme", posting("old", "Backend Engineer", "Remote"))
	api := alerts.New(d.Store, fake)
	ws, err := store.Open(ctx, d.WorkerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	worker := alerts.New(ws, fake)
	notes := notify.New(d.Store)

	// Creating checks the board and takes its name.
	if _, err := api.Create(ctx, alice, alerts.Input{URL: "https://jobs.lever.co/missing"}); !isValidation(err) {
		t.Fatalf("missing board: %v", err)
	}
	if _, err := api.Create(ctx, alice, alerts.Input{URL: "https://linkedin.com/jobs/1"}); !isValidation(err) {
		t.Fatalf("unknown link: %v", err)
	}
	kw := []string{"engineer"}
	w, err := api.Create(ctx, alice, alerts.Input{URL: "jobs.lever.co/acme/old", Keywords: &kw})
	if err != nil || w.Company != "Acme Inc." || w.ATS != "lever" || w.Board != "acme" {
		t.Fatalf("create = %+v, %v", w, err)
	}
	var conflict *library.ConflictError
	if _, err := api.Create(ctx, alice, alerts.Input{URL: "https://jobs.lever.co/acme"}); !errors.As(err, &conflict) {
		t.Fatalf("duplicate: %v", err)
	}

	// The first poll only records what is already open.
	if err := worker.Poll(ctx, "lever", "acme"); err != nil {
		t.Fatal(err)
	}
	if n := alertCount(t, notes, alice); n != 0 {
		t.Fatalf("baseline alerted %d", n)
	}

	// Bob starts watching later, unfiltered.
	if _, err := api.Create(ctx, bob, alerts.Input{URL: "https://jobs.lever.co/acme"}); err != nil {
		t.Fatal(err)
	}

	fake.set("lever/acme", posting("old", "Backend Engineer", "Remote"), posting("new1", "Staff Engineer", "NYC"), posting("new2", "Designer", "NYC"))
	if err := worker.Poll(ctx, "lever", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := worker.Poll(ctx, "lever", "acme"); err != nil { // nothing new: no duplicates
		t.Fatal(err)
	}
	items, err := notes.List(ctx, alice, notify.Filter{Kind: "job_alert", Limit: 100})
	if err != nil || len(items) != 1 || items[0].JobAlert == nil || items[0].JobAlert.Title != "Staff Engineer" {
		t.Fatalf("alice = %+v, %v", items, err)
	}
	if n := alertCount(t, notes, bob); n != 2 {
		t.Fatalf("bob got %d alerts, want 2", n)
	}

	// Paused watches stay quiet; the board's health is reported.
	paused := true
	if w, err = api.Update(ctx, alice, w.ID, alerts.Input{Paused: &paused}); err != nil || !w.Paused || w.LastCheckedAt == nil {
		t.Fatalf("pause = %+v, %v", w, err)
	}
	fake.set("lever/acme", posting("new3", "Platform Engineer", "NYC"))
	if err := worker.Poll(ctx, "lever", "acme"); err != nil {
		t.Fatal(err)
	}
	if n := alertCount(t, notes, alice); n != 1 {
		t.Fatalf("paused watch alerted: %d", n)
	}

	// A failing board records why.
	fake.mu.Lock()
	delete(fake.boards, "lever/acme")
	fake.mu.Unlock()
	if err := worker.Poll(ctx, "lever", "acme"); err != nil {
		t.Fatal(err)
	}
	list, err := api.Watches(ctx, bob)
	if err != nil || len(list) != 1 || list[0].LastError == "" {
		t.Fatalf("watches = %+v, %v", list, err)
	}

	// Deleting removes the alerts and their notifications.
	if err := api.Delete(ctx, bob, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := alertCount(t, notes, bob); n != 0 {
		t.Fatalf("bob still has %d", n)
	}
	if err := api.Delete(ctx, bob, uuid.New()); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func isValidation(err error) bool {
	var v *library.ValidationError
	return errors.As(err, &v)
}

func alertCount(t *testing.T, n *notify.Service, u uuid.UUID) int {
	t.Helper()
	items, err := n.List(context.Background(), u, notify.Filter{Kind: "job_alert", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return len(items)
}
