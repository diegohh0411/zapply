package alerts_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/jobsearch"
	"github.com/diegohh0411/zapply/internal/membership"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/notify"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

// fakeSearch behaves like TheirStack: it returns, oldest first, up to the
// limit of its postings discovered at or after the query's second, charging a
// credit for each, usable or not. It records the queries.
type fakeSearch struct {
	mu       sync.Mutex
	postings []jobsearch.Posting
	err      error
	queries  []jobsearch.Query
	keys     []string
}

func (f *fakeSearch) Search(_ context.Context, key string, q jobsearch.Query) (jobsearch.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries, f.keys = append(f.queries, q), append(f.keys, key)
	if f.err != nil {
		return jobsearch.Result{}, f.err
	}
	since := q.Since.Truncate(time.Second)
	var res jobsearch.Result
	for _, p := range f.postings {
		if p.DiscoveredAt.Before(since) || res.Returned == q.Limit {
			continue
		}
		res.Returned++
		if p.DiscoveredAt.After(res.Newest) {
			res.Newest = p.DiscoveredAt
		}
		if p.URL != "" {
			res.Postings = append(res.Postings, p)
		}
	}
	res.Credits = res.Returned
	return res, nil
}

type searchFixture struct {
	d       *testdb.DB
	owner   uuid.UUID
	member  uuid.UUID
	admin   *membership.Service
	plan    uuid.UUID
	api     *alerts.Service
	worker  *alerts.Service
	search  *fakeSearch
	boards  *fakeBoards
	notes   *notify.Service
	workers *store.Store
}

func newSearchFixture(t *testing.T) *searchFixture {
	t.Helper()
	ctx := context.Background()
	d := testdb.New(t)
	f := &searchFixture{d: d, search: &fakeSearch{}, boards: &fakeBoards{boards: map[string]ats.Board{}}, notes: notify.New(d.Store)}
	var err error
	if f.owner, err = d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "owner"}); err != nil {
		t.Fatal(err)
	}
	if f.member, err = d.Store.ResolveUser(ctx, auth.Principal{Issuer: "test", Subject: "member"}); err != nil {
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
	one := 1
	if _, err = f.admin.SavePlan(ctx, f.owner, f.plan, "Search", "1", "2", &one, 0); err != nil {
		t.Fatal(err)
	}
	key, err := f.admin.AddKeyOfKind(ctx, f.owner, "TheirStack", "theirstack-secret", "theirstack")
	if err != nil {
		t.Fatal(err)
	}
	// A credit costs a cent; a poll asks for at most 10 postings.
	if _, err = f.admin.SaveSearchSettings(ctx, f.owner, &key, "0.01", 10, 1); err != nil {
		t.Fatal(err)
	}
	f.api = alerts.New(d.Store, f.boards)
	if f.workers, err = store.Open(ctx, d.WorkerURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.workers.Close)
	f.worker = alerts.New(f.workers, f.boards).WithSearch(f.search, ring)
	return f
}

func (f *searchFixture) join(t *testing.T, status string) {
	t.Helper()
	version := 0
	var current *int
	_ = f.d.Admin(t).QueryRow(context.Background(), `SELECT version FROM platform.membership WHERE user_id=$1`, f.member).Scan(&current)
	if current != nil {
		version = *current
	}
	if _, err := f.admin.SaveMembership(context.Background(), f.owner, f.member, f.plan, status, version); err != nil {
		t.Fatal(err)
	}
}

