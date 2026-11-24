package api_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

func TestScraperSettingsCrawl4AI(t *testing.T) {
	db := testdb.New(t)
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	chain := posting.NewChain(posting.NewDBSource(db.Store, ring))
	srv := httptest.NewServer(api.Handler(api.Deps{
		Store: db.Store, Keys: ring, Postings: posting.New(chain),
		Verifier: auth.NewDev(secret, "authenticated"),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(srv.Close)
	tok, err := auth.MintDev(secret, "authenticated", "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	alice := &client{t: t, base: srv.URL, token: tok}
	owner := uuid.MustParse(alice.must(200, "GET", "/v1/me", nil).Body["id"].(string))
	if err := store.SetOwner(t.Context(), db.AdminURL, owner, true); err != nil {
		t.Fatal(err)
	}

	// A Crawl4AI server that wants its bearer token.
	var calls atomic.Int32
	var token atomic.Value
	crawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		token.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"results":[{"success":true,"status_code":200,"html":"","markdown":{"raw_markdown":"# Via Crawl4AI"},"metadata":{"og:title":"The job"}}]}`)
	}))
	t.Cleanup(crawl.Close)

	crawlKey := alice.must(201, "POST", "/v1/admin/system-keys", map[string]any{"label": "Crawl4AI", "key": "c4ai-secret-token", "kind": "crawl4ai"})
	if crawlKey.Body["kind"] != "crawl4ai" || crawlKey.Body["active"] != false {
		t.Fatalf("key = %v", crawlKey.Body)
	}
	fireKey := alice.must(201, "POST", "/v1/admin/system-keys", map[string]any{"label": "Firecrawl", "key": "fc-secret-key", "kind": "firecrawl"})

	put := func(want int, providers ...any) response {
		return alice.must(want, "PUT", "/v1/admin/scraper-settings", map[string]any{"providers": providers, "rules": []any{}}, "If-Match", "1")
	}
	// Crawl4AI has no hosted API, and takes only its own kind of key.
	if r := put(422, map[string]any{"kind": "crawl4ai"}); !hasFieldError(r, "providers[0].base_url") {
		t.Errorf("no base_url error: %v", r.Body)
	}
	put(422, map[string]any{"kind": "crawl4ai", "base_url": crawl.URL, "key_id": fireKey.Body["id"]})
	put(422, map[string]any{"kind": "firecrawl", "key_id": crawlKey.Body["id"]})

	saved := put(200, map[string]any{"kind": "crawl4ai", "base_url": crawl.URL, "key_id": crawlKey.Body["id"]})
	providers := saved.Body["providers"].([]any)
	if len(providers) != 1 || providers[0].(map[string]any)["kind"] != "crawl4ai" {
		t.Fatalf("saved = %v", saved.Body)
	}

	r := alice.must(200, "POST", "/v1/postings/read", map[string]any{"url": "https://example.com/job"})
	if r.Body["description"] != "# Via Crawl4AI" || r.Body["title"] != "The job" {
		t.Fatalf("read = %v", r.Body)
	}
	if calls.Load() != 1 || token.Load() != "Bearer c4ai-secret-token" {
		t.Fatalf("calls = %d, token = %v", calls.Load(), token.Load())
	}
}
