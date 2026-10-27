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
	"strings"
	"time"

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
	return MigrateTo(ctx, url, 0, log)
}

// MigrateTo applies only forward migrations up to a fixed version. Zero means
// all pending migrations. Upgrade tests use this to create real older schemas.
func MigrateTo(ctx context.Context, url string, target int64, log *slog.Logger) error {
	cfg, err := adminConfig(url)
	if err != nil {
		return err
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	return migrate(ctx, db, log, target)
}

// migrationProvider builds the complete SQL and Go migration catalog without
// opening a database connection, so it can also be validated in unit tests.
func migrationProvider(db *sql.DB) (*goose.Provider, error) {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, db, dir,
		goose.WithGoMigrations(goose.NewGoMigration(6, &goose.GoFunc{RunTx: backfillJobURLs}, nil),
			goose.NewGoMigration(8, &goose.GoFunc{RunDB: migrateRiver}, nil)))
}

func migrate(ctx context.Context, db *sql.DB, log *slog.Logger, target int64) error {
	p, err := migrationProvider(db)
	if err != nil {
		return err
	}
	var results []*goose.MigrationResult
	if target > 0 {
		results, err = p.UpTo(ctx, target)
	} else {
		results, err = p.Up(ctx)
	}
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
	}
	return err
}

// poolerSafe uses exec mode unless the URL sets default_query_exec_mode. Named
// prepared statements break behind Supavisor, in transaction mode and also
// when a session-mode backend is reused with statements still prepared.
func poolerSafe(cfg *pgx.ConnConfig, url string) {
	if !strings.Contains(url, "default_query_exec_mode") {
		cfg.DefaultQueryExecMode = pgx.QueryExecModeExec
	}
}

// registerTypes maps our Go types, since exec mode infers parameter types.
func registerTypes(c *pgx.Conn) {
	c.TypeMap().RegisterDefaultPgType(uuid.UUID{}, "uuid")
	c.TypeMap().RegisterDefaultPgType([]uuid.UUID{}, "_uuid")
}

// adminConfig is the pooler-safe configuration for migration credentials.
func adminConfig(url string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	poolerSafe(cfg, url)
	return cfg, nil
}

// connectAdmin opens one pooler-safe connection with migration credentials.
func connectAdmin(ctx context.Context, url string) (*pgx.Conn, error) {
	cfg, err := adminConfig(url)
	if err != nil {
		return nil, err
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	registerTypes(c)
	return c, nil
}

// Store is safe for concurrent use.
type Store struct {
	pool  *pgxpool.Pool
	users *lru.Cache[auth.Principal, uuid.UUID]
}

// Open connects with the runtime role and refuses roles that would bypass RLS.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	users, _ := lru.New[auth.Principal, uuid.UUID](50_000)
	return &Store{pool: pool, users: users}, nil
}

// Connect opens a pool for a restricted role (the API's or the relay's) and
// refuses roles that are superuser or bypass RLS.
//
// Queries use pgx's exec mode (no named prepared statements) unless the URL
// sets default_query_exec_mode, so the pool works behind transaction-mode
// poolers such as Supavisor that cannot carry prepared statements between
// backends.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	poolerSafe(cfg.ConnConfig, url)
	cfg.AfterConnect = func(_ context.Context, c *pgx.Conn) error {
		registerTypes(c)
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := checkRuntimeRole(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func (s *Store) Close() { s.pool.Close() }

// Pool supplies the same database to River's insert-only client. All domain
// queries still go through InTenant.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

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
		return fmt.Errorf("%w: %q (use a restricted role, e.g. zapply_app)", ErrUnsafeRole, name)
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
	query := `SELECT resolve_identity($1, $2, $3)`
	args := []any{p.Issuer, p.Subject, newID}
	if p.Email != "" {
		query = `SELECT resolve_identity($1, $2, $3, $4)`
		args = append(args, p.Email)
	}
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("resolve identity: %w", err)
	}
	s.users.Add(p, id)
	return id, nil
}

type User struct {
	ID        uuid.UUID
	CreatedAt time.Time
}

// GetUser returns the tenant's own user row.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	err := s.InTenant(ctx, id, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, created_at FROM app_user WHERE id = $1`, id).Scan(&u.ID, &u.CreatedAt)
	})
	return u, err
}