// due makes the member's searches due and returns what the worker would run.
func (f *searchFixture) due(t *testing.T) []alerts.SearchPollArgs {
	t.Helper()
	ctx := context.Background()
	if _, err := f.d.Admin(t).Exec(ctx, `UPDATE platform.search_watch SET next_poll_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.workers.Pool().QueryRow(ctx, `SELECT platform.search_watches_due(100)`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var due []alerts.SearchPollArgs
	if err := jsonUnmarshal(raw, &due); err != nil {
		t.Fatal(err)
	}
	return due
}

func (f *searchFixture) spent(t *testing.T) (amount int64, charges int) {
	t.Helper()
	if err := f.d.Admin(t).QueryRow(context.Background(), `SELECT COALESCE(sum(e.amount),0)::bigint,count(DISTINCT c.id)::int FROM platform.charge c
 LEFT JOIN platform.ledger_entry e ON e.charge_id=c.id WHERE c.user_id=$1 AND c.kind='search'`, f.member).Scan(&amount, &charges); err != nil {
		t.Fatal(err)
	}
	return amount, charges
}

func search(keywords ...string) alerts.Input {
	return alerts.Input{Kind: alerts.KindSearch, Keywords: &keywords}
}

func TestSearchWatchNeedsAPlanWithRoom(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	if _, err := f.api.Create(ctx, f.member, search("software engineer")); !isValidation(err) {
		t.Fatalf("without a plan: %v", err)
	}
	f.join(t, "active")
	if _, err := f.api.Create(ctx, f.member, search()); !isValidation(err) {
		t.Fatalf("without keywords: %v", err)
	}
	locs := []string{"Monterrey"}
	in := search("software engineer")
	in.Locations, in.Country = &locs, ptr("mx")
	w, err := f.api.Create(ctx, f.member, in)
	if err != nil || w.Kind != alerts.KindSearch || w.ATS != nil || *w.Country != "MX" || *w.EveryHours != alerts.DefaultEveryHours ||
		w.Company != "software engineer · Monterrey · MX" || w.NextCheckAt == nil {
		t.Fatalf("create = %+v, %v", w, err)
	}
	if _, err := f.api.Create(ctx, f.member, search("designer")); !isValidation(err) {
		t.Fatalf("beyond the plan: %v", err)
	}
	// The database refuses it too, whatever the service checks.
	err = f.d.Store.InTenant(ctx, f.member, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO job_watch(id,user_id,kind,company,keywords,every_hours) VALUES($1,$2,'search','x','{x}',2)`, uuid.New(), f.member)
		return err
	})
	if err == nil {
		t.Fatal("the trigger allowed a second search watcher")
	}
	_, allowance, err := f.api.Watches(ctx, f.member)
	if err != nil || allowance.Allowed != 1 || allowance.Used != 1 || !allowance.Available || allowance.MaxResults != 10 {
		t.Fatalf("allowance = %+v, %v", allowance, err)
	}
	if _, err := f.api.Update(ctx, f.member, w.ID, alerts.Input{EveryHours: ptrInt(5)}); !isValidation(err) {
		t.Fatalf("bad interval: %v", err)
	}
	if w, err = f.api.Update(ctx, f.member, w.ID, alerts.Input{EveryHours: ptrInt(6), Country: ptr("")}); err != nil || *w.EveryHours != 6 || w.Country != nil {
		t.Fatalf("update = %+v, %v", w, err)
	}
	empty := []string{}
	if _, err := f.api.Update(ctx, f.member, w.ID, alerts.Input{Keywords: &empty}); !isValidation(err) {
		t.Fatalf("emptied keywords: %v", err)
	}
}

