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

// fakeFirecrawl answers every scrape with status and records the bearer token
// it was sent.
func fakeFirecrawl(t *testing.T, status int, markdown string, calls *atomic.Int32, token *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		token.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			fmt.Fprint(w, `{"success":false,"error":"nope"}`)
			return
		}
		fmt.Fprintf(w, `{"success":true,"data":{"markdown":%q,"rawHtml":"","metadata":{}}}`, markdown)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestScraperSettingsAdmin(t *testing.T) {
	db := testdb.New(t)
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	// The same chain the server builds: settings come from the database.
	chain := posting.NewChain(posting.NewDBSource(db.Store, ring))
	srv := httptest.NewServer(api.Handler(api.Deps{
		Store: db.Store, Keys: ring, Postings: posting.New(chain),
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

	// Members cannot read or write the settings, nor add provider keys.
	bob.must(403, "GET", "/v1/admin/scraper-settings", nil)
	bob.must(403, "PUT", "/v1/admin/scraper-settings", map[string]any{"providers": []any{}, "rules": []any{}}, "If-Match", "1")
	bob.must(403, "POST", "/v1/admin/system-keys", map[string]any{"label": "Firecrawl", "key": "fc-secret-key", "kind": "firecrawl"})

	owner := uuid.MustParse(alice.must(200, "GET", "/v1/me", nil).Body["id"].(string))
	if err := store.SetOwner(t.Context(), db.AdminURL, owner, true); err != nil {
		t.Fatal(err)
	}

	settings := alice.must(200, "GET", "/v1/admin/scraper-settings", nil)
	if settings.Body["saved"] != false || len(settings.Body["providers"].([]any)) != 0 {
		t.Fatalf("default settings = %v", settings.Body)
	}

	// A Firecrawl key is a system key of its own kind, never active.
	key := alice.must(201, "POST", "/v1/admin/system-keys", map[string]any{"label": "Firecrawl", "key": "fc-secret-key", "kind": "firecrawl"})
	if key.Body["kind"] != "firecrawl" || key.Body["active"] != false {
		t.Fatalf("key = %v", key.Body)
	}
	keyID := key.Body["id"].(string)
	alice.must(422, "POST", "/v1/admin/system-keys", map[string]any{"label": "Bad", "key": "fc-secret-key", "kind": "other"})
	if orKey := alice.must(201, "POST", "/v1/admin/system-keys", map[string]any{"label": "OpenRouter", "key": "or-secret-key"}); orKey.Body["kind"] != "openrouter" {
		t.Fatalf("default kind = %v", orKey.Body)
	}

	var failCalls, okCalls atomic.Int32
	var failToken, okToken atomic.Value
	failing := fakeFirecrawl(t, http.StatusInternalServerError, "", &failCalls, &failToken)
	working := fakeFirecrawl(t, http.StatusOK, "# The job", &okCalls, &okToken)

	body := func(rules ...any) map[string]any {
		return map[string]any{
			"providers": []any{
				map[string]any{"kind": "firecrawl", "base_url": failing.URL},
				map[string]any{"kind": "firecrawl", "base_url": working.URL, "key_id": keyID},
			},
			"rules": rules,
		}
	}
	put := func(want int, b map[string]any, version any) response {
		return alice.must(want, "PUT", "/v1/admin/scraper-settings", b, "If-Match", fmt.Sprint(version))
	}

	// Validation names the setting at fault.
	for field, rule := range map[string]map[string]any{
		"rules[0].host":      {"host": "https://linkedin.com", "providers": []int{0}},
		"rules[0].providers": {"host": "linkedin.com", "providers": []int{5}},
	} {
		if r := put(422, body(rule), 1); !hasFieldError(r, field) {
			t.Errorf("no error for %s: %v", field, r.Body)
		}
	}
	if r := put(422, map[string]any{"providers": []any{map[string]any{"kind": "firecrawl", "base_url": "ftp://x"}}, "rules": []any{}}, 1); !hasFieldError(r, "providers[0].base_url") {
		t.Errorf("no error for base_url: %v", r.Body)
	}
	put(422, map[string]any{"providers": []any{map[string]any{"kind": "firecrawl", "key_id": uuid.NewString()}}, "rules": []any{}}, 1)

	saved := put(200, body(), 1)
	if saved.Body["saved"] != true || saved.Body["version"] != float64(2) || len(saved.Body["providers"].([]any)) != 2 {
		t.Fatalf("saved = %v", saved.Body)
	}
	put(412, body(), 1)

	// The first provider fails, so the second reads the page with its key.
	read := func() response {
		return alice.must(200, "POST", "/v1/postings/read", map[string]any{"url": "https://example.com/job"})
	}
	if r := read(); r.Body["description"] != "# The job" {
		t.Fatalf("read = %v", r.Body)
	}
	if failCalls.Load() != 1 || okCalls.Load() != 1 || okToken.Load() != "Bearer fc-secret-key" {
		t.Fatalf("calls = %d/%d, token = %v", failCalls.Load(), okCalls.Load(), okToken.Load())
	}
}
