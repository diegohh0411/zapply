package alerts

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/jobsearch"
	"github.com/diegohh0411/zapply/internal/joburl"
	"github.com/diegohh0411/zapply/internal/logx"
)

// Searcher runs a search with the provider's API key; *jobsearch.TheirStack
// is the real one.
type Searcher interface {
	Search(ctx context.Context, key string, q jobsearch.Query) (jobsearch.Result, error)
}

// KeyOpener opens a system key sealed by the key ring; *secrets.Ring is one.
type KeyOpener interface {
	Decrypt(id, version string, ciphertext []byte) (string, error)
}

// WithSearch adds the TheirStack suppliers, opening the provider's key with keys.
func (s *Service) WithSearch(searcher Searcher, keys KeyOpener) *Service {
	s.search, s.keys = searcher, keys
	return s
}

// SupplyInterval is how often each supplier runs.
const SupplyInterval = 30 * time.Minute

// The TheirStack suppliers. Between them they cover what members accept:
// postings in their countries, and remote ones from anywhere.
const (
	SupplierCountries = "theirstack:countries"
	SupplierRemote    = "theirstack:remote"
)

var suppliers = []string{SupplierCountries, SupplierRemote}

// SupplyArgs runs one supplier. Tick makes each due slot its own unique job.
type SupplyArgs struct {
	Name string `json:"name" river:"unique"`
	Tick string `json:"tick" river:"unique"`
}

func (SupplyArgs) Kind() string { return "posting_supply" }

type supplyWorker struct {
	river.WorkerDefaults[SupplyArgs]
	s *Service
}

func (w *supplyWorker) Work(ctx context.Context, j *river.Job[SupplyArgs]) error {
	return w.s.Supply(ctx, j.Args.Name)
}

// IngestArgs is the queue between suppliers and the catalog: one supplier
// run's sightings.
type IngestArgs struct {
	Sightings []catalog.Sighting `json:"sightings"`
}

func (IngestArgs) Kind() string { return "posting_ingest" }

type ingestWorker struct {
	river.WorkerDefaults[IngestArgs]
	s *Service
}

func (w *ingestWorker) Work(ctx context.Context, j *river.Job[IngestArgs]) error {
	return w.s.Ingest(ctx, j.Args.Sightings)
}

// Ingest adds sightings to the catalog and routes whatever is ready (with no
// classifier models set, that is everything new).
func (s *Service) Ingest(ctx context.Context, ss []catalog.Sighting) error {
	if _, err := s.catalog.Add(ctx, ss); err != nil {
		return err
	}
	_, err := s.Route(ctx)
	return err
}