func TestSearchPollBillsAndAlertsOnce(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	f.join(t, "active")
	w, err := f.api.Create(ctx, f.member, search("engineer"))
	if err != nil {
		t.Fatal(err)
	}
	discovered := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	f.search.postings = []jobsearch.Posting{
		{ID: "1", Title: "Backend Engineer", Company: "Acme, Inc.", Location: "Monterrey, NL, MX", URL: "https://jobs.lever.co/acme/1?utm_source=theirstack", DiscoveredAt: discovered},
		// The same opening found again on LinkedIn.
		{ID: "2", Title: "Backend engineer", Company: "ACME", Location: "Monterrey", URL: "https://www.linkedin.com/jobs/view/2", DiscoveredAt: discovered},
		{ID: "3", Title: "Frontend Engineer", Company: "Beta", Location: "Remote", URL: "https://beta.example/jobs/3", DiscoveredAt: discovered.Add(time.Second)},
	}
	due := f.due(t)
	if len(due) != 1 || due[0].WatchID != w.ID {
		t.Fatalf("due = %+v", due)
	}
	if err := f.worker.PollSearch(ctx, due[0]); err != nil {
		t.Fatal(err)
	}
	if q := f.search.queries[0]; f.search.keys[0] != "theirstack-secret" || q.Limit != 10 || q.Titles[0] != "engineer" || q.Since.After(time.Now()) {
		t.Fatalf("query = %+v key %q", q, f.search.keys[0])
	}
	cent, _ := modelapi.ParseUSD("0.01")
	if amount, n := f.spent(t); n != 1 || amount != 3*cent {
		t.Fatalf("spent %d in %d charges, want 3 credits in 1", amount, n)
	}
	if n := alertCount(t, f.notes, f.member); n != 2 {
		t.Fatalf("alerts = %d, want 2 (the LinkedIn copy is a duplicate)", n)
	}
	// The cursor moved past everything returned: the next poll buys nothing again.
	due = f.due(t)
	if !due[0].CursorAt.Equal(discovered.Add(2 * time.Second)) {
		t.Fatalf("cursor = %v, want %v", due[0].CursorAt, discovered.Add(2*time.Second))
	}
	if err := f.worker.PollSearch(ctx, due[0]); err != nil {
		t.Fatal(err)
	}
	if amount, n := f.spent(t); n != 2 || amount != 3*cent {
		t.Fatalf("repeat poll spent %d in %d charges, want nothing more", amount, n)
	}
	if n := alertCount(t, f.notes, f.member); n != 2 {
		t.Fatalf("repeat alerted: %d", n)
	}
	list, _, err := f.api.Watches(ctx, f.member)
	if err != nil || list[0].Spent30dUSD != "0.030000000000" || list[0].LastCheckedAt == nil {
		t.Fatalf("watch = %+v, %v", list, err)
	}
	// The activity list shows the searches as one alerts line.
	var entries []byte
	if err := f.d.Store.InTenant(ctx, f.member, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.usage_entries(NULL,10)`).Scan(&entries)
	}); err != nil || !bytes.Contains(entries, []byte(`"app": "alerts"`)) || !bytes.Contains(entries, []byte(`"search"`)) {
		t.Fatalf("entries = %s, %v", entries, err)
	}
}

func TestSearchPollWaitsForThePlan(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	f.join(t, "active")
	w, err := f.api.Create(ctx, f.member, search("engineer"))
	if err != nil {
		t.Fatal(err)
	}
	// A suspended member's search is marked, not run.
	f.join(t, "suspended")
	if due := f.due(t); len(due) != 0 {
		t.Fatalf("suspended member's search ran: %+v", due)
	}
	if list, _, _ := f.api.Watches(ctx, f.member); list[0].BlockedReason != "plan" {
		t.Fatalf("blocked = %q", list[0].BlockedReason)
	}
	// Back on the plan but out of budget: the poll is refused by the ledger.
	f.join(t, "active")
	if _, err := f.admin.SavePlan(ctx, f.owner, f.plan, "Search", "0.05", "2", nil, 1); err != nil {
		t.Fatal(err)
	}
	due := f.due(t)
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	if err := f.worker.PollSearch(ctx, due[0]); err != nil {
		t.Fatal(err)
	}
	if list, _, _ := f.api.Watches(ctx, f.member); list[0].ID != w.ID || list[0].BlockedReason != "request_exceeds_plan" || len(f.search.queries) != 0 {
		t.Fatalf("watch = %+v, searches %d", list[0], len(f.search.queries))
	}
	// A provider that refuses the search costs nothing and is reported.
	if _, err := f.admin.SavePlan(ctx, f.owner, f.plan, "Search", "1", "2", nil, 2); err != nil {
		t.Fatal(err)
	}
	f.search.err = &jobsearch.Error{Reason: "the job search provider is out of credits"}
	if err := f.worker.PollSearch(ctx, f.due(t)[0]); err != nil {
		t.Fatal(err)
	}
	if amount, n := f.spent(t); amount != 0 || n != 1 {
		t.Fatalf("refused search charged %d (%d charges)", amount, n)
	}
	if list, _, _ := f.api.Watches(ctx, f.member); list[0].LastError != "the job search provider is out of credits" {
		t.Fatalf("error = %q", list[0].LastError)
	}
	// One that did not answer is charged its reservation.
	f.search.err = &jobsearch.Error{Reason: "the job search provider did not answer", Unknown: true}
	if err := f.worker.PollSearch(ctx, f.due(t)[0]); err != nil {
		t.Fatal(err)
	}
	cent, _ := modelapi.ParseUSD("0.01")
	if amount, _ := f.spent(t); amount != 10*cent {
		t.Fatalf("unanswered search charged %d, want its 10-credit reservation", amount)
	}
}

func TestBoardAndSearchShareAlerts(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	f.join(t, "active")
	f.boards.set("lever/acme")
	if _, err := f.api.Create(ctx, f.member, alerts.Input{URL: "https://jobs.lever.co/acme"}); err != nil {
		t.Fatal(err)
	}
	if err := f.worker.Poll(ctx, "lever", "acme"); err != nil { // baseline
		t.Fatal(err)
	}
	if _, err := f.api.Create(ctx, f.member, search("engineer")); err != nil {
		t.Fatal(err)
	}
	f.boards.set("lever/acme", posting("p1", "Data Engineer", "Monterrey"))
	if err := f.worker.Poll(ctx, "lever", "acme"); err != nil {
		t.Fatal(err)
	}
	f.search.postings = []jobsearch.Posting{{ID: "9", Title: "Data Engineer", Company: "Acme", Location: "Monterrey", URL: "https://jobs.lever.co/acme/p1", DiscoveredAt: time.Now()}}
	if err := f.worker.PollSearch(ctx, f.due(t)[0]); err != nil {
		t.Fatal(err)
	}
	if n := alertCount(t, f.notes, f.member); n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
}

// A full page the client cannot use still moves the cursor, and a posting
// job_alert would refuse is skipped rather than failing the poll.
func TestSearchCursorPastUnusablePostings(t *testing.T) {
	f := newSearchFixture(t)
	ctx := context.Background()
	f.join(t, "active")
	if _, err := f.api.Create(ctx, f.member, search("engineer")); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	long := "https://example.com/" + strings.Repeat("a", 2100)
	for i := range 10 { // a full page (max_results is 10) without a usable link
		f.search.postings = append(f.search.postings, jobsearch.Posting{ID: fmt.Sprint(i), Title: "Engineer", DiscoveredAt: at})
	}
	f.search.postings = append(f.search.postings,
		jobsearch.Posting{ID: "long", Title: "Engineer", Company: "Acme", URL: long, DiscoveredAt: at.Add(time.Second)},
		jobsearch.Posting{ID: "ok", Title: "Engineer", Company: "Beta", URL: "https://beta.example/1", DiscoveredAt: at.Add(2 * time.Second)})
	for range 4 {
		if err := f.worker.PollSearch(ctx, f.due(t)[0]); err != nil {
			t.Fatal(err)
		}
	}
	// The full page is bought twice (more might share its second), then the
	// cursor steps past it; then the long and the good posting; then nothing.
	cent, _ := modelapi.ParseUSD("0.01")
	if amount, n := f.spent(t); amount != 22*cent || n != 4 {
		t.Fatalf("spent %d in %d polls, want 22 credits in 4", amount, n)
	}
	if n := alertCount(t, f.notes, f.member); n != 1 {
		t.Fatalf("alerts = %d, want the usable posting only", n)
	}
	if list, _, _ := f.api.Watches(ctx, f.member); list[0].LastError != "" {
		t.Fatalf("last error = %q", list[0].LastError)
	}
}

func TestNextCursor(t *testing.T) {
	c := time.Date(2026, 1, 1, 10, 0, 0, 500, time.UTC)
	newest := c.Add(5 * time.Second)
	for _, tc := range []struct {
		name   string
		newest time.Time
		full   bool
		want   time.Time
	}{
		{"nothing", time.Time{}, false, c},
		{"partial page", newest, false, newest.Truncate(time.Second).Add(time.Second)},
		{"full page", newest, true, newest.Truncate(time.Second)},
		{"full page stuck on one second", c, true, c.Truncate(time.Second).Add(time.Second)},
	} {
		if got := alerts.NextCursor(c, tc.newest, tc.full); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func ptr(s string) *string { return &s }
func ptrInt(n int) *int    { return &n }

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
