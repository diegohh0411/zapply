package store

import (
	"database/sql"
	"testing"
)

func TestMigrationCatalogValid(t *testing.T) {
	// sql.Open is lazy. An invalid hostname ensures this test cannot depend on
	// Postgres or silently skip validation when integration tests are disabled.
	db, err := sql.Open("pgx", "postgres://unused:unused@migrations.invalid/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := migrationProvider(db); err != nil {
		t.Fatalf("invalid SQL/Go migration catalog: %v", err)
	}
}
