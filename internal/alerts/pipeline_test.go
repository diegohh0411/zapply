package alerts_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/jobsearch"
	"github.com/diegohh0411/zapply/internal/joburl"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/notify"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/diegohh0411/zapply/internal/worker"
)

// fakeSearch behaves like TheirStack: it returns, oldest first, up to the
// limit of its postings discovered at or after the query's second, charging a
// credit for each. It records the queries.
type fakeSearch struct {
	mu       sync.Mutex
	postings []jobsearch.Posting
	err      error
	queries  []jobsearch.Query
}

func (f *fakeSearch) Search(_ context.Context, _ string, q jobsearch.Query) (jobsearch.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.err != nil {
		return jobsearch.Result{}, f.err
	}
	since := q.Since.Truncate(time.Second)
	var res jobsearch.Result
	for _, p := range f.postings {
		if p.DiscoveredAt.Before(since) || res.Returned == q.Limit || (q.RemoteOnly && !p.Remote) {
			continue
		}
		res.Returned++
		if p.DiscoveredAt.After(res.Newest) {
			res.Newest = p.DiscoveredAt
		}
		res.Postings = append(res.Postings, p)
	}
	res.Credits = res.Returned
	return res, nil
}

type fixture struct {
	d       *testdb.DB
	owner   uuid.UUID
	admin   *membership.Service
	plan    uuid.UUID
	queue   *worker.Queue
	workers *store.Store
	search  *fakeSearch
	alerts  *alerts.Service
	catalog *catalog.Catalog
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	d := testdb.New(t)
	f := &fixture{d: d, search: &fakeSearch{}}
	var err error
	if f.owner, err = d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err = store.SetOwner(ctx, d.AdminURL, f.owner, true); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	f.admin = membership.New(d.Store, ring)
	f.plan = uuid.New()
	if _, err = f.admin.SavePlan(ctx, f.owner, f.plan, "Member", "1", "2", nil, 0); err != nil {
		t.Fatal(err)
	}
	key, err := f.admin.AddKeyOfKind(ctx, f.owner, "TheirStack", "theirstack-secret", "theirstack")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.admin.SaveSearchSettings(ctx, f.owner, &key, "0.01", 10, 1); err != nil {
		t.Fatal(err)
	}
	if f.workers, err = store.Open(ctx, d.WorkerURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.workers.Close)
	if f.queue, err = worker.NewQueue(d.Store); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.catalog = catalog.New(f.workers, nil, log)
	f.alerts = alerts.New(f.workers, f.queue, f.catalog, log).WithSearch(f.search, ring)
	return f
}

type wants struct {
	min, max   string
	areas      []string
	countries  []string
	remoteOK   bool
	excluded   []string
	membership string
}

// member makes an active member (unless w.membership says otherwise) with
// those preferences.
func (f *fixture) member(t *testing.T, name string, w wants) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	u, err := f.d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: name})
	if err != nil {
		t.Fatal(err)
	}
	status := w.membership
	if status == "" {
		status = "active"
	}
	if _, err := f.admin.SaveMembership(ctx, f.owner, u, f.plan, status, 0); err != nil {
		t.Fatal(err)
	}
	var lo, hi *string
	if w.min != "" {
		lo, hi = &w.min, &w.max
	}
	nz := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	if _, err := f.d.Admin(t).Exec(ctx, `INSERT INTO job_preference(user_id,seniority_min,seniority_max,areas,countries,remote_ok,excluded_employment,stale)
 VALUES($1,$2,$3,$4,$5,$6,$7,false)`, u, lo, hi, nz(w.areas), nz(w.countries), w.remoteOK, nz(w.excluded)); err != nil {
		t.Fatal(err)
	}
	return u
}

