package store_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

func TestScraperSettingsMigrationWithoutInheritedPlatformOwnership(t *testing.T) {
	db := testdb.NewAtVersion(t, 31)
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
	if err := store.MigrateTo(ctx, migrationURL.String(), 32, discardLogger()); err != nil {
		t.Fatalf("migration: %v", err)
	}
	source, err := os.ReadFile("migrations/00032_scraper_settings.sql")
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
	var left int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname IN ('scraper_settings','save_scraper_settings','seed_scraper_settings','scraper_key','scraper_settings_valid')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rollback left %d functions (%v)", left, err)
	}
	if _, err := admin.Exec(ctx, `SELECT platform.add_key(gen_random_uuid(),'old',''::bytea,'v1','fp')`); err == nil || !strings.Contains(err.Error(), "platform owner") {
		t.Fatalf("restored add_key should demand an owner: %v", err)
	}
}

// scraperFixture is an owner and a member of a migrated database.
type scraperFixture struct {
	db            *testdb.DB
	owner, member uuid.UUID
}

func newScraperFixture(t *testing.T) scraperFixture {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	f := scraperFixture{db: db}
	var err error
	if f.owner, err = db.Store.ResolveUser(ctx, principal()); err != nil {
		t.Fatal(err)
	}
	if f.member, err = db.Store.ResolveUser(ctx, principal()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetOwner(ctx, db.AdminURL, f.owner, true); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f scraperFixture) save(user uuid.UUID, providers, rules string, version int) (string, error) {
	var out string
	err := f.db.Store.InTenant(context.Background(), user, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT platform.save_scraper_settings($1::jsonb,$2::jsonb,$3)::text`, providers, rules, version).Scan(&out)
	})
	return out, err
}

func (f scraperFixture) addKey(t *testing.T, kind string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := f.db.Store.InTenant(context.Background(), f.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `SELECT platform.add_key($1,$2,$3,$4,$5,$6)`, id, kind+" key", []byte("sealed"), "v1", "fingerprint", kind)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func pgCode(err error) string {
	var p *pgconn.PgError
	if e, ok := err.(*pgconn.PgError); ok {
		p = e
	}
	if p == nil {
		return ""
	}
	return p.Code
}

func TestScraperSettingsSave(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()

	raw, err := f.db.Store.ScraperSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Providers, Rules []json.RawMessage
		Saved            bool
		Version          int
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.Saved || s.Version != 1 || len(s.Providers) != 0 || len(s.Rules) != 0 {
		t.Fatalf("unsaved settings = %s (%v)", raw, err)
	}

	key := f.addKey(t, "firecrawl")
	providers := `[{"kind":"firecrawl","base_url":null,"key_id":"` + key.String() + `"},{"kind":"firecrawl","base_url":"http://127.0.0.1:3002"}]`
	rules := `[{"host":"linkedin.com","providers":[1,0]}]`

	if _, err := f.save(f.member, providers, rules, 1); pgCode(err) != "ZA403" {
		t.Fatalf("member save: %v", err)
	}
	out, err := f.save(f.owner, providers, rules, 1)
	if err != nil {
		t.Fatalf("owner save: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil || !s.Saved || s.Version != 2 || len(s.Providers) != 2 || len(s.Rules) != 1 {
		t.Fatalf("saved settings = %s (%v)", out, err)
	}
	if _, err := f.save(f.owner, providers, rules, 1); pgCode(err) != "ZA412" {
		t.Fatalf("stale save: %v", err)
	}

	// The runtime read needs no user and sees the saved settings.
	raw, err = f.db.Store.ScraperSettings(ctx)
	if err != nil || !strings.Contains(string(raw), "linkedin.com") {
		t.Fatalf("runtime read = %s (%v)", raw, err)
	}

	for name, tc := range map[string]struct{ providers, rules, code string }{
		"unknown kind":     {`[{"kind":"other"}]`, `[]`, "ZA422"},
		"extra field":      {`[{"kind":"firecrawl","token":"x"}]`, `[]`, "ZA422"},
		"relative url":     {`[{"kind":"firecrawl","base_url":"localhost:3002"}]`, `[]`, "ZA422"},
		"not an array":     {`{}`, `[]`, "ZA422"},
		"scheme in host":   {providers, `[{"host":"https://linkedin.com","providers":[0]}]`, "ZA422"},
		"uppercase host":   {providers, `[{"host":"LinkedIn.com","providers":[0]}]`, "ZA422"},
		"missing provider": {providers, `[{"host":"linkedin.com","providers":[2]}]`, "ZA422"},
		"no providers":     {providers, `[{"host":"linkedin.com","providers":[]}]`, "ZA422"},
		"fractional ref":   {providers, `[{"host":"linkedin.com","providers":[0.5]}]`, "ZA422"},
		"unknown key":      {`[{"kind":"firecrawl","key_id":"` + uuid.NewString() + `"}]`, `[]`, "ZA422"},
		"openrouter key":   {`[{"kind":"firecrawl","key_id":"` + f.addKey(t, "openrouter").String() + `"}]`, `[]`, "ZA422"},
	} {
		if _, err := f.save(f.owner, tc.providers, tc.rules, 2); pgCode(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
	// The table rejects what the function would not have let through.
	if _, err := f.db.Admin(t).Exec(ctx, `UPDATE platform.scraper_settings SET rules='[{"host":"x","providers":[9]}]'`); pgCode(err) != "23514" {
		t.Errorf("direct write: %v", err)
	}
}

func TestScraperSettingsSeed(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	id := uuid.New()
	providers := json.RawMessage(`[{"kind":"firecrawl","base_url":"http://127.0.0.1:3002","key_id":"` + id.String() + `"}]`)

	seeded, err := f.db.Store.SeedScraperSettings(ctx, providers, &store.SealedKey{ID: id, Ciphertext: []byte("sealed"), Version: "v1", Fingerprint: "abc"})
	if err != nil || !seeded {
		t.Fatalf("seeded = %v, err = %v", seeded, err)
	}
	cipher, version, ok, err := f.db.Store.ScraperKey(ctx, id)
	if err != nil || !ok || string(cipher) != "sealed" || version != "v1" {
		t.Fatalf("key = %q %q %v (%v)", cipher, version, ok, err)
	}
	// The key is a Firecrawl system key: listed, never active.
	var keys string
	if err := f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.keys()::text`).Scan(&keys)
	}); err != nil || !strings.Contains(keys, `"kind": "firecrawl"`) || !strings.Contains(keys, `"active": false`) {
		t.Fatalf("keys = %s (%v)", keys, err)
	}
	err = f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.activate_key($1,true,1)`, id)
		return err
	})
	if pgCode(err) != "23514" {
		t.Errorf("activating a Firecrawl key: %v", err)
	}

	// Once settings exist, the environment is ignored.
	if seeded, err = f.db.Store.SeedScraperSettings(ctx, json.RawMessage(`[]`), nil); err != nil || seeded {
		t.Fatalf("second seed = %v, %v", seeded, err)
	}
	raw, err := f.db.Store.ScraperSettings(ctx)
	if err != nil || !strings.Contains(string(raw), "127.0.0.1:3002") || !strings.Contains(string(raw), `"saved": true`) {
		t.Fatalf("settings = %s (%v)", raw, err)
	}

	// An owner's save counts too: nothing is imported over it.
	g := newScraperFixture(t)
	if _, err := g.save(g.owner, `[]`, `[]`, 1); err != nil {
		t.Fatal(err)
	}
	if seeded, err = g.db.Store.SeedScraperSettings(ctx, providers, nil); err != nil || seeded {
		t.Fatalf("seed after save = %v, %v", seeded, err)
	}
}

func TestScraperFunctionsAreNotForTheWorkerToWrite(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	admin := f.db.Admin(t)
	for fn, want := range map[string]bool{
		"platform.scraper_settings()":                                true,
		"platform.scraper_key(uuid)":                                 true,
		"platform.save_scraper_settings(jsonb,jsonb,integer)":        false,
		"platform.seed_scraper_settings(jsonb,uuid,bytea,text,text)": false,
		"platform.add_key(uuid,text,bytea,text,text,text)":           false,
	} {
		var ok bool
		if err := admin.QueryRow(ctx, `SELECT has_function_privilege('zapply_worker',$1,'EXECUTE')`, fn).Scan(&ok); err != nil || ok != want {
			t.Errorf("worker execute %s = %v (%v), want %v", fn, ok, err, want)
		}
		if err := admin.QueryRow(ctx, `SELECT has_function_privilege('public',$1,'EXECUTE')`, fn).Scan(&ok); err != nil || ok {
			t.Errorf("public can execute %s", fn)
		}
	}
}
