package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/edgetoken"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

type edgeWorker struct {
	id    uuid.UUID
	hash  []byte
	token string
}

// worker registers an edge worker as the owner and marks it live, as a first
// poll does, so jobs for its hosts can be queued.
func (f scraperFixture) worker(t *testing.T, hosts []string, perHour, concurrent int) edgeWorker {
	t.Helper()
	token, hash, err := edgetoken.New()
	if err != nil {
		t.Fatal(err)
	}
	w := edgeWorker{id: uuid.New(), hash: hash, token: token}
	err = f.db.Store.InTenant(context.Background(), f.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `SELECT platform.create_edge_worker($1,$2,$3,$4,$5,$6)`, w.id, "laptop", hash, hosts, perHour, concurrent)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Store.ClaimScrapeJob(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	return w
}

func (f scraperFixture) enqueue(t *testing.T, rawURL string) (uuid.UUID, bool) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	id, ok, err := f.db.Store.EnqueueScrapeJob(context.Background(), rawURL, u.Hostname(), 60)
	if err != nil {
		t.Fatal(err)
	}
	return id, ok
}

func (f scraperFixture) claim(t *testing.T, w edgeWorker) *store.EdgeJob {
	t.Helper()
	j, err := f.db.Store.ClaimScrapeJob(context.Background(), w.hash)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func done(md string) store.EdgeResult {
	return store.EdgeResult{Status: "done", Markdown: md, RawHTML: "<p>" + md + "</p>", Metadata: map[string]any{"ogTitle": "t"}, StatusCode: 200}
}

func TestEdgeClaimOrderAndHosts(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	li := f.worker(t, []string{"linkedin.com"}, 100, 1)
	indeed := f.worker(t, []string{"indeed.com"}, 100, 1)
	noHosts := f.worker(t, []string{}, 100, 1)

	first, _ := f.enqueue(t, "https://www.linkedin.com/jobs/view/1")
	second, _ := f.enqueue(t, "https://linkedin.com/jobs/view/2")
	other, ok := f.enqueue(t, "https://es.indeed.com/viewjob?jk=3")
	if !ok {
		t.Fatal("indeed subdomain should be queued for its worker")
	}
	// Neither a sibling domain that merely ends in the same letters nor a
	// host nobody may open is queued.
	for _, u := range []string{"https://evillinkedin.com/x", "https://example.org/"} {
		if _, ok := f.enqueue(t, u); ok {
			t.Errorf("%s queued without an allowed worker", u)
		}
	}

	if j := f.claim(t, noHosts); j != nil {
		t.Errorf("a worker with no allowed hosts claimed %v", j)
	}
	j := f.claim(t, li)
	if j == nil || j.ID != first || j.URL != "https://www.linkedin.com/jobs/view/1" {
		t.Fatalf("first claim = %+v, want the oldest job %s", j, first)
	}
	if j := f.claim(t, li); j != nil {
		t.Errorf("max_concurrent 1: claimed %+v while one is held", j)
	}
	if err := f.db.Store.FinishScrapeJob(ctx, li.hash, first, done("one")); err != nil {
		t.Fatal(err)
	}
	if j := f.claim(t, li); j == nil || j.ID != second {
		t.Fatalf("second claim = %+v, want %s", j, second)
	}
	if j := f.claim(t, indeed); j == nil || j.ID != other {
		t.Fatalf("indeed claim = %+v, want %s", j, other)
	}
	if j := f.claim(t, indeed); j != nil {
		t.Errorf("nothing is left for indeed: %+v", j)
	}

	// "*" alone opens any host, so a wildcard worker takes a host nobody else may.
	anyWorker := f.worker(t, []string{"*"}, 100, 1)
	anyJob, ok := f.enqueue(t, "https://example.org/any")
	if !ok {
		t.Fatal("a wildcard worker should queue any host")
	}
	if j := f.claim(t, anyWorker); j == nil || j.ID != anyJob {
		t.Fatalf("wildcard claim = %+v, want %s", j, anyJob)
	}
}

func TestEdgeLimits(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	w := f.worker(t, []string{"example.com"}, 2, 5)
	var ids []uuid.UUID
	for i := 0; i < 4; i++ {
		id, ok := f.enqueue(t, "https://example.com/p"+string(rune('a'+i)))
		if !ok {
			t.Fatalf("job %d not queued", i)
		}
		ids = append(ids, id)
	}
	for i := 0; i < 2; i++ {
		if j := f.claim(t, w); j == nil || j.ID != ids[i] {
			t.Fatalf("claim %d = %+v", i, j)
		}
	}
	if j := f.claim(t, w); j != nil {
		t.Errorf("max_per_hour 2: third claim got %+v", j)
	}
	// With its hour used up the worker is not a reason to queue more.
	if _, ok := f.enqueue(t, "https://example.com/more"); ok {
		t.Error("queued a job for a worker with no capacity left")
	}
	// An hour on, its quota is back.
	if _, err := f.db.Admin(t).Exec(ctx, `UPDATE platform.scrape_job SET claimed_at=now()-interval '61 minutes',deadline=now()+interval '1 minute' WHERE status='claimed'`); err != nil {
		t.Fatal(err)
	}
	if j := f.claim(t, w); j == nil || j.ID != ids[2] {
		t.Errorf("claim after the hour = %+v", j)
	}

	// Concurrency: a worker allowed 1 at a time takes the next only after finishing.
	g := newScraperFixture(t)
	one := g.worker(t, []string{"example.com"}, 100, 1)
	a, _ := g.enqueue(t, "https://example.com/a")
	g.enqueue(t, "https://example.com/b")
	if j := g.claim(t, one); j == nil || j.ID != a {
		t.Fatalf("claim = %+v", j)
	}
	if j := g.claim(t, one); j != nil {
		t.Errorf("concurrent claim = %+v", j)
	}
	if err := g.db.Store.FinishScrapeJob(ctx, one.hash, a, done("a")); err != nil {
		t.Fatal(err)
	}
	if j := g.claim(t, one); j == nil {
		t.Error("no claim after finishing")
	}
}

// Workers poll at once: every job goes to exactly one of them.
func TestEdgeClaimsDoNotOverlap(t *testing.T) {
	f := newScraperFixture(t)
	const jobs, workers = 24, 6
	var ws []edgeWorker
	for i := 0; i < workers; i++ {
		ws = append(ws, f.worker(t, []string{"example.com"}, 600, 10))
	}
	for i := 0; i < jobs; i++ {
		if _, ok := f.enqueue(t, "https://example.com/"+uuid.NewString()); !ok {
			t.Fatal("not queued")
		}
	}
	var mu sync.Mutex
	got := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := f.db.Store.ClaimScrapeJob(context.Background(), w.hash)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				got[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != jobs {
		t.Fatalf("claimed %d distinct jobs, want %d", len(got), jobs)
	}
	for id, n := range got {
		if n != 1 {
			t.Errorf("job %s claimed %d times", id, n)
		}
	}
}

func TestEdgeAuthenticationAndOwnership(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	a := f.worker(t, []string{"example.com"}, 100, 5)
	b := f.worker(t, []string{"example.com"}, 100, 5)
	id, _ := f.enqueue(t, "https://example.com/x")

	if _, err := f.db.Store.ClaimScrapeJob(ctx, edgetoken.Hash("nope")); !errors.Is(err, store.ErrEdgeUnauthorized) {
		t.Errorf("unknown token: %v", err)
	}
	// Not yet claimed: nobody can finish it.
	if err := f.db.Store.FinishScrapeJob(ctx, a.hash, id, done("x")); !errors.Is(err, store.ErrScrapeJobGone) {
		t.Errorf("finishing an unclaimed job: %v", err)
	}
	if j := f.claim(t, a); j == nil || j.ID != id {
		t.Fatalf("claim = %+v", j)
	}
	if err := f.db.Store.FinishScrapeJob(ctx, b.hash, id, done("stolen")); !errors.Is(err, store.ErrScrapeJobGone) {
		t.Errorf("another worker finished the job: %v", err)
	}
	if err := f.db.Store.FinishScrapeJob(ctx, a.hash, id, store.EdgeResult{Status: "weird"}); err == nil {
		t.Error("an invalid status was accepted")
	}
	if err := f.db.Store.FinishScrapeJob(ctx, a.hash, id, done("mine")); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Store.FinishScrapeJob(ctx, a.hash, id, done("twice")); !errors.Is(err, store.ErrScrapeJobGone) {
		t.Errorf("a finished job was finished again: %v", err)
	}

	// The API reads the result once.
	out, err := f.db.Store.ScrapeJobResult(ctx, id, false)
	if err != nil || out.Status != "done" || out.Markdown != "mine" || out.RawHTML != "<p>mine</p>" || out.Metadata["ogTitle"] != "t" || *out.StatusCode != 200 {
		t.Fatalf("result = %+v (%v)", out, err)
	}
	out, err = f.db.Store.ScrapeJobResult(ctx, id, false)
	if err != nil || out.Status != "done" || out.Markdown != "" || out.RawHTML != "" || out.Metadata != nil {
		t.Errorf("second read kept the result: %+v (%v)", out, err)
	}

	// A disabled worker and a rotated token stop working at once.
	if err := f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.update_edge_worker($1,NULL,false,NULL,NULL,NULL,1)`, b.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Store.ClaimScrapeJob(ctx, b.hash); !errors.Is(err, store.ErrEdgeUnauthorized) {
		t.Errorf("disabled worker: %v", err)
	}
	newToken, newHash, _ := edgetoken.New()
	_ = newToken
	if err := f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.rotate_edge_worker_token($1,$2)`, a.id, newHash)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Store.ClaimScrapeJob(ctx, a.hash); !errors.Is(err, store.ErrEdgeUnauthorized) {
		t.Errorf("old token after rotation: %v", err)
	}
	if _, err := f.db.Store.ClaimScrapeJob(ctx, newHash); err != nil {
		t.Errorf("new token: %v", err)
	}
}

func TestEdgeExpiryAndPruning(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	admin := f.db.Admin(t)
	w := f.worker(t, []string{"example.com"}, 100, 5)

	// A caller that gave up expires the job: a worker cannot claim it, and a
	// result that arrives late is refused and kept nowhere.
	gone, _ := f.enqueue(t, "https://example.com/gone")
	out, err := f.db.Store.ScrapeJobResult(ctx, gone, true)
	if err != nil || out.Status != "expired" {
		t.Fatalf("cancel = %+v (%v)", out, err)
	}
	if j := f.claim(t, w); j != nil {
		t.Errorf("claimed an expired job: %+v", j)
	}

	// Claimed, then the caller cancels while the worker is still reading.
	slow, _ := f.enqueue(t, "https://example.com/slow")
	if j := f.claim(t, w); j == nil || j.ID != slow {
		t.Fatalf("claim = %+v", j)
	}
	if _, err := f.db.Store.ScrapeJobResult(ctx, slow, true); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Store.FinishScrapeJob(ctx, w.hash, slow, done("late")); !errors.Is(err, store.ErrScrapeJobGone) {
		t.Errorf("late result accepted: %v", err)
	}
	var kept int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM platform.scrape_job WHERE result_markdown IS NOT NULL OR result_html IS NOT NULL`).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("results kept = %d (%v)", kept, err)
	}

	// A job past its deadline is not claimed and is marked expired by the next enqueue.
	stale, _ := f.enqueue(t, "https://example.com/stale")
	if _, err := admin.Exec(ctx, `UPDATE platform.scrape_job SET deadline=now()-interval '1 second' WHERE id=$1`, stale); err != nil {
		t.Fatal(err)
	}
	if j := f.claim(t, w); j != nil {
		t.Errorf("claimed a job past its deadline: %+v", j)
	}
	next, _ := f.enqueue(t, "https://example.com/next")
	if j := f.claim(t, w); j == nil || j.ID != next {
		t.Fatalf("claim = %+v", j)
	}
	if err := f.db.Store.FinishScrapeJob(ctx, w.hash, next, store.EdgeResult{Status: "failed", Error: "x"}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := admin.QueryRow(ctx, `SELECT status FROM platform.scrape_job WHERE id=$1`, stale).Scan(&status); err != nil || status != "expired" {
		t.Errorf("stale job status = %q (%v)", status, err)
	}

	// An unread result is dropped after an hour; the row after the stats window.
	read, _ := f.enqueue(t, "https://example.com/unread")
	if j := f.claim(t, w); j == nil || j.ID != read {
		t.Fatalf("claim = %+v", j)
	}
	if err := f.db.Store.FinishScrapeJob(ctx, w.hash, read, done("unread")); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.scrape_job SET finished_at=now()-interval '2 hours' WHERE status='done'`); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, "https://example.com/prune")
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM platform.scrape_job WHERE status='done' AND (result_markdown IS NOT NULL OR result_html IS NOT NULL OR result_metadata IS NOT NULL)`).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("results older than an hour kept = %d (%v)", kept, err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.scrape_job SET finished_at=now()-interval '26 hours' WHERE status IN ('done','expired')`); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, "https://example.com/prune2")
	var old int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM platform.scrape_job WHERE status IN ('done','expired') AND finished_at<now()-interval '25 hours'`).Scan(&old); err != nil || old != 0 {
		t.Errorf("rows older than 25 hours = %d (%v)", old, err)
	}
}

func TestEdgeWorkerAdministration(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	w := f.worker(t, []string{"linkedin.com"}, 20, 1)

	// Owner only.
	for name, sql := range map[string]string{
		"list":   `SELECT platform.edge_workers()::text`,
		"create": `SELECT platform.create_edge_worker(gen_random_uuid(),'x','\x0000000000000000000000000000000000000000000000000000000000000000'::bytea,'{}',1,1)::text`,
		"update": `SELECT platform.update_edge_worker(gen_random_uuid(),'x',true,NULL,NULL,NULL,1)::text`,
		"rotate": `SELECT platform.rotate_edge_worker_token(gen_random_uuid(),'\x00'::bytea)::text`,
	} {
		err := f.db.Store.InTenant(ctx, f.member, func(tx pgx.Tx) error {
			var s string
			return tx.QueryRow(ctx, sql).Scan(&s)
		})
		if pgCode(err) != "ZA403" {
			t.Errorf("member %s: %v, want ZA403", name, err)
		}
	}

	var list string
	if err := f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.edge_workers()::text`).Scan(&list)
	}); err != nil {
		t.Fatal(err)
	}
	var workers []struct {
		ID           uuid.UUID `json:"id"`
		AllowedHosts []string  `json:"allowed_hosts"`
		Version      int       `json:"version"`
		LastSeenAt   *string   `json:"last_seen_at"`
		Jobs24h      int       `json:"jobs_24h"`
		Done24h      int       `json:"done_24h"`
		Failed24h    int       `json:"failed_24h"`
	}
	if err := json.Unmarshal([]byte(list), &workers); err != nil || len(workers) != 1 || workers[0].ID != w.id || workers[0].LastSeenAt == nil || strings.Contains(list, "token") {
		t.Fatalf("list = %s (%v)", list, err)
	}

	// Stats: one done and one failed job in the last 24 hours.
	for _, res := range []store.EdgeResult{done("ok"), {Status: "failed", StatusCode: 999, Error: "blocked"}} {
		id, _ := f.enqueue(t, "https://linkedin.com/jobs/view/"+uuid.NewString())
		if j := f.claim(t, w); j == nil || j.ID != id {
			t.Fatalf("claim = %+v", j)
		}
		if err := f.db.Store.FinishScrapeJob(ctx, w.hash, id, res); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.update_edge_worker($1,'renamed',NULL,'{indeed.com,linkedin.com}',5,NULL,$2)::text`, w.id, workers[0].Version).Scan(&list)
	}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`[`+list+`]`), &workers); err != nil || workers[0].Jobs24h != 2 || workers[0].Done24h != 1 || workers[0].Failed24h != 1 || len(workers[0].AllowedHosts) != 2 || workers[0].Version != 2 {
		t.Fatalf("updated = %s (%v)", list, err)
	}
	// A stale version, an invalid host and an out-of-range limit are refused.
	for name, tc := range map[string]struct{ sql, code string }{
		"stale version": {`SELECT platform.update_edge_worker($1,NULL,false,NULL,NULL,NULL,1)::text`, "ZA412"},
		"bad host":      {`SELECT platform.update_edge_worker($1,NULL,NULL,'{https://x.com}',NULL,NULL,2)::text`, "23514"},
		"upper host":    {`SELECT platform.update_edge_worker($1,NULL,NULL,'{LinkedIn.com}',NULL,NULL,2)::text`, "23514"},
		"zero limit":    {`SELECT platform.update_edge_worker($1,NULL,NULL,NULL,0,NULL,2)::text`, "23514"},
	} {
		err := f.db.Store.InTenant(ctx, f.owner, func(tx pgx.Tx) error {
			var s string
			return tx.QueryRow(ctx, tc.sql, w.id).Scan(&s)
		})
		if pgCode(err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}

	// Audited, and the audit never holds a token hash.
	var n int
	if err := f.db.Admin(t).QueryRow(ctx, `SELECT count(*) FROM platform.audit WHERE action LIKE 'edge_worker.%' AND detail::text !~* 'hash|token_hash'`).Scan(&n); err != nil || n < 2 {
		t.Errorf("audit rows = %d (%v)", n, err)
	}
}

func TestEdgeProviderInSettings(t *testing.T) {
	f := newScraperFixture(t)
	if _, err := f.save(f.owner, `[{"kind":"edge"},{"kind":"firecrawl"}]`, `[{"host":"linkedin.com","providers":[0,1]}]`, 1); err != nil {
		t.Fatalf("save edge provider: %v", err)
	}
	for name, providers := range map[string]string{
		"edge with an address": `[{"kind":"edge","base_url":"http://x:1"}]`,
		"edge with a key":      `[{"kind":"edge","key_id":"` + uuid.NewString() + `"}]`,
	} {
		if _, err := f.save(f.owner, providers, `[]`, 2); pgCode(err) != "ZA422" {
			t.Errorf("%s: %v, want ZA422", name, err)
		}
	}
}

func TestEdgeFunctionsAreForTheAPIOnly(t *testing.T) {
	f := newScraperFixture(t)
	ctx := context.Background()
	admin := f.db.Admin(t)
	for _, fn := range []string{
		"platform.edge_workers()",
		"platform.create_edge_worker(uuid,text,bytea,text[],integer,integer)",
		"platform.update_edge_worker(uuid,text,boolean,text[],integer,integer,integer)",
		"platform.rotate_edge_worker_token(uuid,bytea)",
		"platform.enqueue_scrape_job(text,text,integer)",
		"platform.claim_scrape_job(bytea)",
		"platform.finish_scrape_job(bytea,uuid,text,text,text,jsonb,integer,text)",
		"platform.scrape_job_result(uuid,boolean)",
		"platform.expire_scrape_jobs()",
		"platform.edge_authenticate(bytea)",
	} {
		app := fn != "platform.expire_scrape_jobs()" && fn != "platform.edge_authenticate(bytea)"
		for role, want := range map[string]bool{"zapply_app": app, "zapply_worker": false, "zapply_relay": false, "public": false} {
			var ok bool
			if err := admin.QueryRow(ctx, `SELECT has_function_privilege($1,$2,'EXECUTE')`, role, fn).Scan(&ok); err != nil || ok != want {
				t.Errorf("%s execute %s = %v (%v), want %v", role, fn, ok, err, want)
			}
		}
	}
	for _, table := range []string{"platform.edge_worker", "platform.scrape_job"} {
		var ok bool
		if err := admin.QueryRow(ctx, `SELECT has_table_privilege('zapply_app',$1,'SELECT,INSERT,UPDATE,DELETE')`, table).Scan(&ok); err != nil || ok {
			t.Errorf("the API role reads %s directly", table)
		}
	}
}

func edgeMigrationDown(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("migrations/00035_scraper_edge.sql")
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitN(string(source), "-- +goose Down", 2)[1]
}

// The migration login can SET ROLE zapply_platform but inherits nothing from
// it, as in production: migration 34 must still apply and roll back.
func TestEdgeMigrationWithoutInheritedPlatformOwnership(t *testing.T) {
	db := testdb.NewAtVersion(t, 34)
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
	if err := store.MigrateTo(ctx, migrationURL.String(), 35, discardLogger()); err != nil {
		t.Fatalf("migration: %v", err)
	}
	conn, err := pgx.Connect(ctx, migrationURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, edgeMigrationDown(t)); return err }); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var left int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname IN ('edge_workers','create_edge_worker','update_edge_worker','rotate_edge_worker_token','enqueue_scrape_job','claim_scrape_job','finish_scrape_job','scrape_job_result','expire_scrape_jobs','edge_authenticate','edge_worker_json','edge_hosts_valid')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rollback left %d functions (%v)", left, err)
	}
}

func TestEdgeMigrationRollback(t *testing.T) {
	db := testdb.NewAtVersion(t, 34)
	ctx := context.Background()
	admin := db.Admin(t)
	if err := store.MigrateTo(ctx, db.AdminURL, 35, discardLogger()); err != nil {
		t.Fatalf("up: %v", err)
	}
	down := edgeMigrationDown(t)
	rollback := func() error {
		return pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, down); return err })
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.scraper_settings SET providers='[{"kind":"edge"}]'`); err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err == nil || !strings.Contains(err.Error(), "edge") {
		t.Fatalf("rollback with an edge provider: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.scraper_settings SET providers='[]'`); err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.scraper_settings SET providers='[{"kind":"edge"}]'`); pgCode(err) != "23514" {
		t.Errorf("restored validator accepted edge: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.scraper_settings SET providers='[{"kind":"crawl4ai","base_url":"http://c:1"}]'`); err != nil {
		t.Errorf("restored validator rejected crawl4ai: %v", err)
	}
}
