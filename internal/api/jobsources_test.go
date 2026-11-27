package api_test

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

// The owner manages job sources over HTTP; members can't.
func TestJobSources(t *testing.T) {
	db := testdb.New(t)
	srv := httptest.NewServer(api.Handler(api.Deps{
		Store:    db.Store,
		Verifier: auth.NewDev(secret, "authenticated"),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(srv.Close)
	mk := func(sub string) *client {
		tok, err := auth.MintDev(secret, "authenticated", sub, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: srv.URL, token: tok}
	}
	alice, bob := mk("alice"), mk("bob")
	bob.must(403, "GET", "/v1/admin/job-sources", nil)
	owner := uuid.MustParse(alice.must(200, "GET", "/v1/me", nil).Body["id"].(string))
	if err := store.SetOwner(t.Context(), db.AdminURL, owner, true); err != nil {
		t.Fatal(err)
	}

	added := alice.must(200, "POST", "/v1/admin/ats-boards", map[string]any{
		"links": "https://job-boards.greenhouse.io/acme\nhttps://jobs.lever.co/beta/123, https://www.linkedin.com/jobs/view/1",
	})
	if added.Body["added"] != float64(2) || len(added.Body["invalid"].([]any)) != 1 {
		t.Fatalf("added = %v", added.Body)
	}
	page := alice.must(200, "GET", "/v1/admin/ats-boards?status=active", nil)
	if page.Body["total"] != float64(2) {
		t.Fatalf("active = %v", page.Body)
	}
	board := alice.must(200, "PUT", "/v1/admin/ats-boards/lever/beta/status", map[string]any{"status": "disabled"})
	if board.Body["status"] != "disabled" {
		t.Fatalf("board = %v", board.Body)
	}
	alice.must(404, "POST", "/v1/admin/ats-boards/lever/beta/poll", nil)
	alice.must(200, "POST", "/v1/admin/ats-boards/greenhouse/acme/poll", nil)
	sources := alice.must(200, "PUT", "/v1/admin/job-sources/interval", map[string]any{"name": "ats:boards", "interval_minutes": 60})
	if b := sources.Body["boards"].(map[string]any); b["active"] != float64(1) || b["disabled"] != float64(1) {
		t.Fatalf("sources = %v", sources.Body)
	}
	alice.must(422, "PUT", "/v1/admin/job-sources/interval", map[string]any{"name": "ats:boards", "interval_minutes": 7})
}
