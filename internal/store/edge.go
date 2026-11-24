package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrEdgeUnauthorized is an edge worker token that matches no enabled worker.
var ErrEdgeUnauthorized = errors.New("invalid edge worker token")

// ErrScrapeJobGone is a job that does not exist, is not claimed by the calling
// worker, or ran out of time.
var ErrScrapeJobGone = errors.New("scrape job not found")

// EdgeJob is a scrape job handed to a worker.
type EdgeJob struct {
	ID  uuid.UUID `json:"id"`
	URL string    `json:"url"`
}

// EdgeResult is what a worker read.
type EdgeResult struct {
	Status     string // done or failed
	Markdown   string
	RawHTML    string
	Metadata   map[string]any
	StatusCode int
	Error      string
}

// ScrapeOutcome is the API's view of a job: Status is queued, claimed, done,
// failed or expired. A finished job's result is read once.
type ScrapeOutcome struct {
	Status     string         `json:"status"`
	StatusCode *int           `json:"status_code"`
	Error      *string        `json:"error"`
	Markdown   string         `json:"markdown"`
	RawHTML    string         `json:"raw_html"`
	Metadata   map[string]any `json:"metadata"`
}

func edgeErr(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.Code {
		case "ZA401":
			return ErrEdgeUnauthorized
		case "ZA404":
			return ErrScrapeJobGone
		}
	}
	return err
}

// EnqueueScrapeJob queues url for edge workers. ok is false, and nothing is
// queued, when no live worker may open host. ttlSeconds bounds how long the
// job can be claimed.
func (s *Store) EnqueueScrapeJob(ctx context.Context, url, host string, ttlSeconds int) (id uuid.UUID, ok bool, err error) {
	var got *uuid.UUID
	if err = s.pool.QueryRow(ctx, `SELECT platform.enqueue_scrape_job($1,$2,$3)`, url, host, ttlSeconds).Scan(&got); err != nil || got == nil {
		return uuid.Nil, false, err
	}
	return *got, true, nil
}

// ScrapeJobResult reads a job. With cancel, a job still pending is expired.
func (s *Store) ScrapeJobResult(ctx context.Context, id uuid.UUID, cancel bool) (ScrapeOutcome, error) {
	var raw json.RawMessage
	if err := s.pool.QueryRow(ctx, `SELECT platform.scrape_job_result($1,$2)`, id, cancel).Scan(&raw); err != nil {
		return ScrapeOutcome{}, edgeErr(err)
	}
	var o ScrapeOutcome
	return o, json.Unmarshal(raw, &o)
}

// ClaimScrapeJob returns the worker's next job, or nil when there is none it
// may take.
func (s *Store) ClaimScrapeJob(ctx context.Context, tokenHash []byte) (*EdgeJob, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT platform.claim_scrape_job($1)`, tokenHash).Scan(&raw); err != nil {
		return nil, edgeErr(err)
	}
	if raw == nil {
		return nil, nil
	}
	var j EdgeJob
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// FinishScrapeJob records a worker's result for a job it claimed.
func (s *Store) FinishScrapeJob(ctx context.Context, tokenHash []byte, id uuid.UUID, r EdgeResult) error {
	var meta *string
	if r.Metadata != nil {
		b, err := json.Marshal(r.Metadata)
		if err != nil {
			return err
		}
		m := string(b)
		meta = &m
	}
	var code *int
	if r.StatusCode > 0 {
		code = &r.StatusCode
	}
	var msg *string
	if r.Error != "" {
		msg = &r.Error
	}
	_, err := s.pool.Exec(ctx, `SELECT platform.finish_scrape_job($1,$2,$3,$4,$5,$6::jsonb,$7,$8)`,
		tokenHash, id, r.Status, r.Markdown, r.RawHTML, meta, code, msg)
	return edgeErr(err)
}