// recoverSuppliers dispatches every supplier that is due. A run is attempted
// once: a retry would pay again, and the next run resumes from the same cursor.
func (s *Service) recoverSuppliers(ctx context.Context, client *river.Client[pgx.Tx]) error {
	if s.search == nil {
		return nil
	}
	if _, err := s.store.Pool().Exec(ctx, `INSERT INTO platform.supplier(name) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, suppliers); err != nil {
		return err
	}
	rows, err := s.store.Pool().Query(ctx, `UPDATE platform.supplier SET next_run_at=now()+$2::interval
 WHERE name=ANY($1) AND next_run_at<=now() RETURNING name`, suppliers, SupplyInterval.String())
	if err != nil {
		return err
	}
	due, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	tick := time.Now().Truncate(time.Minute).Format(time.RFC3339)
	for _, name := range due {
		if _, err := client.Insert(ctx, SupplyArgs{Name: name, Tick: tick}, &river.InsertOpts{MaxAttempts: 1,
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
				rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}); err != nil {
			return err
		}
	}
	return nil
}

// Supply runs one supplier: it asks TheirStack for what it discovered since
// the supplier's cursor, within what members accept, and queues the postings
// for ingest in the same transaction that moves the cursor, so a run's
// postings are neither lost nor bought twice. A supplier with nothing to ask
// for (no member wants it) records a run and waits.
func (s *Service) Supply(ctx context.Context, name string) error {
	fail := func(msg string) error {
		_, err := s.store.Pool().Exec(ctx, `UPDATE platform.supplier SET failures=failures+1,last_error=left($2,500),
 next_run_at=now()+least($3::interval*power(2,least(failures,6)),interval '1 day') WHERE name=$1`, name, msg, SupplyInterval.String())
		return err
	}
	if s.search == nil || s.keys == nil {
		return fail("job search isn't set up")
	}
	var keyID uuid.UUID
	var ciphertext []byte
	var version string
	var maxResults int
	err := s.store.Pool().QueryRow(ctx, `SELECT key_id,ciphertext,encryption_version,max_results FROM platform.search_config()`).Scan(&keyID, &ciphertext, &version, &maxResults)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail("no TheirStack key is set")
	}
	if err != nil {
		return err
	}
	key, err := s.keys.Decrypt(keyID.String(), version, ciphertext)
	if err != nil {
		logx.From(ctx).ErrorContext(ctx, "search key not opened", "error", err)
		return fail("the TheirStack key can't be opened")
	}
	var countries []string
	var remote bool
	var cursor time.Time
	if err := s.store.Pool().QueryRow(ctx, `SELECT s.countries,s.remote,p.cursor_at FROM platform.supply_scope() s, platform.supplier p WHERE p.name=$1`, name).
		Scan(&countries, &remote, &cursor); err != nil {
		return err
	}
	q := jobsearch.Query{Since: cursor, Limit: maxResults}
	switch name {
	case SupplierCountries:
		q.Countries = countries
	case SupplierRemote:
		q.RemoteOnly = true
	}
	if (name == SupplierCountries && len(countries) == 0) || (name == SupplierRemote && !remote) {
		// Nobody wants these; start from now when someone does.
		_, err := s.store.Pool().Exec(ctx, `UPDATE platform.supplier SET cursor_at=now(),last_ok_at=now(),failures=0,last_error='' WHERE name=$1`, name)
		return err
	}
	res, searchErr := s.search.Search(ctx, key, q)
	credits := res.Credits
	var se *jobsearch.Error
	if errors.As(searchErr, &se) {
		credits = se.Credits
	}
	if searchErr != nil {
		msg := "couldn't run the search"
		if se != nil {
			msg = se.Reason
		}
		logx.From(ctx).WarnContext(ctx, "supplier failed", "supplier", name, "error", searchErr)
		_, _ = s.store.Pool().Exec(context.WithoutCancel(ctx), `UPDATE platform.supplier SET credits_total=credits_total+$2 WHERE name=$1`, name, credits)
		return fail(msg)
	}
	ss := make([]catalog.Sighting, 0, len(res.Postings))
	for _, p := range res.Postings {
		ss = append(ss, catalog.Sighting{Source: catalog.SourceSearch, Company: clip(p.Company, 200), Title: clip(p.Title, 500), Location: clip(p.Location, 500),
			URL: p.URL, Remote: p.Remote, Country: p.CountryCode, PublishedAt: p.PostedAt, Description: p.Description, SeniorityHint: p.Seniority})
	}
	next := nextCursor(cursor, res.Newest, res.Returned >= maxResults)
	return pgx.BeginFunc(context.WithoutCancel(ctx), s.store.Pool(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE platform.supplier SET cursor_at=greatest(cursor_at,$2),last_ok_at=now(),failures=0,last_error='',credits_total=credits_total+$3
 WHERE name=$1`, name, next, credits); err != nil {
			return err
		}
		if len(ss) == 0 {
			return nil
		}
		if s.queue == nil {
			return errors.New("ingest queue unavailable")
		}
		return s.queue.InsertTx(ctx, tx, IngestArgs{Sightings: ss})
	})
}

// nextCursor is where the next run starts. The provider filters discovery
// times by whole seconds, inclusively. After a partial page everything up to
// the newest second was returned, so the next run starts one second later
// and buys none of it again. After a full page more may share the newest
// second, so the next run starts at it, re-buying only that second, unless
// the page did not move the cursor at all: then it steps one second on rather
// than buy the same page forever.
func nextCursor(cursor, newest time.Time, full bool) time.Time {
	if newest.IsZero() {
		return cursor
	}
	newest = newest.Truncate(time.Second)
	switch {
	case !full:
		newest = newest.Add(time.Second)
	case !newest.After(cursor.Truncate(time.Second)):
		newest = cursor.Truncate(time.Second).Add(time.Second)
	}
	if newest.Before(cursor) {
		return cursor
	}
	return newest
}

// clip cuts s to at most n characters.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func normalized(u string) string {
	n, _ := joburl.Normalize(u)
	return n
}
