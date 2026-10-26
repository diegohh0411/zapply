// Package store owns Postgres access: the connection pool, migrations and
// tenant-scoped transactions.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/diegohh0411/zapply/internal/auth"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies pending migrations. url must connect as the schema owner.
func Migrate(ctx context.Context, url string, log *slog.Logger) error {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return err
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	return migrate(ctx, db, log)
}

func migrate(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, dir)
	if err != nil {
		return err
	}
	results, err := p.Up(ctx)
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
	}
	return err
}

// Store is safe for concurrent use.
type Store struct {
	pool  *pgxpool.Pool
	users *lru.Cache[auth.Principal, uuid.UUID]
}

// Open connects with the runtime role and refuses roles that would bypass RLS.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := checkRuntimeRole(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	users, _ := lru.New[auth.Principal, uuid.UUID](50_000)
	return &Store{pool: pool, users: users}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Ping checks database reachability for readiness probes.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

var ErrUnsafeRole = errors.New("database role bypasses row-level security")

func checkRuntimeRole(ctx context.Context, pool *pgxpool.Pool) error {
	var name string
	var super, bypass bool
	err := pool.QueryRow(ctx,
		`SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&name, &super, &bypass)
	if err != nil {
		return fmt.Errorf("check runtime role: %w", err)
	}
	if super || bypass {
		return fmt.Errorf("%w: %q (use the restricted runtime role, e.g. zapply_app)", ErrUnsafeRole, name)
	}
	return nil
}

// InTenant runs fn in a transaction scoped to userID. RLS policies see the
// user through app_user_id(); the setting is transaction-local, so pooled
// connections never leak it.
func (s *Store) InTenant(ctx context.Context, userID uuid.UUID, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.user_id', $1, true)`, userID.String()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// ResolveUser maps a verified principal to a user ID, creating the user on
// first sight. Mappings never change, so results are cached in-process.
func (s *Store) ResolveUser(ctx context.Context, p auth.Principal) (uuid.UUID, error) {
	if id, ok := s.users.Get(p); ok {
		return id, nil
	}
	newID, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT resolve_identity($1, $2, $3)`, p.Issuer, p.Subject, newID,
	).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("resolve identity: %w", err)
	}
	s.users.Add(p, id)
	return id, nil
}
