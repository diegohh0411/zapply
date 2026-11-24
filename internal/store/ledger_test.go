package store_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

// ledgerMigrations are the central ledger's migrations, newest first.
var ledgerMigrations = []string{"00040_search_watchers.sql", "00039_scrape_charges.sql", "00038_ledger.sql"}

func migrationDown(t *testing.T, file string) string {
	t.Helper()
	source, err := os.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitN(string(source), "-- +goose Down", 2)[1]
}

// platformFunctions snapshots every function's definition and grants.
func platformFunctions(t *testing.T, conn *pgx.Conn) map[string]string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT p.oid::regprocedure::text,pg_get_functiondef(p.oid)||COALESCE(p.proacl::text,'')||pg_get_userbyid(p.proowner)
 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN ('public','platform') AND p.prokind='f'`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var v [2]string
		return v, r.Scan(&v[0], &v[1])
	})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, v := range out {
		m[v[0]] = v[1]
	}
	return m
}

// The ledger migrations move model accounting into charge, model_charge and
// ledger_entry without losing a row, as a migration login that can SET ROLE
// zapply_platform but inherits nothing from it (as in production), and roll
// back to the same functions, grants and rows.
func TestLedgerMigrationsKeepHistoryWithoutInheritedPlatformOwnership(t *testing.T) {
	db := testdb.NewAtVersion(t, 37)
	ctx := context.Background()
	admin := db.Admin(t)
	u, err := db.Store.ResolveUser(ctx, principal())
	if err != nil {
		t.Fatal(err)
	}
	op, attempt, key := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []string{
		`INSERT INTO platform.system_key(id,label,ciphertext,encryption_version,fingerprint) VALUES('` + key.String() + `','k','\x00','v1','f')`,
		`INSERT INTO public.model_operation(id,user_id,idempotency_key,request_hash,status) VALUES('` + op.String() + `','` + u.String() + `','k','h','completed')`,
		`INSERT INTO platform.model_attempt(id,user_id,operation_id,attempt_number,request_hash,model,key_id,quote,reserved,dispatched_at,deadline_at,state,generation_id)
		 VALUES('` + attempt.String() + `','` + u.String() + `','` + op.String() + `',1,'h','m','` + key.String() + `','{"q":1}',900,now()-interval '1 day',now(),'settled','gen-1')`,
		`INSERT INTO platform.model_usage(attempt_id,user_id,amount,accounted_at,source,dedup_key,usage) OVERRIDING SYSTEM VALUE
		 VALUES('` + attempt.String() + `','` + u.String() + `',700,now()-interval '1 day','provider_response','settlement','{"input_tokens":5}')`,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(conn *pgx.Conn) (attempts, usage string) {
		if err := conn.QueryRow(ctx, `SELECT (SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM platform.model_attempt a),
 (SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM platform.model_usage u)`).Scan(&attempts, &usage); err != nil {
			t.Fatal(err)
		}
		return attempts, usage
	}

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
	// The migration login owns what it created; zapply_platform owns the functions.
	if _, err := admin.Exec(ctx, `GRANT zapply_platform TO `+role+` WITH INHERIT FALSE, SET TRUE;
ALTER SCHEMA platform OWNER TO `+role+`;
GRANT CREATE ON SCHEMA public TO `+role+`;
DO $$ DECLARE r record; BEGIN
 FOR r IN SELECT c.oid::regclass AS name FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname IN ('public','platform') AND c.relkind='r' AND pg_get_userbyid(c.relowner)=current_user LOOP
  EXECUTE format('ALTER TABLE %s OWNER TO `+role+`',r.name);
 END LOOP;
 FOR r IN SELECT p.oid::regprocedure AS sig FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname IN ('public','platform') AND pg_get_userbyid(p.proowner)=current_user LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO `+role+`',r.sig);
 END LOOP;
END $$`); err != nil {
		t.Fatal(err)
	}
	beforeFunctions := platformFunctions(t, admin)
	beforeAttempts, beforeUsage := snapshot(admin)

	migrationURL, err := url.Parse(db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	migrationURL.User = url.UserPassword(role, password)
	if err := store.MigrateTo(ctx, migrationURL.String(), 40, discardLogger()); err != nil {
		t.Fatalf("migration: %v", err)
	}
	var kind, state string
	var reserved, amount int64
	var model, detail string
	if err := admin.QueryRow(ctx, `SELECT c.kind,c.state,c.reserved,m.model,e.amount,e.detail::text FROM platform.charge c
 JOIN platform.model_charge m ON m.charge_id=c.id JOIN platform.ledger_entry e ON e.charge_id=c.id WHERE c.id=$1`, attempt).
		Scan(&kind, &state, &reserved, &model, &amount, &detail); err != nil {
		t.Fatal(err)
	}
	if kind != "model" || state != "settled" || reserved != 900 || model != "m" || amount != 700 || detail != `{"input_tokens": 5}` {
		t.Fatalf("migrated %s %s %d %s %d %s", kind, state, reserved, model, amount, detail)
	}
	var usage []byte
	if err := db.Store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.usage()`).Scan(&usage)
	}); err != nil || !strings.Contains(string(usage), `"spent_7d_usd": "0.000000000700"`) {
		t.Fatalf("usage = %s, %v", usage, err)
	}

	conn, err := pgx.Connect(ctx, migrationURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, file := range ledgerMigrations {
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, migrationDown(t, file)); return err }); err != nil {
			t.Fatalf("rollback %s: %v", file, err)
		}
	}
	afterAttempts, afterUsage := snapshot(admin)
	if afterAttempts != beforeAttempts || afterUsage != beforeUsage {
		t.Errorf("rollback changed rows:\n%s\n%s\nwant\n%s\n%s", afterAttempts, afterUsage, beforeAttempts, beforeUsage)
	}
	after := platformFunctions(t, admin)
	for sig, def := range beforeFunctions {
		if after[sig] != def {
			t.Errorf("rollback changed %s:\n%s\nwant\n%s", sig, after[sig], def)
		}
	}
	for sig := range after {
		if _, ok := beforeFunctions[sig]; !ok {
			t.Errorf("rollback left %s", sig)
		}
	}
}

