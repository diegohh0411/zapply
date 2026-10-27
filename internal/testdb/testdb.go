// Package testdb provisions a disposable, migrated Postgres database per test.
//
// Tests that need it are skipped unless ZAPPLY_TEST_DATABASE_URL points at an
// admin connection to a throwaway cluster (see scripts/test-integration.sh).
// The cluster must already have the zapply_app and zapply_relay roles with
// LOGIN, each with its name as password (deploy/postgres/init.sql creates
// them). Roles are cluster-wide,
// so creating them here would race between packages tested in parallel.
package testdb

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/store"
)

// DB is a fresh database with migrations applied.
type DB struct {
	AdminURL  string // schema owner (superuser)
	AppURL    string // restricted runtime role
	RelayURL  string // relay role, outbox only
	WorkerURL string // model execution role
	Store     *store.Store
}

func New(t *testing.T) *DB {
	t.Helper()
	return NewAtVersion(t, 0)
}

// NewAtVersion provisions a real historical schema for upgrade tests; zero
// selects all current migrations.
func NewAtVersion(t *testing.T, version int64) *DB {
	t.Helper()
	base := os.Getenv("ZAPPLY_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("ZAPPLY_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := "zapply_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	adminURL := withDB(t, base, name, "")
	if err := store.MigrateTo(ctx, adminURL, version, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	appURL := withDB(t, base, name, "zapply_app")
	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return &DB{AdminURL: adminURL, AppURL: appURL, RelayURL: withDB(t, base, name, "zapply_relay"), WorkerURL: withDB(t, base, name, "zapply_worker"), Store: st}
}

// Admin opens a superuser connection to the test database.
func (d *DB) Admin(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), d.AdminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func withDB(t *testing.T, base, db, user string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse ZAPPLY_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, user)
	}
	return u.String()
}
