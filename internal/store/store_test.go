package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