type ledgerFixture struct {
	db            *testdb.DB
	owner, member uuid.UUID
}

func newLedgerFixture(t *testing.T) ledgerFixture {
	t.Helper()
	f := newScraperFixture(t)
	return ledgerFixture{db: f.db, owner: f.owner, member: f.member}
}

// join puts the member on a plan of weekly cap week (USD).
func (f ledgerFixture) join(t *testing.T, week string) {
	t.Helper()
	ctx := context.Background()
	w, _ := modelapi.ParseUSD(week)
	plan := uuid.New()
	if err := f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT platform.save_plan($1,'P',$2,$2,NULL,0)`, plan, w); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT platform.save_membership($1,$2,'active',0)`, f.member, plan)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f ledgerFixture) open(t *testing.T, cost, price int64) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	reason, err := f.db.Store.OpenScrapeCharge(context.Background(), f.member, id, "firecrawl", "jobs.example.com", price, cost, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return id, reason
}

func (f ledgerFixture) spent(t *testing.T) (week, held int64) {
	t.Helper()
	if err := f.db.Admin(t).QueryRow(context.Background(), `SELECT week,held FROM platform.spend($1,now())`, f.member).Scan(&week, &held); err != nil {
		t.Fatal(err)
	}
	return week, held
}

func TestScrapeChargesBillThePlan(t *testing.T) {
	f := newLedgerFixture(t)
	ctx := context.Background()
	usd := func(s string) int64 { n, _ := modelapi.ParseUSD(s); return n }

	// Without a plan only free scrapes run, and they are still recorded.
	free, reason := f.open(t, 0, 0)
	if reason != "" {
		t.Fatalf("free scrape refused: %s", reason)
	}
	if err := f.db.Store.CloseScrapeCharge(ctx, f.member, free, 1); err != nil {
		t.Fatal(err)
	}
	if _, reason := f.open(t, usd("0.5"), usd("0.1")); reason != "membership_required" {
		t.Fatalf("priced scrape without a plan: %q", reason)
	}

	f.join(t, "1")
	id, reason := f.open(t, usd("0.5"), usd("0.1"))
	if reason != "" {
		t.Fatalf("priced scrape refused: %s", reason)
	}
	if _, held := f.spent(t); held != usd("0.5") {
		t.Fatalf("held %d", held)
	}
	if err := f.db.Store.CloseScrapeCharge(ctx, f.member, id, 2); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Store.CloseScrapeCharge(ctx, f.member, id, 3); err != nil { // settling again does nothing
		t.Fatal(err)
	}
	if week, held := f.spent(t); week != usd("0.2") || held != 0 {
		t.Fatalf("spent %d held %d, want 0.2 and 0", week, held)
	}
	if _, reason := f.open(t, usd("0.9"), usd("0.1")); reason != "usage_limit" {
		t.Fatalf("over the cap: %q", reason)
	}

	// A charge its process abandoned settles at what it reserved.
	stale, _ := f.open(t, usd("0.3"), usd("0.1"))
	if _, err := f.db.Admin(t).Exec(ctx, `UPDATE platform.charge SET deadline_at=now()-interval '1 second' WHERE id=$1`, stale); err != nil {
		t.Fatal(err)
	}
	ws, err := store.Open(ctx, f.db.WorkerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var n int
	if err := ws.Pool().QueryRow(ctx, `SELECT platform.expire_charges()`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	if week, _ := f.spent(t); week != usd("0.5") {
		t.Fatalf("after expiry spent %d", week)
	}
	var entries string
	if err := f.db.Store.InTenant(ctx, f.member, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.usage_entries(NULL,10)::text`).Scan(&entries)
	}); err != nil || !strings.Contains(entries, `"scrape"`) {
		t.Fatalf("entries = %s, %v", entries, err)
	}
	// The worker does not scrape, so it cannot open scrape charges.
	if err := ws.InTenant(ctx, f.member, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.open_scrape_charge($1,'edge','x',0,0,now()+interval '1 minute')`, uuid.New())
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("worker opened a scrape charge: %v", err)
	}
}

func TestCreditPriceInScraperSettings(t *testing.T) {
	f := newScraperFixture(t)
	if _, err := f.save(f.owner, `[{"kind":"firecrawl","credit_usd":"0.0053"}]`, `[]`, 1); err != nil {
		t.Fatalf("priced provider: %v", err)
	}
	for _, providers := range []string{`[{"kind":"edge","credit_usd":"0.01"}]`, `[{"kind":"firecrawl","credit_usd":"-1"}]`, `[{"kind":"firecrawl","credit_usd":1}]`} {
		if _, err := f.save(f.owner, providers, `[]`, 2); pgCode(err) != "ZA422" {
			t.Errorf("%s: %v", providers, err)
		}
	}
}
