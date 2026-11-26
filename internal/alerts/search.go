package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/diegohh0411/zapply/internal/jobsearch"
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

// WithSearch lets the service run search watchers through searcher, opening
// the provider's key with keys.
func (s *Service) WithSearch(searcher Searcher, keys KeyOpener) *Service {
	s.search, s.keys = searcher, keys
	return s
}

// SearchPollArgs runs one search. The watch's query rides along so the poll
// needs no tenant read; Tick makes each due slot its own unique job.
type SearchPollArgs struct {
	WatchID    uuid.UUID `json:"watch_id" river:"unique"`
	Tick       string    `json:"tick" river:"unique"`
	UserID     uuid.UUID `json:"user_id"`
	Keywords   []string  `json:"keywords"`
	Locations  []string  `json:"locations"`
	Country    *string   `json:"country"`
	RemoteOnly bool      `json:"remote_only"`
	CursorAt   time.Time `json:"cursor_at"`
}

func (SearchPollArgs) Kind() string { return "search_poll" }

type searchWorker struct {
	river.WorkerDefaults[SearchPollArgs]
	s *Service
}

func (w *searchWorker) Work(ctx context.Context, j *river.Job[SearchPollArgs]) error {
	return w.s.PollSearch(ctx, j.Args)
}

// recoverSearches dispatches every due search. A poll is attempted once: a
// retry would pay again, and the next poll resumes from the same cursor.
func (s *Service) recoverSearches(ctx context.Context, client *river.Client[pgx.Tx]) error {
	if s.search == nil {
		return nil
	}
	var raw []byte
	if err := s.store.Pool().QueryRow(ctx, `SELECT platform.search_watches_due(200)`).Scan(&raw); err != nil {
		return err
	}
	var due []SearchPollArgs
	if err := json.Unmarshal(raw, &due); err != nil {
		return err
	}
	tick := time.Now().Truncate(time.Minute).Format(time.RFC3339)
	for _, d := range due {
		d.Tick = tick
		if _, err := client.Insert(ctx, d, &river.InsertOpts{MaxAttempts: 1,
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
				rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}); err != nil {
			return err
		}
	}
	return nil
}

type searchConfig struct {
	keyID       uuid.UUID
	ciphertext  []byte
	version     string
	creditPrice int64
	maxResults  int
}

// searchChargeTimeout bounds a poll's charge: past it, an open charge settles
// at its reservation (platform.expire_charges).
const searchChargeTimeout = 10 * time.Minute

// PollSearch runs one search for its member: it reserves the most the call
// can cost on their ledger, asks the provider for what it discovered since
// the cursor, settles the charge at the credits charged, files the new
// postings and moves the cursor. A search the member's plan cannot pay for is
// marked and waits for its next interval.
func (s *Service) PollSearch(ctx context.Context, a SearchPollArgs) error {
	record := func(cursor *time.Time, errText, blocked *string) error {
		_, err := s.store.Pool().Exec(ctx, `SELECT platform.record_search_poll($1,$2,$3,$4)`, a.WatchID, cursor, errText, blocked)
		return err
	}
	blocked := func(reason string) error { return record(nil, nil, &reason) }
	var cfg searchConfig
	err := s.store.Pool().QueryRow(ctx, `SELECT key_id,ciphertext,encryption_version,credit_price,max_results FROM platform.search_config()`).
		Scan(&cfg.keyID, &cfg.ciphertext, &cfg.version, &cfg.creditPrice, &cfg.maxResults)
	if errors.Is(err, pgx.ErrNoRows) || s.search == nil || s.keys == nil {
		return blocked("provider_unavailable")
	}
	if err != nil {
		return err
	}
	key, err := s.keys.Decrypt(cfg.keyID.String(), cfg.version, cfg.ciphertext)
	if err != nil {
		logx.From(ctx).ErrorContext(ctx, "search key not opened", "error", err)
		return blocked("provider_unavailable")
	}
	charge := uuid.Must(uuid.NewV7())
	var reason string
	if err := s.store.InTenant(ctx, a.UserID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.open_search_charge($1,$2,$3,$4,$5)`, charge, a.WatchID, cfg.creditPrice,
			cfg.creditPrice*int64(cfg.maxResults), time.Now().Add(searchChargeTimeout)).Scan(&reason)
	}); err != nil {
		return err
	}
	switch reason {
	case "":
	case "watch_not_found":
		return nil
	default:
		return blocked(reason)
	}
	q := jobsearch.Query{Titles: a.Keywords, Locations: a.Locations, RemoteOnly: a.RemoteOnly, Since: a.CursorAt, Limit: cfg.maxResults}
	if a.Country != nil {
		q.Country = *a.Country
	}
	res, searchErr := s.search.Search(ctx, key, q)
	credits := res.Credits
	var se *jobsearch.Error
	if searchErr != nil {
		// A failed call costs what the provider said it charged; one that got
		// no answer may have run, and costs its reservation.
		credits = cfg.maxResults
		if errors.As(searchErr, &se) && !se.Unknown {
			credits = se.Credits
		}
	}
	// Settle even when the job is being stopped: the provider may have charged.
	if err := s.store.InTenant(context.WithoutCancel(ctx), a.UserID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT platform.close_search_charge($1,$2,$3)`, charge, credits, len(res.Postings))
		return err
	}); err != nil {
		return err
	}
	if searchErr != nil {
		msg := "couldn't run the search"
		if se != nil {
			msg = se.Reason
		}
		logx.From(ctx).WarnContext(ctx, "search failed", "watch", a.WatchID, "error", searchErr)
		return record(nil, &msg, nil)
	}
	cs := make([]candidate, 0, len(res.Postings))
	for _, p := range res.Postings {
		c := candidate{WatchID: a.WatchID, PostingKey: clip("theirstack:"+p.ID, 300), Company: clip(p.Company, 200), Title: clip(p.Title, 500),
			Location: clip(p.Location, 500), URL: p.URL, Remote: p.Remote, PublishedAt: p.PostedAt}
		if c.fileable() {
			cs = append(cs, c)
		}
	}
	// The cursor moves before alerts are filed: a poll that cannot file must
	// not buy the same postings again on the next one.
	cursor := nextCursor(a.CursorAt, res.Newest, res.Returned >= cfg.maxResults)
	if err := record(&cursor, nil, nil); err != nil {
		return err
	}
	if len(cs) > 0 {
		if _, err := s.file(ctx, a.UserID, cs); err != nil {
			msg := "found new postings but couldn't file them"
			_ = record(nil, &msg, nil)
			return err
		}
	}
	// After filing, and whatever matched: the catalog keeps every posting a
	// search paid for.
	s.catalogSearch(ctx, res.Postings)
	return nil
}

// nextCursor is where the next poll starts. The provider filters discovery
// times by whole seconds, inclusively. After a partial page everything up to
// the newest second was returned, so the next poll starts one second later
// and buys none of it again. After a full page more may share the newest
// second, so the next poll starts at it, re-buying only that second, unless
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
