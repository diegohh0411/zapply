package store_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

func TestScraperCrawl4AIProviders(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	crawlKey, fireKey, routerKey := f.addKey(t, "crawl4ai"), f.addKey(t, "firecrawl"), f.addKey(t, "openrouter")

	providers := `[{"kind":"crawl4ai","base_url":"http://crawl4ai:11235","key_id":"` + crawlKey.String() + `"},{"kind":"firecrawl","key_id":"` + fireKey.String() + `"}]`
	if _, err := f.save(f.owner, providers, `[{"host":"linkedin.com","providers":[0,1]}]`, 1); err != nil {
		t.Fatalf("save crawl4ai provider: %v", err)
	}
	cipher, _, ok, err := f.db.Store.ScraperKey(ctx, crawlKey)
	if err != nil || !ok || string(cipher) != "sealed" {
		t.Fatalf("crawl4ai key = %q %v (%v)", cipher, ok, err)
	}
	if _, _, ok, err = f.db.Store.ScraperKey(ctx, routerKey); err != nil || ok {
		t.Errorf("an OpenRouter key is not a scraper key: %v %v", ok, err)
	}

	for name, tc := range map[string]string{
		"crawl4ai without an address":       `[{"kind":"crawl4ai"}]`,
		"crawl4ai with a null address":      `[{"kind":"crawl4ai","base_url":null}]`,
		"crawl4ai naming a firecrawl key":   `[{"kind":"crawl4ai","base_url":"http://c:11235","key_id":"` + fireKey.String() + `"}]`,
		"firecrawl naming a crawl4ai key":   `[{"kind":"firecrawl","key_id":"` + crawlKey.String() + `"}]`,
		"crawl4ai naming an OpenRouter key": `[{"kind":"crawl4ai","base_url":"http://c:11235","key_id":"` + routerKey.String() + `"}]`,
		"crawl4ai with a relative address":  `[{"kind":"crawl4ai","base_url":"crawl4ai:11235"}]`,
	} {
		if _, err := f.save(f.owner, tc, `[]`, 2); pgCode(err) != "ZA422" {
			t.Errorf("%s: err = %v, want ZA422", name, err)
		}
	}

	// Only an OpenRouter key is ever active.
	err = f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.activate_key($1,true,1)`, crawlKey)
		return err
	})
	if pgCode(err) != "23514" {
		t.Errorf("activating a Crawl4AI key: %v", err)
	}
	var keys string
	if err := f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.keys()::text`).Scan(&keys)
	}); err != nil || !strings.Contains(keys, `"kind": "crawl4ai"`) {
		t.Fatalf("keys = %s (%v)", keys, err)
	}
	if _, err := f.db.Admin(t).Exec(ctx, `INSERT INTO platform.system_key(id,label,ciphertext,encryption_version,fingerprint,kind) VALUES(gen_random_uuid(),'x','\x00','v1','f','other')`); pgCode(err) != "23514" {
		t.Errorf("unknown key kind: %v", err)
	}
}

// The migration login can SET ROLE zapply_platform but inherits nothing from
// it, as in production: migration 33 must still apply and roll back.
func TestScraperCrawl4AIMigrationWithoutInheritedPlatformOwnership(t *testing.T) {
	db := testdb.NewAtVersion(t, 32)
	ctx := context.Background()
	admin := db.Admin(t)
	role := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "REASSIGN OWNED BY "+role+" TO CURRENT_USER; DROP OWNED BY "+role+"; DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	})
	for _, statement := range []string{
		"GRANT zapply_platform TO " + role + " WITH INHERIT FALSE, SET TRUE",
		"ALTER TABLE public.goose_db_version OWNER TO " + role,
		"ALTER TABLE platform.system_key OWNER TO " + role,
		"ALTER TABLE platform.scraper_settings OWNER TO " + role,
		"ALTER FUNCTION platform.scraper_settings_valid(jsonb,jsonb) OWNER TO " + role,
		"ALTER SCHEMA platform OWNER TO " + role,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	migrationURL, err := url.Parse(db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	migrationURL.User = url.UserPassword(role, password)
	if err := store.MigrateTo(ctx, migrationURL.String(), 33, discardLogger()); err != nil {
		t.Fatalf("migration: %v", err)
	}
	source, err := os.ReadFile("migrations/00033_scraper_crawl4ai.sql")
	if err != nil {
		t.Fatal(err)
	}
	down := strings.SplitN(string(source), "-- +goose Down", 2)[1]
	conn, err := pgx.Connect(ctx, migrationURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, down); return err }); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

func TestScraperCrawl4AIMigrationRollback(t *testing.T) {
	db := testdb.NewAtVersion(t, 32)
	ctx := context.Background()
	admin := db.Admin(t)
	if err := store.MigrateTo(ctx, db.AdminURL, 33, discardLogger()); err != nil {
		t.Fatalf("up: %v", err)
	}
	source, err := os.ReadFile("migrations/00033_scraper_crawl4ai.sql")
	if err != nil {
		t.Fatal(err)
	}
	down := strings.SplitN(string(source), "-- +goose Down", 2)[1]
	rollback := func() error {
		return pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, down); return err })
	}

	// A saved Crawl4AI provider blocks the rollback, and nothing is lost.
	if _, err := admin.Exec(ctx, `UPDATE platform.scraper_settings SET providers='[{"kind":"crawl4ai","base_url":"http://c:11235"}]'`); err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err == nil || !strings.Contains(err.Error(), "Crawl4AI") {
		t.Fatalf("rollback with a crawl4ai provider: %v", err)
	}

	// Without one, Crawl4AI keys go and the Firecrawl-only checks return.
	if _, err := admin.Exec(ctx, `UPDATE platform.scraper_settings SET providers='[]'`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO platform.system_key(id,label,ciphertext,encryption_version,fingerprint,kind) VALUES(gen_random_uuid(),'c','\x00','v1','f','crawl4ai')`); err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM platform.system_key WHERE kind='crawl4ai'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("crawl4ai keys left = %d (%v)", n, err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.scraper_settings SET providers='[{"kind":"crawl4ai","base_url":"http://c:11235"}]'`); pgCode(err) != "23514" {
		t.Errorf("restored validator accepted crawl4ai: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.scraper_settings SET providers='[{"kind":"firecrawl"}]'`); err != nil {
		t.Errorf("restored validator rejected firecrawl: %v", err)
	}
}
