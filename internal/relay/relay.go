// Package relay moves committed outbox rows to NATS. Design:
// docs/parts/03-events.md.
//
// Each batch is one transaction: take the relay's advisory lock, claim the
// oldest unpublished rows, stamp them with seq values, publish and flush, then
// commit. The lock serializes batches across replicas, so seq follows publish
// order. Publishing before commit makes delivery at-least-once: a crash in
// between leaves the rows unpublished and they are sent again.
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/diegohh0411/zapply/internal/events"
	"github.com/diegohh0411/zapply/internal/store/db"
)

// lockKey identifies the relay's transaction-scoped advisory lock. A
// transaction lock, unlike a session lock, works behind transaction-mode
// poolers.
const lockKey int64 = 0x7a61_7070_6c79_0003

// Publisher is the part of *nats.Conn the relay uses.
type Publisher interface {
	Publish(subject string, data []byte) error
	FlushTimeout(timeout time.Duration) error
}

type Config struct {
	Interval  time.Duration // poll interval when idle
	Batch     int           // rows per transaction
	Retention time.Duration // published rows older than this are pruned
}

type Relay struct {
	pool *pgxpool.Pool
	pub  Publisher
	cfg  Config
	log  *slog.Logger

	// beforeCommit runs after a batch is flushed to NATS. Tests use it to
	// simulate a crash between publish and commit.
	beforeCommit func() error
}

func New(pool *pgxpool.Pool, pub Publisher, cfg Config, log *slog.Logger) *Relay {
	if cfg.Interval <= 0 {
		cfg.Interval = 200 * time.Millisecond
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 500
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 24 * time.Hour
	}
	return &Relay{pool: pool, pub: pub, cfg: cfg, log: log}
}

const (
	flushTimeout = 5 * time.Second
	stepTimeout  = 30 * time.Second
	pruneEvery   = time.Minute
	pruneBatch   = 5000
	maxBackoff   = 5 * time.Second
)

// Run relays until ctx is cancelled. A full batch is followed by another one
// immediately; otherwise it waits Interval. Errors back off up to 5 s.
func (r *Relay) Run(ctx context.Context) error {
	var lastPrune time.Time
	backoff := r.cfg.Interval
	for {
		n, err := r.Step(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			r.log.Error("relay batch failed", "err", err, "retry_in", backoff)
		default:
			backoff = r.cfg.Interval
			if n > 0 {
				r.log.Debug("relayed", "events", n)
			}
		}

		if time.Since(lastPrune) >= pruneEvery {
			if _, err := r.Prune(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("prune failed", "err", err)
			}
			lastPrune = time.Now()
		}

		wait := r.cfg.Interval
		if err != nil {
			wait = backoff
			backoff = min(backoff*2, maxBackoff)
		} else if n == r.cfg.Batch {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Step relays one batch and reports how many events it published. It returns
// 0 without error when another replica holds the lock.
func (r *Relay) Step(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	var n int
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return nil
		}
		q := db.New(tx)
		rows, err := q.ClaimOutbox(ctx, int32(r.cfg.Batch))
		if err != nil || len(rows) == 0 {
			return err
		}
		seqs, err := q.NextOutboxSeqs(ctx, int32(len(rows)))
		if err != nil {
			return err
		}
		if len(seqs) != len(rows) {
			return fmt.Errorf("got %d seqs for %d rows", len(seqs), len(rows))
		}
		// Values come from one statement under the lock, so they are all
		// above every earlier batch; sort them to follow claim order.
		slices.Sort(seqs)

		ids := make([]uuid.UUID, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
			msg, err := json.Marshal(events.Event{Seq: seqs[i], Topic: row.Topic, Data: row.Payload})
			if err != nil {
				return err
			}
			if err := r.pub.Publish(events.Subject(row.UserID, row.Topic), msg); err != nil {
				return fmt.Errorf("publish: %w", err)
			}
		}
		if err := r.pub.FlushTimeout(flushTimeout); err != nil {
			return fmt.Errorf("flush: %w", err)
		}
		if err := q.PublishOutbox(ctx, db.PublishOutboxParams{Ids: ids, Seqs: seqs}); err != nil {
			return err
		}
		if r.beforeCommit != nil {
			if err := r.beforeCommit(); err != nil {
				return err
			}
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Prune deletes published rows past the retention window, in bounded batches.
// It needs no lock: it never touches unpublished rows.
func (r *Relay) Prune(ctx context.Context) (int64, error) {
	before := time.Now().Add(-r.cfg.Retention)
	var total int64
	for {
		n, err := db.New(r.pool).PruneOutbox(ctx, db.PruneOutboxParams{Before: before, N: pruneBatch})
		total += n
		if err != nil || n < pruneBatch {
			return total, err
		}
	}
}
