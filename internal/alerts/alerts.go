// Package alerts tells members about new openings that fit their job
// preferences (package preferences), soon after publication, so they can
// apply among the first.
//
// Members watch nothing. Suppliers (supply.go) find postings for the whole
// platform and queue them; ingest adds them to the posting catalog (package
// catalog), which labels them; routing (route.go) files an alert for every
// member each labeled posting fits (fit.go), once per member and posting.
package alerts

import (
	"context"
	"log/slog"
	"time"

	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

type Service struct {
	store   *store.Store
	queue   *worker.Queue
	catalog *catalog.Catalog
	search  Searcher
	keys    KeyOpener
	// boards reads ATS boards; boardSlots bound concurrent reads per ATS.
	boards     BoardFetcher
	boardSlots map[string]chan struct{}
	log        *slog.Logger
}

// New returns the alert pipeline. Without a queue nothing is queued: tests
// call Supply, Ingest and Route directly.
func New(st *store.Store, q *worker.Queue, c *catalog.Catalog, log *slog.Logger) *Service {
	return &Service{store: st, queue: q, catalog: c, log: log}
}

func (s *Service) AddWorkers(w *river.Workers) {
	river.AddWorker(w, &supplyWorker{s: s})
	river.AddWorker(w, &ingestWorker{s: s})
	river.AddWorker(w, &routeWorker{s: s})
	river.AddWorker(w, &pollWorker{s: s})
}

// Recover runs every minute: it starts every supplier and board that is due, and routes
// whatever a missed wake, or the end of a hold, left waiting.
func (s *Service) Recover(ctx context.Context, client *river.Client[pgx.Tx]) error {
	if err := s.recoverSuppliers(ctx, client); err != nil {
		return err
	}
	if err := s.recoverBoards(ctx, client); err != nil {
		return err
	}
	_, err := s.Route(ctx)
	return err
}

// Alert is one new posting that fits the member.
type Alert struct {
	ID          uuid.UUID  `json:"id"`
	PostingID   uuid.UUID  `json:"posting_id"`
	Company     string     `json:"company"`
	Title       string     `json:"title"`
	Location    string     `json:"location"`
	URL         string     `json:"url"`
	Remote      bool       `json:"remote"`
	PublishedAt *time.Time `json:"published_at"`
	FoundAt     time.Time  `json:"found_at"`
}

// AlertsByID loads alerts for notifications, inside the caller's tenant transaction.
func AlertsByID(ctx context.Context, tx pgx.Tx, u uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Alert, error) {
	out := map[uuid.UUID]Alert{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id,posting_id,company,title,location,url,remote,published_at,found_at FROM job_alert WHERE user_id=$1 AND id=ANY($2)`, u, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.PostingID, &a.Company, &a.Title, &a.Location, &a.URL, &a.Remote, &a.PublishedAt, &a.FoundAt); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}
