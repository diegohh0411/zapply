package alerts_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/catalog"
)

// fakeBoards serves boards from memory; a missing board is ats.ErrNotFound.
type fakeBoards struct {
	mu     sync.Mutex
	boards map[string]ats.Board
}

func (f *fakeBoards) Fetch(_ context.Context, system, board string) (ats.Board, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.boards[system+":"+board]
	if !ok {
		return ats.Board{}, ats.ErrNotFound
	}
	return b, nil
}

func (f *fakeBoards) open(system, board string, keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.boards[system+":"+board]
	for _, k := range keys {
		b.Postings = append(b.Postings, ats.Posting{Key: k, Title: "Role " + k, Location: "Remote",
			URL: "https://jobs.lever.co/" + board + "/" + k, Remote: true, Description: "Do " + k})
	}
	f.boards[system+":"+board] = b
}

// asOwner runs a platform function as the owner.
func (f *fixture) asOwner(t *testing.T, q string, args ...any) (json.RawMessage, error) {
	t.Helper()
	var raw json.RawMessage
	err := f.d.Store.InTenant(context.Background(), f.owner, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), q, args...).Scan(&raw)
	})
	return raw, err
}

func newRiverClient(t *testing.T, f *fixture) *river.Client[pgx.Tx] {
	t.Helper()
	c, err := river.NewClient(riverpgxv5.New(f.d.Store.Pool()), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ingestQueued runs, and removes, every queued ingest job.
func ingestQueued(t *testing.T, f *fixture) {
	t.Helper()
	jobs := ingestJobs(t, f)
	f.exec(t, `DELETE FROM river.river_job WHERE kind='posting_ingest'`)
	for _, ss := range jobs {
		if err := f.alerts.Ingest(context.Background(), ss); err != nil {
			t.Fatal(err)
		}
	}
}

func boardFixture(t *testing.T) (*fixture, *fakeBoards) {
	t.Helper()
	f := newFixture(t)
	b := &fakeBoards{boards: map[string]ats.Board{}}
	f.alerts.WithBoards(b)
	return f, b
}

func (f *fixture) boardField(t *testing.T, board, col string) string {
	t.Helper()
	var v string
	if err := f.d.Admin(t).QueryRow(context.Background(), `SELECT COALESCE(`+col+`::text,'') FROM platform.ats_board WHERE board=$1`, board).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A board's first read catalogs what is already open without alerting; later
// reads alert on what opened since, and only once.
func TestBoardBaselineThenAlerts(t *testing.T) {
	f, boards := boardFixture(t)
	ctx := context.Background()
	anyone := f.member(t, "anyone", wants{})
	boards.open(ats.Lever, "acme", "1", "2")
	if _, err := f.asOwner(t, `SELECT platform.add_ats_boards($1)`, `[{"ats":"lever","board":"acme"}]`); err != nil {
		t.Fatal(err)
	}

	if err := f.alerts.Poll(ctx, ats.Lever, "acme"); err != nil {
		t.Fatal(err)
	}
	ingestQueued(t, f)
	var n int
	if err := f.d.Admin(t).QueryRow(ctx, `SELECT count(*) FROM platform.posting WHERE source='ats' AND description LIKE 'Do %'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("baseline cataloged %d postings, %v", n, err)
	}
	if got := f.titles(t, anyone); len(got) != 0 {
		t.Fatalf("baseline alerted %v", got)
	}
	if got := f.boardField(t, "acme", "company"); got != "Acme" {
		t.Errorf("company = %q, want the slug's name", got)
	}

	boards.open(ats.Lever, "acme", "3")
	if err := f.alerts.Poll(ctx, ats.Lever, "acme"); err != nil {
		t.Fatal(err)
	}
	if jobs := ingestJobs(t, f); len(jobs) != 1 || len(jobs[0]) != 1 || jobs[0][0].ATSKey != "lever:acme:3" {
		t.Fatalf("second read queued %+v, want only the new posting", jobs)
	}
	ingestQueued(t, f)
	if got := f.titles(t, anyone); !equal(got, []string{"Role 3"}) {
		t.Fatalf("after a new posting got %v", got)
	}
	if f.boardField(t, "acme", "last_new_at") == "" || f.boardField(t, "acme", "open_count") != "3" {
		t.Error("the read didn't record the new posting")
	}

	// Unread for longer than the gap: the next read is a baseline again.
	f.exec(t, `UPDATE platform.ats_board SET last_ok_at=now()-$1::interval`, (alerts.BaselineGap + time.Hour).String())
	boards.open(ats.Lever, "acme", "4")
	if err := f.alerts.Poll(ctx, ats.Lever, "acme"); err != nil {
		t.Fatal(err)
	}
	ingestQueued(t, f)
	if got := f.titles(t, anyone); !equal(got, []string{"Role 3"}) {
		t.Fatalf("a read after a gap alerted: %v", got)
	}
}

// Failures back off, and a board missing for a week is disabled. A disabled
// board isn't read.
func TestBoardFailures(t *testing.T) {
	f, _ := boardFixture(t)
	ctx := context.Background()
	if _, err := f.asOwner(t, `SELECT platform.add_ats_boards($1)`, `[{"ats":"ashby","board":"gone"}]`); err != nil {
		t.Fatal(err)
	}
	if err := f.alerts.Poll(ctx, ats.Ashby, "gone"); err != nil {
		t.Fatal(err)
	}
	var later bool
	if err := f.d.Admin(t).QueryRow(ctx, `SELECT next_poll_at>now()+interval '14 minutes' FROM platform.ats_board WHERE board='gone'`).Scan(&later); err != nil || !later {
		t.Fatalf("no backoff: %v %v", later, err)
	}
	if f.boardField(t, "gone", "failures") != "1" || f.boardField(t, "gone", "not_found_since") == "" || f.boardField(t, "gone", "status") != "active" {
		t.Fatal("a first miss should count and keep the board")
	}
	f.exec(t, `UPDATE platform.ats_board SET not_found_since=now()-interval '8 days'`)
	if err := f.alerts.Poll(ctx, ats.Ashby, "gone"); err != nil {
		t.Fatal(err)
	}
	if f.boardField(t, "gone", "status") != "disabled" || f.boardField(t, "gone", "last_error") != "not found for a week" {
		t.Fatalf("still %s: %s", f.boardField(t, "gone", "status"), f.boardField(t, "gone", "last_error"))
	}
	if err := f.alerts.Poll(ctx, ats.Ashby, "gone"); err != nil || f.boardField(t, "gone", "failures") != "2" {
		t.Fatalf("a disabled board was read: %v", err)
	}
}

// Search postings that link to a board offer it to the owner; a rejected
// board stays rejected.
func TestBoardDiscovery(t *testing.T) {
	f, _ := boardFixture(t)
	ctx := context.Background()
	s1, s2 := sighting("One", "MX", false), sighting("Two", "MX", false)
	s1.URL, s2.URL = "https://jobs.lever.co/kavak/1", "https://jobs.lever.co/Kavak/2"
	if err := f.alerts.Ingest(ctx, []catalog.Sighting{s1, s2, sighting("Elsewhere", "MX", false)}); err != nil {
		t.Fatal(err)
	}
	if f.boardField(t, "kavak", "status") != "pending" || f.boardField(t, "kavak", "sightings") != "2" || f.boardField(t, "kavak", "source") != "discovered" {
		t.Fatal("the board wasn't offered once with both sightings")
	}
	if _, err := f.asOwner(t, `SELECT platform.set_ats_board_status('lever','kavak','disabled')`); err != nil {
		t.Fatal(err)
	}
	s3 := sighting("Three", "MX", false)
	s3.URL = "https://jobs.lever.co/kavak/3"
	if err := f.alerts.Ingest(ctx, []catalog.Sighting{s3}); err != nil {
		t.Fatal(err)
	}
	if f.boardField(t, "kavak", "status") != "disabled" || f.boardField(t, "kavak", "last_error") != "rejected" {
		t.Fatal("discovery brought a rejected board back")
	}
}

// Due boards are queued once per tick and moved by the board interval, which
// the owner sets.
func TestBoardScheduleAndAdmin(t *testing.T) {
	f, _ := boardFixture(t)
	ctx := context.Background()
	raw, err := f.asOwner(t, `SELECT platform.add_ats_boards($1)`, `[{"ats":"greenhouse","board":"a"},{"ats":"greenhouse","board":"A"},{"ats":"greenhouse","board":"b"}]`)
	if err != nil || !strings.Contains(string(raw), `"added": 2`) {
		t.Fatalf("add = %s, %v", raw, err)
	}
	if _, err := f.asOwner(t, `SELECT platform.save_supplier_interval('ats:boards',60)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.asOwner(t, `SELECT platform.save_supplier_interval('ats:boards',7)`); err == nil {
		t.Fatal("an interval off the list was saved")
	}
	client := newRiverClient(t, f)
	if err := f.alerts.Recover(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := f.alerts.Recover(ctx, client); err != nil {
		t.Fatal(err)
	}
	var queued int
	var spaced bool
	if err := f.d.Admin(t).QueryRow(ctx, `SELECT count(*) FROM river.river_job WHERE kind='ats_board_poll'`).Scan(&queued); err != nil || queued != 2 {
		t.Fatalf("queued %d polls, %v", queued, err)
	}
	if err := f.d.Admin(t).QueryRow(ctx, `SELECT bool_and(next_poll_at>now()+interval '59 minutes') FROM platform.ats_board`).Scan(&spaced); err != nil || !spaced {
		t.Fatalf("next polls not an interval away: %v", err)
	}

	// The cap counts active boards.
	f.exec(t, `INSERT INTO platform.ats_board(ats,board,source,status) SELECT 'lever','x'||g,'seed','active' FROM generate_series(1,1998) g`)
	f.exec(t, `INSERT INTO platform.ats_board(ats,board,source,status) VALUES('lever','waiting','discovered','pending')`)
	if _, err := f.asOwner(t, `SELECT platform.set_ats_board_status('lever','waiting','active')`); err == nil {
		t.Fatal("approved past the cap")
	}
	if _, err := f.asOwner(t, `SELECT platform.job_sources()`); err != nil {
		t.Fatal(err)
	}
	// Members can't run it.
	member := f.member(t, "m", wants{})
	err = f.d.Store.InTenant(ctx, member, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.job_sources()`)
		return err
	})
	if err == nil {
		t.Fatal("a member read the job sources")
	}
}
