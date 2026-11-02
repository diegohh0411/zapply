package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

func principal() auth.Principal {
	return auth.Principal{Issuer: "test", Subject: uuid.NewString()}
}

func TestOpenRejectsUnsafeRole(t *testing.T) {
	db := testdb.New(t)
	_, err := store.Open(context.Background(), db.AdminURL)
	if !errors.Is(err, store.ErrUnsafeRole) {
		t.Fatalf("err = %v, want ErrUnsafeRole", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testdb.New(t)
	if err := store.Migrate(context.Background(), db.AdminURL, discardLogger()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestIdentityEmailMigrationWithoutInheritedPlatformOwnership(t *testing.T) {
	db := testdb.NewAtVersion(t, 9)
	ctx := context.Background()
	admin := db.Admin(t)
	role := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN NOINHERIT NOSUPERUSER"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "REASSIGN OWNED BY "+role+" TO CURRENT_USER; DROP OWNED BY "+role+"; DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	})
	for _, statement := range []string{
		"GRANT zapply_platform TO " + role + " WITH INHERIT FALSE, SET TRUE",
		"GRANT USAGE, CREATE ON SCHEMA public TO " + role,
		"GRANT USAGE, CREATE ON SCHEMA platform TO " + role,
		"ALTER TABLE public.identity OWNER TO " + role,
		"ALTER TABLE public.goose_db_version OWNER TO " + role,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	migrationURL, err := url.Parse(db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	query := migrationURL.Query()
	query.Set("role", role)
	migrationURL.RawQuery = query.Encode()
	// This role owns the identity schema change, not later feature migrations.
	if err := store.MigrateTo(ctx, migrationURL.String(), 10, discardLogger()); err != nil {
		t.Fatalf("migrate as non-inheriting schema owner: %v", err)
	}
	var owner string
	if err := admin.QueryRow(ctx, `SELECT pg_get_userbyid(proowner) FROM pg_proc WHERE oid='platform.members(uuid,integer)'::regprocedure`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "zapply_platform" {
		t.Fatalf("members function owner = %s; want zapply_platform", owner)
	}
}

func TestResolveUserStable(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	p := principal()

	a, err := db.Store.ResolveUser(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version() != 7 {
		t.Fatalf("user id version = %d, want 7", a.Version())
	}
	// A fresh store has an empty cache, so this goes to the database.
	other, err := store.Open(ctx, db.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	b, err := other.ResolveUser(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("same principal resolved to %s and %s", a, b)
	}

	c, _ := db.Store.ResolveUser(ctx, auth.Principal{Issuer: "other", Subject: p.Subject})
	if c == a {
		t.Fatal("different issuer resolved to the same user")
	}
}

func TestResolveUserConcurrentFirstSight(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	p := principal()

	const n = 16
	ids := make([]uuid.UUID, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			s, err := store.Open(ctx, db.AppURL) // separate caches
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			ids[i], err = s.ResolveUser(ctx, p)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("concurrent resolves disagree: %v", ids)
		}
	}
	var users int
	if err := db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM app_user`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Fatalf("app_user rows = %d, want 1", users)
	}
}

func TestTenantIsolation(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	alice, _ := db.Store.ResolveUser(ctx, principal())
	bob, _ := db.Store.ResolveUser(ctx, principal())

	// Unfiltered reads see only the tenant's own row.
	err := db.Store.InTenant(ctx, alice, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT id FROM app_user`)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != alice {
			t.Errorf("alice sees %v", ids)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Writes for another tenant are rejected by the policy's WITH CHECK.
	err = db.Store.InTenant(ctx, alice, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO app_user (id) VALUES ($1)`, uuid.New())
		return err
	})
	if !isCode(err, "42501") {
		t.Fatalf("cross-tenant insert err = %v, want RLS violation", err)
	}
	err = db.Store.InTenant(ctx, alice, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM app_user WHERE id = $1`, bob)
		if err == nil && tag.RowsAffected() != 0 {
			t.Error("alice deleted bob")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoTenantSeesNothing(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	if _, err := db.Store.ResolveUser(ctx, principal()); err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.Connect(ctx, db.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM app_user`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("app_user visible without tenant: %d rows", n)
	}
	// resolve_identity must not leak its tenant setting to the caller.
	var setting string
	if err := conn.QueryRow(ctx, `SELECT coalesce(current_setting('app.user_id', true), '')`).Scan(&setting); err != nil {
		t.Fatal(err)
	}
	if setting != "" {
		t.Fatalf("app.user_id leaked: %q", setting)
	}
	_, err = conn.Exec(ctx, `SELECT * FROM identity`)
	if !isCode(err, "42501") {
		t.Fatalf("identity readable by runtime role: err = %v", err)
	}
}

func TestTenantSettingIsTransactionLocal(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	alice, _ := db.Store.ResolveUser(ctx, principal())

	conn, err := pgx.Connect(ctx, db.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT set_config('app.user_id', $1, true)`, alice.String())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM app_user`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("tenant setting survived the transaction")
	}
}

func isCode(err error, code string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == code
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFunctionsAndIdentityNotPublic(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	if _, err := db.Store.ResolveUser(ctx, principal()); err != nil {
		t.Fatal(err)
	}
	admin := db.Admin(t)
	role := "probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), db.AdminURL)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP OWNED BY "+role+"; DROP ROLE "+role)
			c.Close(context.Background())
		}
	})

	for _, fn := range []string{"resolve_identity(text,text,uuid)", "resolve_identity(text,text,uuid,text)", "app_user_id()"} {
		var ok bool
		if err := admin.QueryRow(ctx, `SELECT has_function_privilege($1, $2, 'EXECUTE')`, role, fn).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("unrelated role can execute %s", fn)
		}
	}

	// Simulate a platform that grants every new table to its own roles
	// (Supabase's anon): RLS on identity must still hide every row.
	if _, err := admin.Exec(ctx, "GRANT SELECT ON identity TO "+role); err != nil {
		t.Fatal(err)
	}
	var n int
	err := pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM identity`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("granted role reads %d identity rows", n)
	}
}

// Transaction-mode poolers (Supavisor) can't carry named prepared statements
// between backends, so the pool must not create any.
func TestNoNamedPreparedStatements(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	sep := "?"
	if strings.Contains(db.AppURL, "?") {
		sep = "&"
	}
	s, err := store.Open(ctx, db.AppURL+sep+"pool_max_conns=1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	uid, err := s.ResolveUser(ctx, principal())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		err := s.InTenant(ctx, uid, func(tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx, `SELECT id FROM app_user WHERE id = $1`, uid).Scan(&id)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d named prepared statements on the connection", n)
	}
}

func TestSummaryTokenCapMigrationWithoutInheritedPlatformOwnership(t *testing.T) {
	db := testdb.NewAtVersion(t, 22)
	ctx := context.Background()
	admin := db.Admin(t)
	user, err := db.Store.ResolveUser(ctx, principal())
	if err != nil {
		t.Fatal(err)
	}
	job := uuid.New()
	status := uuid.New()
	if _, err := admin.Exec(ctx, `INSERT INTO job_status(id,user_id,name,normalized_name,stage,position) VALUES($1,$2,'Saved','saved','saved','a0')`, status, user); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO job(id,user_id,status_id,title,position,content_hash) VALUES($1,$2,$3,'Engineer','a0','\x00'::bytea)`, job, user, status); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO cv_draft(user_id,job_id) VALUES($1,$2)`, user, job); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO cv_summary(user_id,job_id,dirty) VALUES($1,$2,false)`, user, job); err != nil {
		t.Fatal(err)
	}
	role := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "REASSIGN OWNED BY "+role+" TO CURRENT_USER; DROP OWNED BY "+role+"; DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	})
	for _, statement := range []string{
		"GRANT zapply_platform TO " + role + " WITH INHERIT FALSE, SET TRUE",
		"GRANT USAGE, CREATE ON SCHEMA public, platform TO " + role,
		"ALTER TABLE platform.model_settings OWNER TO " + role,
		"ALTER TABLE public.goose_db_version OWNER TO " + role,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	migrationURL, err := url.Parse(db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	query := migrationURL.Query()
	// RESET ROLE must also return to this restricted identity, never the admin.
	query.Set("session_authorization", role)
	migrationURL.RawQuery = query.Encode()
	if err := store.MigrateTo(ctx, migrationURL.String(), 23, discardLogger()); err != nil {
		t.Fatalf("migrate as non-inheriting schema owner: %v", err)
	}
	if err := store.MigrateTo(ctx, migrationURL.String(), 23, discardLogger()); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	for _, signature := range []string{"platform.summary_settings()", "platform.save_summary_settings(text,text,integer)"} {
		var owner string
		if err := admin.QueryRow(ctx, `SELECT pg_get_userbyid(proowner) FROM pg_proc WHERE oid=$1::regprocedure`, signature).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != "zapply_platform" {
			t.Fatalf("%s owner = %s; want zapply_platform", signature, owner)
		}
	}
	var cap int
	if err := admin.QueryRow(ctx, `SELECT (platform.summary_settings()->>'max_output_tokens')::int`).Scan(&cap); err != nil || cap != 2048 {
		t.Fatalf("summary cap = %d, err = %v; want 2048", cap, err)
	}
	var dirty bool
	if err := admin.QueryRow(ctx, `SELECT dirty FROM cv_summary WHERE user_id=$1 AND job_id=$2`, user, job).Scan(&dirty); err != nil || !dirty {
		t.Fatalf("existing summary not invalidated: dirty=%v err=%v", dirty, err)
	}
	// Exercise the actual rollback SQL under the same restricted migration role.
	source, err := os.ReadFile("migrations/00023_summary_token_cap.sql")
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
		t.Fatalf("rollback as non-inheriting schema owner: %v", err)
	}
	var hasCap bool
	if err := admin.QueryRow(ctx, `SELECT platform.summary_settings() ? 'max_output_tokens'`).Scan(&hasCap); err != nil || hasCap {
		t.Fatalf("rollback did not restore summary settings: hasCap=%v err=%v", hasCap, err)
	}
}
