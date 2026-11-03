package mail

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type syncWorker struct {
	river.WorkerDefaults[SyncArgs]
	s *Service
}

func (w *syncWorker) Work(ctx context.Context, j *river.Job[SyncArgs]) error {
	return w.s.Sync(ctx, j.Args.UserID, j.Args.AccountID)
}

type watchWorker struct {
	river.WorkerDefaults[WatchArgs]
	s *Service
}

func (w *watchWorker) Work(ctx context.Context, j *river.Job[WatchArgs]) error {
	return w.s.RenewWatch(ctx, j.Args.UserID, j.Args.AccountID)
}

type teardownWorker struct {
	river.WorkerDefaults[TeardownArgs]
	s *Service
}

func (w *teardownWorker) Work(ctx context.Context, j *river.Job[TeardownArgs]) error {
	return w.s.Teardown(ctx, j.Args.UserID, j.Args.AccountID)
}

type classifyWorker struct {
	river.WorkerDefaults[ClassifyArgs]
	s *Service
}

func (w *classifyWorker) Work(ctx context.Context, j *river.Job[ClassifyArgs]) error {
	return w.s.Classify(ctx, j.Args.UserID, j.Args.MessageID)
}

func (s *Service) AddWorkers(w *river.Workers) {
	river.AddWorker(w, &syncWorker{s: s})
	river.AddWorker(w, &watchWorker{s: s})
	river.AddWorker(w, &teardownWorker{s: s})
	river.AddWorker(w, &classifyWorker{s: s})
}

// Recover is the safety net under push notifications. It syncs accounts that
// have gone quiet for the reconcile interval (a notification may have been
// dropped), renews watches before they lapse, and re-wakes messages whose
// classification job was lost. Each insert is unique, so concurrent workers and
// repeated scans do not pile up jobs.
func (s *Service) Recover(ctx context.Context, client *river.Client[pgx.Tx]) error {
	now := time.Now()
	bucket := now.Truncate(max(s.reconcile, time.Minute)).Format(time.RFC3339)
	var watchBefore *time.Time
	if s.push {
		t := now.Add(48 * time.Hour)
		watchBefore = &t
	}
	var raw []byte
	if e := s.store.Pool().QueryRow(ctx, `SELECT platform.mail_accounts_due(1000,$1,$2)`, now.Add(-s.reconcile), watchBefore).Scan(&raw); e != nil {
		return e
	}
	var due []struct {
		UserID    uuid.UUID `json:"user_id"`
		AccountID uuid.UUID `json:"account_id"`
		Sync      bool      `json:"sync"`
		Watch     bool      `json:"watch"`
	}
	if e := json.Unmarshal(raw, &due); e != nil {
		return e
	}
	for _, d := range due {
		u, a := d.UserID, d.AccountID
		if d.Sync {
			if _, e := client.Insert(ctx, SyncArgs{UserID: u, AccountID: a, Tick: "reconcile:" + bucket}, insertOpts()); e != nil {
				return e
			}
		}
		if d.Watch {
			if _, e := client.Insert(ctx, WatchArgs{UserID: u, AccountID: a, Tick: bucket}, insertOpts()); e != nil {
				return e
			}
		}
	}
	if e := s.store.Pool().QueryRow(ctx, `SELECT platform.mail_pending_messages(1000,$1)`, now.Add(-15*time.Minute)).Scan(&raw); e != nil {
		return e
	}
	var pending []struct {
		UserID    uuid.UUID `json:"user_id"`
		MessageID uuid.UUID `json:"message_id"`
	}
	if e := json.Unmarshal(raw, &pending); e != nil {
		return e
	}
	for _, p := range pending {
		if _, e := client.Insert(ctx, ClassifyArgs{UserID: p.UserID, MessageID: p.MessageID, Wake: "recover:" + now.Truncate(time.Minute).Format(time.RFC3339)}, insertOpts()); e != nil {
			return e
		}
	}
	return nil
}

// insertOpts matches the shared queue: unique by the args tagged unique, until
// the job finishes.
func insertOpts() *river.InsertOpts {
	return &river.InsertOpts{MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true,
		ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}
