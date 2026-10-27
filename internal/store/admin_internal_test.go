package store

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// Migration and provisioning commands run through Supabase's pooler too.
func TestAdminConnectionsAvoidNamedStatements(t *testing.T) {
	cfg, err := adminConfig("postgres://u:p@localhost/db")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Fatalf("exec mode=%v", cfg.DefaultQueryExecMode)
	}
	cfg, err = adminConfig("postgres://u:p@localhost/db?default_query_exec_mode=simple_protocol")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Fatalf("explicit mode overridden: %v", cfg.DefaultQueryExecMode)
	}
}
