package catalog_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/membership"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/worker"
)

type env struct {
	db      *testdb.DB
	worker  *store.Store
	queue   *worker.Queue
	catalog *catalog.Catalog
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	ws, err := store.Open(ctx, db.WorkerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ws.Close)
	q, err := worker.NewQueue(db.Store)
	if err != nil {
		t.Fatal(err)
	}
	return &env{db, ws, q, catalog.New(ws, q, slog.New(slog.NewTextHandler(io.Discard, nil)))}
}

func (e *env) add(t *testing.T, ss ...catalog.Sighting) []uuid.UUID {
	t.Helper()
	ids, err := e.catalog.Add(context.Background(), ss)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.worker.Pool().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func board(key, title, location string) catalog.Sighting {
	return catalog.Sighting{Source: catalog.SourceATS, ATSKey: "lever:acme:" + key, Company: "Acme Inc.", Title: title, Location: location,
		URL: "https://jobs.lever.co/acme/" + key}
}

func TestIdentityResolution(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.add(t, board("1", "Staff Engineer", "New York, NY"))[0]

	// The same ATS posting is the same posting, even retitled.
	if id := e.add(t, board("1", "Staff Engineer (Platform)", "New York, NY"))[0]; id != first {
		t.Fatalf("same ATS posting resolved to a new one")
	}
	// Another ATS posting with the same company, title and country is a
	// second opening: board postings match only by ATS key.
	second := e.add(t, board("2", "Staff Engineer", "Brooklyn, NY"))[0]
	if second == first {
		t.Fatal("two ATS postings merged by fingerprint")
	}
	// An aggregator's sighting of it matches by fingerprint (country, not
	// city), and one of the same link matches by URL.
	agg := e.add(t,
		catalog.Sighting{Source: catalog.SourceSearch, Company: "ACME", Title: "Staff Engineer", Location: "Remote", Country: "US", URL: "https://www.linkedin.com/jobs/view/9"},
		catalog.Sighting{Source: catalog.SourceSearch, Company: "Other name", Title: "Different", URL: "https://jobs.lever.co/acme/2?utm_source=linkedin"},
	)
	if agg[0] != first || agg[1] != second {
		t.Fatalf("aggregator sightings = %v, want %v and %v", agg, first, second)
	}
	// A later sighting fills what the first lacked, and adds its identities.
	var country, hint *string
	if err := e.worker.Pool().QueryRow(ctx, `SELECT country,seniority_hint FROM platform.posting WHERE id=$1`, first).Scan(&country, &hint); err != nil || country == nil || *country != "US" {
		t.Fatalf("country = %v, %v", country, err)
	}
	if n := e.count(t, `SELECT count(*) FROM platform.posting_identity WHERE posting_id=$1`, first); n != 5 { // ats, url, both titles' fp, linkedin url
		t.Errorf("first has %d identities, want 5", n)
	}
	// A lapsed fingerprint names a new opening, which takes the key over.
	if _, err := e.db.Store.Pool().Exec(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, e.db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `UPDATE platform.posting_identity SET seen_at=now()-interval '61 days' WHERE key LIKE 'fp:%'`); err != nil {
		t.Fatal(err)
	}
	reposted := e.add(t, catalog.Sighting{Source: catalog.SourceSearch, Company: "Acme", Title: "Staff Engineer", Country: "US", URL: "https://example.com/jobs/77"})[0]
	if reposted == first || reposted == second {
		t.Fatal("lapsed fingerprint still matched")
	}
	if n := e.count(t, `SELECT count(*) FROM platform.posting_identity WHERE key='fp:acme|staff engineer|us' AND posting_id=$1`, reposted); n != 1 {
		t.Error("lapsed fingerprint not taken over")
	}
	// Each new posting waits for both stages; invalid sightings are skipped.
	if ids := e.add(t, catalog.Sighting{Title: "No link", URL: "http://insecure"}); ids[0] != uuid.Nil {
		t.Error("stored a posting without an https link")
	}
	if n := e.count(t, `SELECT count(*) FROM platform.posting_stage WHERE NOT done`); n != 6 {
		t.Errorf("%d stages waiting, want 6", n)
	}
}

func TestConcurrentSightingsMakeOnePosting(t *testing.T) {
	e := newEnv(t)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := e.catalog.Add(context.Background(), []catalog.Sighting{board("7", "Designer", "Austin, TX")})
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM platform.posting`); n != 1 {
		t.Fatalf("%d postings, want 1", n)
	}
}

// model answers decisions and extractions like a well-behaved model, or
// with garbage while broken is set.
type model struct {
	calls  atomic.Int32
	broken atomic.Bool
}

func (m *model) Quote(context.Context) (modelapi.Quote, error) {
	return modelapi.Quote{}, errors.New("unused")
}
func (m *model) Generate(context.Context, string, modelapi.Quote) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("unused")
}
func (m *model) Lookup(context.Context, string, string) (modelapi.Result, error) {
	return modelapi.Result{}, errors.New("unused")
}
func sheet(name string) billing.Sheet {
	return billing.Sheet{Provider: "test", Model: name, InputRate: 1000, Context: 32000, InputPerQuestion: true, MaxRequestBytes: extraction.RequestBudget,
		Prior: billing.Tokenization{TokensPerKB: 1000, OverheadTokens: 16}, FetchedAt: time.Now()}
}
func (m *model) DecisionSheet(_ context.Context, name string) (billing.Sheet, error) {
	return sheet(name), nil
}
func (m *model) DecisionLookupSupported(string) bool { return false }
func (m *model) ExtractionSheet(_ context.Context, name string) (billing.Sheet, error) {
	return sheet(name), nil
}
func charged(text string) (modelapi.Result, error) {
	amount := int64(2_580_000) // $0.00000258
	return modelapi.Result{GenerationID: "gen-" + uuid.NewString(), Text: text, Amount: &amount, Usage: json.RawMessage(`{}`)}, nil
}
func (m *model) Decide(_ context.Context, key string, r systemone.Request, _ modelapi.Quote) (modelapi.Result, error) {
	m.calls.Add(1)
	if key != "provider-secret" {
		return modelapi.Result{}, errors.New("wrong key")
	}
	if m.broken.Load() {
		return charged(`{"q1":{"type":"choice","choice":"senior"}}`) // fails validation
	}
	picks := map[string]string{"senior": "senior", "engineering_software": "engineering_software", "full_time": "full_time", "unstated": "5"}
	out := map[string]systemone.Answer{}
	for key, q := range r.Questions {
		if q.Type == "noul" {
			p := 0.1
			out[key] = systemone.Answer{Type: "noul", Noul: &p}
			continue
		}
		var criteria map[string]json.RawMessage
		_ = json.Unmarshal(q.Criteria, &criteria)
		choice := ""
		for k := range criteria {
			if v, ok := picks[k]; ok {
				choice = v
			}
		}
		probs := map[string]float64{}
		for k := range criteria {
			probs[k] = 0.1 / float64(len(criteria)-1)
		}
		probs[choice] = 0.9
		if choice == "engineering_software" { // a Solutions Engineer
			probs[choice], probs["sales"] = 0.6, 0.3+probs["sales"]
		}
		c := 0.85
		out[key] = systemone.Answer{Type: "choice", Choice: choice, Probabilities: probs, Confidence: &c}
	}
	raw, _ := json.Marshal(out)
	return charged(string(raw))
}
func (m *model) Extract(_ context.Context, key string, r extraction.Request, _ modelapi.Quote) (modelapi.Result, error) {
	m.calls.Add(1)
	return charged(`{"specialty":{"type":"text","value":"sales engineering","confidence":0.9},"countries":{"type":"list","value":["MX","United States"]},"languages":{"type":"list","value":["es","en"]}}`)
}

func setupLedger(t *testing.T, e *env) (*worker.Runtime, *model) {
	t.Helper()
	ctx := context.Background()
	owner, err := e.db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOwner(ctx, e.db.AdminURL, owner, true); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	s := membership.New(e.db.Store, ring)
	key, err := s.AddKey(ctx, owner, "Primary", "provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateKey(ctx, owner, key, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Store.InTenant(ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.save_posting_models('liquid/d1.2','cloudflare/clairic')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m := &model{}
	r := &worker.Runtime{Store: e.worker, Provider: m, Keys: ring, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.OnTerminal = func(ctx context.Context, tx pgx.Tx, op worker.Operation) error {
		_, err := e.catalog.OnTerminal(ctx, tx, op)
		return err
	}
	return r, m
}

// run processes every open classification operation once.
func (e *env) run(t *testing.T, r *worker.Runtime) int {
	t.Helper()
	ctx := context.Background()
	rows, err := e.worker.Pool().Query(ctx, `SELECT operation_id FROM platform.posting_stage WHERE operation_id IS NOT NULL AND NOT done`)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if err := r.Process(ctx, worker.Args{UserID: catalog.SystemMember, OperationID: op}); err != nil {
			t.Fatalf("process %s: %v", op, err)
		}
	}
	return len(ops)
}

func TestClassification(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Without models nothing is sent.
	id := e.add(t, catalog.Sighting{Source: catalog.SourceSearch, Company: "Acme", Title: "Solutions Engineer", Location: "CDMX", URL: "https://example.com/1",
		Description: "Buscamos ingeniero de soluciones bilingüe.", SeniorityHint: "senior"})[0]
	if n, err := e.catalog.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("sweep without models = %d, %v", n, err)
	}
	r, m := setupLedger(t, e)
	if n, err := e.catalog.Sweep(ctx); err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	// Open stages are not sent twice.
	if n, err := e.catalog.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep = %d, %v", n, err)
	}
	if n := e.run(t, r); n != 2 {
		t.Fatalf("ran %d operations", n)
	}
	var seniority, employment, specialty, decisionModel, extractionModel string
	var areas, countries, languages []string
	var remote bool
	var minYears *int
	var version int
	var conf map[string]float64
	if err := e.worker.Pool().QueryRow(ctx, `SELECT version,seniority,min_years,areas,specialty,countries,remote,employment_type,languages,confidence,decision_model,extraction_model
 FROM platform.posting_label WHERE posting_id=$1`, id).Scan(&version, &seniority, &minYears, &areas, &specialty, &countries, &remote, &employment, &languages, &conf, &decisionModel, &extractionModel); err != nil {
		t.Fatal(err)
	}
	if version != catalog.LabelVersion || seniority != "senior" || minYears == nil || *minYears != 5 || len(areas) != 2 || areas[0] != "engineering_software" || areas[1] != "sales" ||
		specialty != "sales engineering" || len(countries) != 2 || countries[0] != "MX" || countries[1] != "US" || remote || employment != "full_time" ||
		len(languages) != 2 || decisionModel != "liquid/d1.2" || extractionModel != "cloudflare/clairic" {
		t.Fatalf("labels: v%d %s %v %v %s %v %v %s %v %s %s", version, seniority, minYears, areas, specialty, countries, remote, employment, languages, decisionModel, extractionModel)
	}
	if conf["seniority"] != 0.85 || conf["remote"] != 0.9 {
		t.Errorf("confidence = %v", conf)
	}
	// Both calls were charged to the catalog's member.
	admin, err := pgx.Connect(ctx, e.db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	adminCount := func(q string) int {
		var n int
		if err := admin.QueryRow(ctx, q, catalog.SystemMember).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := adminCount(`SELECT count(*) FROM platform.charge WHERE user_id=$1 AND state='settled'`); n != 2 {
		t.Errorf("%d settled charges on the catalog's member", n)
	}
	if n := adminCount(`SELECT count(*) FROM platform.charge WHERE user_id<>$1`); n != 0 {
		t.Errorf("%d charges on other members", n)
	}
	// Answers that fail validation retry the stage, then give it up.
	m.broken.Store(true)
	other := e.add(t, catalog.Sighting{Source: catalog.SourceSearch, Company: "Beta", Title: "Practicante de ventas", URL: "https://example.com/2"})[0]
	for i := 0; i < 2; i++ {
		if _, err := e.catalog.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		e.run(t, r)
	}
	var attempts int
	var failed bool
	if err := e.worker.Pool().QueryRow(ctx, `SELECT attempts,failed FROM platform.posting_stage WHERE posting_id=$1 AND stage='decision' AND done`, other).Scan(&attempts, &failed); err != nil || attempts != 2 || !failed {
		t.Fatalf("broken decision: %d attempts, failed %v, %v", attempts, failed, err)
	}
	if n, err := e.catalog.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("sweep after giving up = %d, %v", n, err)
	}
	// A posting identical to a labeled one reuses its cached decision.
	m.broken.Store(false)
	before := m.calls.Load()
	if _, err := e.worker.Pool().Exec(ctx, `UPDATE platform.posting_stage SET done=false,operation_id=NULL WHERE posting_id=$1 AND stage='decision'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.catalog.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if m.calls.Load() != before || e.count(t, `SELECT count(*) FROM platform.posting_stage WHERE posting_id=$1 AND done`, id) != 2 {
		t.Errorf("cached decision called the model again")
	}
}

func TestBackfillIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fake := &boards{postings: []ats.Posting{{Key: "old", Title: "Engineer", Location: "Remote", URL: "https://jobs.lever.co/acme/old"}}}
	api := alerts.New(e.db.Store, fake)
	poller := alerts.New(e.worker, fake) // no catalog: these alerts predate it
	for _, sub := range []string{"alice", "bob"} {
		u, err := e.db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: sub})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := api.Create(ctx, u, alerts.Input{URL: "https://jobs.lever.co/acme"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := poller.Poll(ctx, "lever", "acme"); err != nil { // baseline
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		fake.postings = append(fake.postings, ats.Posting{Key: string(rune('a' + i)), Title: "Role " + string(rune('A'+i)), Location: "Austin, TX",
			URL: "https://jobs.lever.co/acme/" + string(rune('a'+i))})
	}
	if err := poller.Poll(ctx, "lever", "acme"); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM platform.posting`); n != 0 {
		t.Fatalf("%d postings before the backfill", n)
	}
	// Batches of two: each member takes two batches, and both members' alerts
	// of the same posting resolve to one posting.
	res, err := e.catalog.Backfill(ctx, 2)
	if err != nil || res.Members != 2 || res.Alerts != 6 || res.Postings != 3 {
		t.Fatalf("backfill = %+v, %v", res, err)
	}
	if n := e.count(t, `SELECT count(*) FROM platform.posting_identity WHERE key LIKE 'ats:lever:acme:%'`); n != 3 {
		t.Errorf("%d ATS identities", n)
	}
	again, err := e.catalog.Backfill(ctx, 2)
	if err != nil || again.Alerts != 0 || again.Postings != 0 {
		t.Fatalf("second backfill = %+v, %v", again, err)
	}
	// A run stopped part way resumes from its cursor.
	admin, err := pgx.Connect(ctx, e.db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `UPDATE platform.posting_backfill SET done=false`); err != nil {
		t.Fatal(err)
	}
	resumed, err := e.catalog.Backfill(ctx, 2)
	if err != nil || resumed.Alerts != 0 || e.count(t, `SELECT count(*) FROM platform.posting`) != 3 {
		t.Fatalf("resumed backfill = %+v, %v", resumed, err)
	}
}

type boards struct{ postings []ats.Posting }

func (b *boards) Fetch(context.Context, string, string) (ats.Board, error) {
	return ats.Board{Company: "Acme Inc.", Postings: append([]ats.Posting(nil), b.postings...)}, nil
}

// The owner's status report counts each stage, reports the catalog member's
// spend and shows the newest labels; members can't read it.
func TestCatalogStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r, m := setupLedger(t, e)
	owner, err := e.db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	status := func() map[string]any {
		t.Helper()
		var raw []byte
		if err := e.db.Store.InTenant(ctx, owner, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT platform.posting_catalog(20)`).Scan(&raw)
		}); err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	empty := status()
	if empty["postings"] != 0.0 || len(empty["recent"].([]any)) != 0 {
		t.Fatalf("empty catalog: %v", empty)
	}
	if empty["stages"].(map[string]any)["extraction"].(map[string]any)["waiting"] != 0.0 {
		t.Fatalf("empty stages: %v", empty["stages"])
	}
	// One posting labeled by both stages.
	e.add(t, catalog.Sighting{Source: catalog.SourceSearch, Company: "Acme", Title: "Solutions Engineer", Location: "CDMX", URL: "https://example.com/1"})
	if _, err := e.catalog.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	e.run(t, r)
	// A second whose decision fails, with extraction off.
	if err := e.db.Store.InTenant(ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.save_posting_models('liquid/d1.2',NULL)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m.broken.Store(true)
	e.add(t, catalog.Sighting{Source: catalog.SourceSearch, Company: "Beta", Title: "Practicante de ventas", URL: "https://example.com/2"})
	for i := 0; i < 2; i++ {
		if _, err := e.catalog.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		e.run(t, r)
	}
	v := status()
	if v["postings"] != 2.0 || v["postings_24h"] != 2.0 || v["labeled"] != 1.0 || v["waiting"] != 1.0 || v["failed"] != 1.0 {
		t.Errorf("counts: postings %v, 24h %v, labeled %v, waiting %v, failed %v", v["postings"], v["postings_24h"], v["labeled"], v["waiting"], v["failed"])
	}
	stages := v["stages"].(map[string]any)
	want := map[string]map[string]float64{
		"decision":   {"done": 1, "waiting": 0, "running": 0, "failed": 1},
		"extraction": {"done": 1, "waiting": 1, "running": 0, "failed": 0},
	}
	for stage, counts := range want {
		got := stages[stage].(map[string]any)
		for k, n := range counts {
			if got[k] != n {
				t.Errorf("%s %s = %v, want %v", stage, k, got[k], n)
			}
		}
	}
	recent := v["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("recent = %v", recent)
	}
	label := recent[0].(map[string]any)
	if label["title"] != "Solutions Engineer" || label["company"] != "Acme" || label["seniority"] != "senior" || len(label["areas"].([]any)) != 2 ||
		label["decision_model"] != "liquid/d1.2" || label["confidence"].(map[string]any)["seniority"] != 0.85 {
		t.Errorf("label = %v", label)
	}
	usage := v["usage"].(map[string]any)
	if usage["membership"].(map[string]any)["plan_name"] != "Internal: posting catalog" || usage["cap_7d_usd"] == nil || usage["cap_30d_usd"] == nil {
		t.Errorf("usage = %v", usage)
	}
	// Members can't read it.
	member, err := e.db.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.Store.InTenant(ctx, member, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.posting_catalog(20)`)
		return err
	}); err == nil {
		t.Error("a member read the catalog's status")
	}
}
