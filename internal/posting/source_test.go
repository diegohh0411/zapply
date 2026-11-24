package posting

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/testdb"
)

func TestSeedFromEnvImportsOnce(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	var auth string
	firecrawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"success":true,"data":{"markdown":"# Job","rawHtml":"","metadata":{}}}`))
	}))
	t.Cleanup(firecrawl.Close)

	// Nothing configured, nothing imported.
	if seeded, err := SeedFromEnv(ctx, db.Store, ring, "", ""); err != nil || seeded {
		t.Fatalf("empty env: seeded = %v, err = %v", seeded, err)
	}
	src := NewDBSource(db.Store, ring)
	if s, err := src.Settings(ctx); err != nil || len(s.Providers) != 0 {
		t.Fatalf("settings = %+v, err = %v", s, err)
	}

	if seeded, err := SeedFromEnv(ctx, db.Store, ring, firecrawl.URL, "fc-env-key"); err != nil || !seeded {
		t.Fatalf("seeded = %v, err = %v", seeded, err)
	}
	s, err := src.Settings(ctx)
	if err != nil || len(s.Providers) != 1 || s.Providers[0].BaseURL != firecrawl.URL || s.Providers[0].KeyID == "" {
		t.Fatalf("settings = %+v, err = %v", s, err)
	}
	if key, err := src.Key(ctx, s.Providers[0].KeyID); err != nil || key != "fc-env-key" {
		t.Fatalf("key = %q, err = %v", key, err)
	}

	// The environment is ignored from then on.
	if seeded, err := SeedFromEnv(ctx, db.Store, ring, "http://other.test", "fc-other"); err != nil || seeded {
		t.Fatalf("second import: seeded = %v, err = %v", seeded, err)
	}
	if s2, _ := src.Settings(ctx); len(s2.Providers) != 1 || s2.Providers[0].BaseURL != firecrawl.URL {
		t.Fatalf("settings changed: %+v", s2)
	}

	// The imported chain reads a page with the imported key.
	page, err := NewChain(src).Scrape(ctx, "https://example.com/job")
	if err != nil || page.Markdown != "# Job" || auth != "Bearer fc-env-key" {
		t.Fatalf("page = %+v, auth = %q, err = %v", page, auth, err)
	}
	if _, err := src.Key(ctx, "not-a-uuid"); err == nil {
		t.Error("invalid key ID accepted")
	}
}

func TestSeedFromEnvWithoutKeyRing(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	// A key cannot be sealed without a ring; nothing is half-imported.
	if seeded, err := SeedFromEnv(ctx, db.Store, nil, "", "fc-key"); err == nil || seeded {
		t.Fatalf("seeded = %v, err = %v", seeded, err)
	}
	// Firecrawl Local without a token needs no key.
	if seeded, err := SeedFromEnv(ctx, db.Store, nil, "http://127.0.0.1:3002", ""); err != nil || !seeded {
		t.Fatalf("seeded = %v, err = %v", seeded, err)
	}
}