// titles are the member's alerts, sorted.
func (f *fixture) titles(t *testing.T, u uuid.UUID) []string {
	t.Helper()
	rows, err := f.d.Admin(t).Query(context.Background(), `SELECT title FROM job_alert WHERE user_id=$1 ORDER BY title`, u)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.d.Admin(t).Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func sighting(title, country string, remote bool) catalog.Sighting {
	return catalog.Sighting{Source: catalog.SourceSearch, Company: "Acme", Title: title, Location: country, Country: country, Remote: remote,
		URL: "https://acme.example/jobs/" + uuid.NewString()}
}

func equal(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Without classifier models, postings route as soon as they are ingested, on
// what the source says: here, the country and remote flag.
func TestIngestRoutesByPreferences(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mx := f.member(t, "mx", wants{countries: []string{"MX"}, remoteOK: true})
	onsite := f.member(t, "onsite", wants{countries: []string{"MX"}})
	anyone := f.member(t, "anyone", wants{})
	suspended := f.member(t, "suspended", wants{membership: "suspended"})

	if err := f.alerts.Ingest(ctx, []catalog.Sighting{
		sighting("Monterrey role", "MX", false),
		sighting("Austin role", "US", false),
		sighting("Remote anywhere", "", true),
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.titles(t, mx); !equal(got, []string{"Monterrey role", "Remote anywhere"}) {
		t.Errorf("mx got %v", got)
	}
	if got := f.titles(t, onsite); !equal(got, []string{"Monterrey role"}) {
		t.Errorf("onsite got %v", got)
	}
	if got := f.titles(t, anyone); len(got) != 3 {
		t.Errorf("no preferences got %v, want everything", got)
	}
	if got := f.titles(t, suspended); len(got) != 0 {
		t.Errorf("suspended member got %v", got)
	}
	if got := f.titles(t, catalog.SystemMember); len(got) != 0 {
		t.Errorf("the catalog's member got %v", got)
	}
	// Each alert is a notification.
	page, err := notify.New(f.d.Store).List(ctx, mx, notify.Filter{Kind: "job_alert", Limit: 10})
	if err != nil || len(page) != 2 || page[0].JobAlert == nil || page[0].JobAlert.PostingID == uuid.Nil {
		t.Fatalf("notifications = %+v, %v", page, err)
	}

	// Seeing the same postings again, or routing again, files nothing.
	f.exec(t, `UPDATE platform.posting SET routed_at=NULL`)
	if n, err := f.alerts.Route(ctx); err != nil || n != 0 {
		t.Fatalf("second route filed %d, %v", n, err)
	}
	if got := f.titles(t, mx); len(got) != 2 {
		t.Errorf("after re-route mx has %v", got)
	}
}

// With models set, a posting waits for both stages, then routes on its
// labels; one that waits past the hold goes out on what is known.
func TestRouteWaitsForLabels(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec(t, `UPDATE platform.model_settings SET posting_decision_model='d',posting_extraction_model='e' WHERE id`)
	dev := f.member(t, "dev", wants{min: "entry", max: "mid", areas: []string{"engineering_software"}, excluded: []string{"internship"}})

	if err := f.alerts.Ingest(ctx, []catalog.Sighting{
		sighting("Software Engineer", "MX", false),
		sighting("Staff Engineer", "MX", false),
		sighting("Sales Lead", "MX", false),
		sighting("Engineering Intern", "MX", false),
		sighting("Unlabeled", "MX", false),
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.titles(t, dev); len(got) != 0 {
		t.Fatalf("delivered before labels: %v", got)
	}
	label := func(title, seniority, area, employment string, conf float64) {
		t.Helper()
		f.exec(t, `INSERT INTO platform.posting_label(posting_id,version,seniority,areas,employment_type,countries,confidence)
 SELECT id,1,$2,ARRAY[$3],$4,'{}',jsonb_build_object('seniority',$5::float,'areas',$5::float,'employment_type',$5::float) FROM platform.posting WHERE title=$1`,
			title, seniority, area, employment, conf)
		f.exec(t, `UPDATE platform.posting_stage SET done=true WHERE posting_id=(SELECT id FROM platform.posting WHERE title=$1)`, title)
	}
	label("Software Engineer", "mid", "engineering_software", "full_time", 0.9)
	label("Staff Engineer", "staff", "engineering_software", "full_time", 0.9)
	label("Sales Lead", "mid", "sales", "full_time", 0.9)
	// Unsure labels never hide a posting.
	label("Engineering Intern", "intern", "engineering_software", "internship", 0.3)
	if _, err := f.alerts.Route(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.titles(t, dev); !equal(got, []string{"Engineering Intern", "Software Engineer"}) {
		t.Errorf("labeled routing got %v", got)
	}
	// A failed stage counts as finished.
	f.exec(t, `UPDATE platform.posting_stage SET done=true,failed=true WHERE stage='decision' AND posting_id=(SELECT id FROM platform.posting WHERE title='Unlabeled')`)
	if _, err := f.alerts.Route(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.titles(t, dev); len(got) != 2 {
		t.Errorf("routed with a stage still running: %v", got)
	}
	f.exec(t, `UPDATE platform.posting SET first_seen_at=now()-interval '11 minutes' WHERE title='Unlabeled'`)
	if _, err := f.alerts.Route(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.titles(t, dev); !equal(got, []string{"Engineering Intern", "Software Engineer", "Unlabeled"}) {
		t.Errorf("after the hold got %v", got)
	}
}

// A posting the member already saved as a job is not alerted.
func TestSavedJobIsNotAlerted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.member(t, "saver", wants{})
	s := sighting("Saved role", "MX", false)
	norm, err := joburl.Normalize(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO job_status(id,user_id,name,normalized_name,stage,position) VALUES($1,$2,'Saved','saved','saved','a0') ON CONFLICT(user_id,normalized_name) DO NOTHING`, uuid.New(), u)
	f.exec(t, `INSERT INTO job(id,user_id,status_id,title,position,content_hash,url,normalized_url)
 SELECT $1,$2,id,'Saved role','a0','\x00'::bytea,$3,$4 FROM job_status WHERE user_id=$2 LIMIT 1`, uuid.New(), u, s.URL, norm)
	if err := f.alerts.Ingest(ctx, []catalog.Sighting{s}); err != nil {
		t.Fatal(err)
	}
	if got := f.titles(t, u); len(got) != 0 {
		t.Errorf("saved job alerted: %v", got)
	}
}

func ingestJobs(t *testing.T, f *fixture) [][]catalog.Sighting {
	t.Helper()
	rows, err := f.d.Admin(t).Query(context.Background(), `SELECT args FROM river.river_job WHERE kind='posting_ingest' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]catalog.Sighting
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var a alerts.IngestArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a.Sightings)
	}
	return out
}

// Suppliers ask for what members accept, queue what they find in the
// transaction that moves their cursor, and never buy a posting twice.
func TestSupplyQueuesPostings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO platform.supplier(name,cursor_at) VALUES($1,now()-interval '1 hour'),($2,now()-interval '1 hour')`,
		alerts.SupplierCountries, alerts.SupplierRemote)

	// Nobody wants anything yet: no search is paid for.
	if err := f.alerts.Supply(ctx, alerts.SupplierCountries); err != nil {
		t.Fatal(err)
	}
	if len(f.search.queries) != 0 {
		t.Fatalf("searched with no audience: %+v", f.search.queries)
	}
	f.member(t, "mx", wants{countries: []string{"MX", "CO"}})
	f.member(t, "us", wants{countries: []string{"US"}, remoteOK: true})
	f.exec(t, `UPDATE platform.supplier SET cursor_at=now()-interval '1 hour'`)

	now := time.Now().Truncate(time.Second)
	f.search.postings = []jobsearch.Posting{
		{ID: "1", Title: "Backend", Company: "Acme", URL: "https://acme.example/1", CountryCode: "MX", DiscoveredAt: now.Add(-30 * time.Minute)},
		{ID: "2", Title: "Frontend", Company: "Acme", URL: "https://acme.example/2", CountryCode: "US", DiscoveredAt: now.Add(-20 * time.Minute)},
		{ID: "3", Title: "Remote", Company: "Acme", URL: "https://acme.example/3", Remote: true, DiscoveredAt: now.Add(-10 * time.Minute)},
	}
	if err := f.alerts.Supply(ctx, alerts.SupplierCountries); err != nil {
		t.Fatal(err)
	}
	if err := f.alerts.Supply(ctx, alerts.SupplierRemote); err != nil {
		t.Fatal(err)
	}
	if len(f.search.queries) != 2 || !equal(f.search.queries[0].Countries, []string{"CO", "MX", "US"}) || !f.search.queries[1].RemoteOnly ||
		len(f.search.queries[1].Countries) != 0 || f.search.queries[0].Limit != 10 {
		t.Fatalf("queries = %+v", f.search.queries)
	}
	jobs := ingestJobs(t, f)
	if len(jobs) != 2 || len(jobs[0]) != 3 || len(jobs[1]) != 1 || jobs[0][0].Country != "MX" || jobs[0][0].Source != catalog.SourceSearch {
		t.Fatalf("ingest jobs = %+v", jobs)
	}
	var credits int
	if err := f.d.Admin(t).QueryRow(ctx, `SELECT credits_total FROM platform.supplier WHERE name=$1`, alerts.SupplierCountries).Scan(&credits); err != nil || credits != 3 {
		t.Fatalf("credits = %d, %v", credits, err)
	}
	// The next run starts past what it bought.
	if err := f.alerts.Supply(ctx, alerts.SupplierCountries); err != nil {
		t.Fatal(err)
	}
	if got := ingestJobs(t, f); len(got) != 2 {
		t.Fatalf("bought again: %d ingest jobs", len(got))
	}

	// A failing provider backs off and keeps the cursor.
	var before time.Time
	_ = f.d.Admin(t).QueryRow(ctx, `SELECT cursor_at FROM platform.supplier WHERE name=$1`, alerts.SupplierCountries).Scan(&before)
	f.search.err = &jobsearch.Error{Reason: "the job search provider failed (500)"}
	if err := f.alerts.Supply(ctx, alerts.SupplierCountries); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	var failures int
	var lastError string
	if err := f.d.Admin(t).QueryRow(ctx, `SELECT cursor_at,failures,last_error FROM platform.supplier WHERE name=$1`, alerts.SupplierCountries).
		Scan(&after, &failures, &lastError); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) || failures != 1 || lastError != "the job search provider failed (500)" {
		t.Errorf("after failure: cursor %v→%v failures=%d error=%q", before, after, failures, lastError)
	}
}
