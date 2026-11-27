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

// supplyLease bounds one supplier run. A run that dies holds its supplier
// until then; the next run reads the same page again.
const supplyLease = 10 * time.Minute

// recoverSuppliers dispatches every supplier that is due, moving its next run
// in the transaction that queues the job, so a crash neither skips a run nor
// queues one twice. A run is attempted once: a retry would pay again.
func (s *Service) recoverSuppliers(ctx context.Context, client *river.Client[pgx.Tx]) error {
	if s.search == nil {
		return nil
	}
	if _, err := s.store.Pool().Exec(ctx, `INSERT INTO platform.supplier(name) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, suppliers); err != nil {
		return err
	}
	tick := time.Now().Truncate(time.Minute).Format(time.RFC3339)
	return pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE platform.supplier SET next_run_at=now()+$2::interval
 WHERE name=ANY($1) AND next_run_at<=now() RETURNING name`, suppliers, SupplyInterval.String())
		if err != nil {
			return err
		}
		due, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, name := range due {
			if _, err := client.InsertTx(ctx, tx, SupplyArgs{Name: name, Tick: tick}, &river.InsertOpts{MaxAttempts: 1,
				UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
					rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Supply runs one supplier: it reads the next page of what TheirStack
// discovered in the supplier's window, within what members accept, and queues
// the postings for ingest in the same transaction that moves the supplier on.
//
// A window runs from the cursor to a second already past when the window
// opens. It stays fixed while pages are full, so a page boundary can't skip or
// repeat postings that share a discovery second; a partial page closes it and
// the next window starts one second after it. A supplier with a full page runs
// again on the next recovery tick instead of waiting an interval.
//
// One run per supplier at a time: a run holds a lease (supplyLease), so a
// late duplicate job finds it held and does nothing. The two suppliers don't
// overlap: while the remote one runs, the countries one asks only for on-site
// and hybrid postings. A supplier no member needs buys nothing and restarts
// from now.
func (s *Service) Supply(ctx context.Context, name string) error {
	var cursor time.Time
	var window *time.Time
	var page int
	err := s.store.Pool().QueryRow(ctx, `UPDATE platform.supplier SET running_until=now()+$2::interval
 WHERE name=$1 AND (running_until IS NULL OR running_until<now()) RETURNING cursor_at,window_end,page`, name, supplyLease.String()).Scan(&cursor, &window, &page)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // another run holds it, or the supplier is gone
	}
	if err != nil {
		return err
	}
	release := func() {
		_, _ = s.store.Pool().Exec(context.WithoutCancel(ctx), `UPDATE platform.supplier SET running_until=NULL WHERE name=$1`, name)
	}
	defer release()
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
	err = s.store.Pool().QueryRow(ctx, `SELECT key_id,ciphertext,encryption_version,max_results FROM platform.search_config()`).Scan(&keyID, &ciphertext, &version, &maxResults)
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
	if err := s.store.Pool().QueryRow(ctx, `SELECT countries,remote FROM platform.supply_scope()`).Scan(&countries, &remote); err != nil {
		return err
	}
	if (name == SupplierCountries && len(countries) == 0) || (name == SupplierRemote && !remote) {
		_, err := s.store.Pool().Exec(ctx, `UPDATE platform.supplier SET cursor_at=now(),window_end=NULL,page=0,last_ok_at=now(),failures=0,last_error=''
 WHERE name=$1`, name)
		return err
	}
	end := time.Now().Truncate(time.Second).Add(-time.Second)
	if window != nil {
		end = *window
	}
	if end.Before(cursor) {
		end = cursor
	}
	q := jobsearch.Query{Since: cursor, Until: end, Offset: page * maxResults, Limit: maxResults}
	switch name {
	case SupplierCountries:
		q.Countries, q.NotRemote = countries, remote
	case SupplierRemote:
		q.RemoteOnly = true
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
	full := res.Returned >= maxResults
	return pgx.BeginFunc(context.WithoutCancel(ctx), s.store.Pool(), func(tx pgx.Tx) error {
		var err error
		if full {
			// More in this window: keep it, take the next page soon.
			_, err = tx.Exec(ctx, `UPDATE platform.supplier SET window_end=$2,page=page+1,next_run_at=now(),last_ok_at=now(),failures=0,last_error='',
 credits_total=credits_total+$3 WHERE name=$1`, name, end, credits)
		} else {
			_, err = tx.Exec(ctx, `UPDATE platform.supplier SET cursor_at=$2,window_end=NULL,page=0,last_ok_at=now(),failures=0,last_error='',
 credits_total=credits_total+$3 WHERE name=$1`, name, end.Add(time.Second), credits)
		}
		if err != nil || len(ss) == 0 {
			return err
		}
		if s.queue == nil {
			return errors.New("ingest queue unavailable")
		}
		return s.queue.InsertTx(ctx, tx, IngestArgs{Sightings: ss})
	})
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
