package alerts_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/notify"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

// TestCatalogLeavesDeliveryUnchanged polls the same board history without a
// catalog, with one, and with one whose database is gone: the members get
// the same alerts every time.
func TestCatalogLeavesDeliveryUnchanged(t *testing.T) {
	run := func(t *testing.T, mode string) ([]string, int) {
		ctx := context.Background()
		d := testdb.New(t)
		alice, err := d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		fake := &fakeBoards{boards: map[string]ats.Board{}}
		fake.set("lever/acme", posting("old", "Backend Engineer", "Remote"))
		ws, err := store.Open(ctx, d.WorkerURL)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		poller := alerts.New(ws, fake)
		switch mode {
		case "catalog":
			poller.WithCatalog(catalog.New(ws, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
		case "broken":
			gone, err := store.Open(ctx, d.WorkerURL)
			if err != nil {
				t.Fatal(err)
			}
			gone.Close()
			poller.WithCatalog(catalog.New(gone, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
		}
		kw := []string{"engineer"}
		if _, err := alerts.New(d.Store, fake).Create(ctx, alice, alerts.Input{URL: "https://jobs.lever.co/acme", Keywords: &kw}); err != nil {
			t.Fatal(err)
		}
		for _, ps := range [][]ats.Posting{
			{posting("old", "Backend Engineer", "Remote")},
			{posting("old", "Backend Engineer", "Remote"), posting("n1", "Staff Engineer", "NYC"), posting("n2", "Designer", "NYC")},
			{posting("old", "Backend Engineer", "Remote"), posting("n1", "Staff Engineer", "NYC"), posting("n3", "Staff Engineer", "NYC")},
		} {
			fake.set("lever/acme", ps...)
			if err := poller.Poll(ctx, "lever", "acme"); err != nil {
				t.Fatalf("%s poll: %v", mode, err)
			}
		}
		items, err := notify.New(d.Store).List(ctx, alice, notify.Filter{Kind: "job_alert", Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var titles []string
		for _, it := range items {
			titles = append(titles, it.JobAlert.Title+"@"+it.JobAlert.URL)
		}
		var cataloged int
		if err := ws.Pool().QueryRow(ctx, `SELECT count(*) FROM platform.posting`).Scan(&cataloged); err != nil {
			t.Fatal(err)
		}
		return titles, cataloged
	}
	want, none := run(t, "none")
	if len(want) != 2 || none != 0 {
		t.Fatalf("baseline alerts = %v, cataloged %d", want, none)
	}
	for _, mode := range []string{"catalog", "broken"} {
		got, cataloged := run(t, mode)
		if len(got) != len(want) {
			t.Fatalf("%s: alerts %v, want %v", mode, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: alerts %v, want %v", mode, got, want)
			}
		}
		// Every board posting is cataloged, baseline ones included; the
		// two Staff Engineer openings stay two postings.
		if mode == "catalog" && cataloged != 4 {
			t.Errorf("cataloged %d postings, want 4", cataloged)
		}
	}
}
