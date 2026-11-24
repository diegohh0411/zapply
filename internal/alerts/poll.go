package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// PollArgs reads one board. Tick makes each due slot its own unique job.
type PollArgs struct {
	ATS   string `json:"ats" river:"unique"`
	Board string `json:"board" river:"unique"`
	Tick  string `json:"tick" river:"unique"`
}

func (PollArgs) Kind() string { return "ats_poll" }

type pollWorker struct {
	river.WorkerDefaults[PollArgs]
	s *Service
}

func (w *pollWorker) Work(ctx context.Context, j *river.Job[PollArgs]) error {
	return w.s.Poll(ctx, j.Args.ATS, j.Args.Board)
}

func (s *Service) AddWorkers(w *river.Workers) {
	river.AddWorker(w, &pollWorker{s: s})
}

// Recover runs every minute: it dispatches a poll for each board that is due.
// Picking a board moves its next poll forward, so scans never double up.
func (s *Service) Recover(ctx context.Context, client *river.Client[pgx.Tx]) error {
	var raw []byte
	if err := s.store.Pool().QueryRow(ctx, `SELECT platform.ats_boards_due(500,$1::interval)`, PollInterval.String()).Scan(&raw); err != nil {
		return err
	}
	var due []struct {
		ATS   string `json:"ats"`
		Board string `json:"board"`
	}
	if err := json.Unmarshal(raw, &due); err != nil {
		return err
	}
	tick := time.Now().Truncate(time.Minute).Format(time.RFC3339)
	for _, d := range due {
		if _, err := client.Insert(ctx, PollArgs{ATS: d.ATS, Board: d.Board, Tick: tick}, &river.InsertOpts{MaxAttempts: 3,
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
				rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}); err != nil {
			return err
		}
	}
	return nil
}

type match struct {
	UserID      uuid.UUID  `json:"user_id"`
	WatchID     uuid.UUID  `json:"watch_id"`
	Company     string     `json:"company"`
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Location    string     `json:"location"`
	URL         string     `json:"url"`
	Remote      bool       `json:"remote"`
	PublishedAt *time.Time `json:"published_at"`
}

// maxPostings bounds what one board may store.
const maxPostings = 5000

// Poll reads one board, records it, and files an alert for every new posting
// in each member's tenant whose watch it matches. A failure to file leaves the
// postings undelivered, so the retry (or next poll) files them.
func (s *Service) Poll(ctx context.Context, system, board string) error {
	read, fetchErr := s.ats.Fetch(ctx, system, board)
	var postings []byte
	var errText *string
	if fetchErr != nil {
		msg := "couldn't read the board"
		if errors.Is(fetchErr, ats.ErrNotFound) {
			msg = "the board no longer exists"
		}
		errText = &msg
		postings = []byte("[]")
	} else {
		if len(read.Postings) > maxPostings {
			read.Postings = read.Postings[:maxPostings]
		}
		var err error
		if postings, err = json.Marshal(read.Postings); err != nil {
			return err
		}
	}
	var raw []byte
	if err := s.store.Pool().QueryRow(ctx, `SELECT platform.record_ats_poll($1,$2,$3,$4,$5::interval)`,
		system, board, json.RawMessage(postings), errText, PollInterval.String()).Scan(&raw); err != nil {
		return err
	}
	if fetchErr != nil {
		// Recorded and backed off; retrying now would only hammer the board.
		return nil
	}
	var result struct {
		Matches []match  `json:"matches"`
		Pending []string `json:"pending"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	byUser := map[uuid.UUID][]match{}
	for _, m := range result.Matches {
		byUser[m.UserID] = append(byUser[m.UserID], m)
	}
	for u, ms := range byUser {
		if err := s.file(ctx, u, ms); err != nil {
			return err
		}
	}
	if len(result.Pending) == 0 {
		return nil
	}
	_, err := s.store.Pool().Exec(ctx, `SELECT platform.ats_postings_delivered($1,$2,$3)`, system, board, result.Pending)
	return err
}

// file inserts one member's alerts (the trigger files the notifications) and
// tells their open pages.
func (s *Service) file(ctx context.Context, u uuid.UUID, ms []match) error {
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		added := int64(0)
		for _, m := range ms {
			tag, err := tx.Exec(ctx, `INSERT INTO job_alert(id,user_id,watch_id,posting_key,company,title,location,url,remote,published_at)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10 WHERE EXISTS(SELECT FROM job_watch WHERE user_id=$2 AND id=$3)
 ON CONFLICT(watch_id,posting_key) DO NOTHING`,
				uuid.Must(uuid.NewV7()), u, m.WatchID, m.Key, m.Company, m.Title, m.Location, m.URL, m.Remote, m.PublishedAt)
			if err != nil {
				return err
			}
			added += tag.RowsAffected()
		}
		if added == 0 {
			return nil
		}
		payload, _ := json.Marshal(map[string]any{"id": u})
		_, err := tx.Exec(ctx, "INSERT INTO outbox(id,user_id,topic,payload) VALUES($1,$2,$3,$4)", uuid.Must(uuid.NewV7()), u, "notification.updated", json.RawMessage(payload))
		return err
	})
}
