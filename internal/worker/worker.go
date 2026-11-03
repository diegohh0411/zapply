// Package worker runs durable model tasks. Administration and usage queries run
// synchronously in the API, independently of this process.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/membership"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Args struct {
	UserID      uuid.UUID `json:"user_id" river:"unique"`
	OperationID uuid.UUID `json:"operation_id" river:"unique"`
	RequestID   string    `json:"request_id,omitempty"`
	TraceParent string    `json:"traceparent,omitempty"`
}

func (Args) Kind() string { return "model_operation" }

type Operation struct {
	ID             uuid.UUID  `json:"id"`
	Kind           string     `json:"kind"`
	UserID         uuid.UUID  `json:"user_id"`
	Status         string     `json:"status"`
	BlockedReason  string     `json:"blocked_reason"`
	NextEligibleAt *time.Time `json:"next_eligible_at"`
	Result         string     `json:"result"`
	Version        int        `json:"version"`
	CreatedAt      time.Time  `json:"created_at"`
}
type Attempt struct {
	ID           uuid.UUID `json:"id"`
	KeyID        uuid.UUID `json:"key_id"`
	State        string    `json:"state"`
	GenerationID *string   `json:"generation_id"`
	Deadline     time.Time `json:"deadline_at"`
	Number       int       `json:"attempt_number"`
}

type Queue struct {
	store  *store.Store
	client *river.Client[pgx.Tx]
}

func NewQueue(st *store.Store) (*Queue, error) {
	c, err := river.NewClient(riverpgxv5.New(st.Pool()), &river.Config{Schema: "river"})
	return &Queue{st, c}, err
}
func (q *Queue) Enqueue(ctx context.Context, u uuid.UUID, key, requestID string) (Operation, error) {
	ctx, span := otel.Tracer("zapply/worker").Start(ctx, "modelapi.enqueue")
	defer span.End()
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if key == "" || len(key) > 255 || strings.ContainsAny(key, "\x00\r\n") {
		return Operation{}, &library.ValidationError{Fields: []library.FieldError{{Field: "Idempotency-Key", Message: "must be 1 to 255 characters"}}}
	}
	id := uuid.Must(uuid.NewV7())
	h := sha256.Sum256([]byte("diagnostic:v1"))
	hash := hex.EncodeToString(h[:])
	var op Operation
	err := q.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT platform.assert_owner()"); err != nil {
			return err
		}
		var inserted uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO model_operation(id,user_id,idempotency_key,request_hash) VALUES($1,$2,$3,$4)
   ON CONFLICT(user_id,kind,idempotency_key) DO NOTHING RETURNING id`, id, u, key, hash).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			var oldHash string
			if err := tx.QueryRow(ctx, "SELECT id,request_hash FROM model_operation WHERE user_id=$1 AND kind='diagnostic' AND idempotency_key=$2", u, key).Scan(&id, &oldHash); err != nil {
				return err
			}
			if oldHash != hash {
				return &library.ConflictError{Message: "idempotency key belongs to a different request"}
			}
		} else if err != nil {
			return err
		} else {
			if _, err := q.client.InsertTx(ctx, tx, Args{UserID: u, OperationID: id, RequestID: requestID, TraceParent: carrier.Get("traceparent")}, insertOptions()); err != nil {
				return err
			}
		}
		var raw []byte
		if err := tx.QueryRow(ctx, "SELECT to_jsonb(o) FROM model_operation o WHERE id=$1", id).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &op)
	})
	return op, membership.Translate(err)
}
func (q *Queue) Get(ctx context.Context, u, id uuid.UUID) (Operation, error) {
	var op Operation
	err := q.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT platform.assert_owner()"); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, "SELECT to_jsonb(o) FROM model_operation o WHERE id=$1 AND user_id=$2", id, u).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &op)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = library.ErrNotFound
	}
	return op, membership.Translate(err)
}

type Runtime struct {
	river.WorkerDefaults[Args]
	Store           *store.Store
	Provider        modelapi.Provider
	Keys            *secrets.Ring
	Log             *slog.Logger
	OnTerminal      func(context.Context, pgx.Tx, Operation) error
	AddWorkers      func(*river.Workers)
	RecoverFeatures func(context.Context, *river.Client[pgx.Tx]) error
}

func (r *Runtime) Client(concurrency int) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, r)
	if r.AddWorkers != nil {
		r.AddWorkers(workers)
	}
	return river.NewClient(riverpgxv5.New(r.Store.Pool()), &river.Config{Schema: "river", Workers: workers,
		Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: concurrency}}, Logger: r.Log, JobTimeout: 2 * time.Minute})
}

func (r *Runtime) tx(ctx context.Context, u uuid.UUID, fn func(pgx.Tx) error) error {
	return r.Store.InTenant(ctx, u, fn)
}
func (r *Runtime) Work(ctx context.Context, j *river.Job[Args]) error {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": j.Args.TraceParent})
	ctx, span := otel.Tracer("zapply/worker").Start(ctx, "modelapi.operation", trace.WithAttributes(attribute.String("operation.id", j.Args.OperationID.String()), attribute.String("user.id", j.Args.UserID.String())))
	defer span.End()
	err := r.Process(ctx, j.Args)
	// Snoozing is ordinary scheduling, not a provider failure.
	if err != nil && !strings.Contains(err.Error(), "snooze") {
		span.AddEvent("worker returned an error")
	}
	return err
}

func insertOptions() *river.InsertOpts {
	return &river.InsertOpts{MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true,
		ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

// Recover re-enqueues abandoned/nonterminal operations without reading tenant
// results or prompts across users. River uniqueness and dispatch fencing make
// multiple recovery scanners safe.
func (r *Runtime) Recover(ctx context.Context, client *river.Client[pgx.Tx]) error {
	var raw []byte
	if err := r.Store.Pool().QueryRow(ctx, "SELECT platform.pending_operations(1000)").Scan(&raw); err != nil {
		return err
	}
	var pending []Args
	if err := json.Unmarshal(raw, &pending); err != nil {
		return err
	}
	for _, args := range pending {
		if _, err := client.Insert(ctx, args, insertOptions()); err != nil {
			return err
		}
	}
	if r.RecoverFeatures != nil {
		return r.RecoverFeatures(ctx, client)
	}
	return nil
}

// Process is deliberately idempotent across River rescue, retries, and crashes.
func (r *Runtime) Process(ctx context.Context, args Args) error {
	var op Operation
	var attempts []Attempt
	var decision *systemone.Request
	if err := r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, "SELECT to_jsonb(o) FROM model_operation o WHERE id=$1 AND user_id=$2", args.OperationID, args.UserID).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &op); err != nil {
			return err
		}
		if op.Kind == "decision" {
			var body []byte
			if err := tx.QueryRow(ctx, "SELECT body FROM model_request WHERE operation_id=$1 AND user_id=$2", op.ID, args.UserID).Scan(&body); err != nil {
				return err
			}
			var req systemone.Request
			if err := json.Unmarshal(body, &req); err != nil {
				return err
			}
			decision = &req
		}
		if err := tx.QueryRow(ctx, "SELECT platform.attempts_for_operation($1)", args.OperationID).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &attempts)
	}); err != nil {
		return err
	}
	switch op.Status {
	case "completed", "failed", "cancelled":
		return r.tx(ctx, args.UserID, func(tx pgx.Tx) error { return r.terminal(ctx, tx, args) })
	}
	if len(attempts) > 0 {
		a := attempts[0]
		if a.State == "dispatching" || a.State == "uncertain" {
			if a.State == "dispatching" && time.Now().Before(a.Deadline) {
				return river.JobSnooze(time.Until(a.Deadline))
			}
			if a.State == "dispatching" {
				if err := r.uncertain(ctx, args, a.ID, a.GenerationID, "reconciling", ""); err != nil {
					return err
				}
			}
			wait := reconcileDelay(a.Deadline)
			lookupAllowed := true
			if decision != nil {
				dp, ok := r.Provider.(modelapi.DecisionProvider)
				lookupAllowed = ok && dp.DecisionLookupSupported(decision.Model)
			}
			if !lookupAllowed || a.GenerationID == nil {
				return river.JobSnooze(wait)
			}
			key, err := r.key(ctx, args.UserID, a.KeyID)
			if err != nil {
				return river.JobSnooze(wait)
			}
			result, err := r.Provider.Lookup(ctx, key, *a.GenerationID)
			if err != nil || result.Amount == nil {
				return river.JobSnooze(wait)
			}
			return r.settle(ctx, args, a.ID, result, op.Result, "generation_lookup", false, "")
		}
		// An owner resolved accounting; no generation is repeated to recover output.
		if op.Status == "needs_review" || op.Status == "running" {
			return r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "UPDATE model_operation SET status='failed',blocked_reason='result_unavailable',version=version+1,updated_at=now() WHERE id=$1", op.ID)
				if err != nil {
					return err
				}
				if err := emit(ctx, tx, args.UserID, op.ID, "model_operation.changed"); err != nil {
					return err
				}
				return r.terminal(ctx, tx, args)
			})
		}
	}
	var quote modelapi.Quote
	var err error
	if decision != nil {
		dp, ok := r.Provider.(modelapi.DecisionProvider)
		if !ok {
			return r.block(ctx, args, "decision_provider_unavailable", nil, 5*time.Minute)
		}
		quote, err = dp.QuoteDecision(ctx, *decision)
	} else {
		quote, err = r.Provider.Quote(ctx)
	}
	if decision != nil && quote.Model != "" {
		if e := r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO platform.model_quote(model,quote) VALUES($1,$2) ON CONFLICT(model) DO UPDATE SET quote=EXCLUDED.quote,fetched_at=now()`, quote.Model, systemone.Raw(quote))
			return e
		}); e != nil {
			return e
		}
	}
	if err != nil {
		reason := "provider_metadata_unavailable"
		var pe *modelapi.ProviderError
		if errors.As(err, &pe) {
			reason = pe.Code
		}
		if decision != nil && reason == "request_too_large" && quote.Decision {
			return r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
				tag, e := tx.Exec(ctx, `UPDATE model_operation SET status='failed',blocked_reason='request_too_large',version=version+1,updated_at=now() WHERE id=$1 AND user_id=$2 AND status IN ('pending','blocked')`, op.ID, args.UserID)
				if e != nil {
					return e
				}
				if tag.RowsAffected() == 0 {
					return nil
				}
				return r.terminal(ctx, tx, args)
			})
		}
		return r.block(ctx, args, reason, nil, 5*time.Minute)
	}
	id := uuid.Must(uuid.NewV7())
	rawQuote, _ := json.Marshal(quote)
	hashInput := rawQuote
	if decision != nil {
		hashInput = systemone.Raw(map[string]any{"quote": quote, "request": decision})
	}
	hash := sha256.Sum256(hashInput)
	var reservation struct {
		Reason string    `json:"reason"`
		KeyID  uuid.UUID `json:"key_id"`
	}
	if err := r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, "SELECT platform.reserve($1,$2,$3,$4,$5,$6,$7)", id, args.OperationID, quote.Model, hex.EncodeToString(hash[:]), json.RawMessage(rawQuote), quote.Reserved, time.Now().Add(3*time.Minute)).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &reservation)
	}); err != nil {
		return err
	}
	if reservation.Reason != "" {
		if reservation.Reason == "existing_attempt" {
			return river.JobSnooze(time.Minute)
		}
		if reservation.Reason == "operation_not_pending" {
			return nil
		}
		var next *time.Time
		if reservation.Reason == "usage_limit" {
			_ = r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT platform.eligible_at($1)", quote.Reserved).Scan(&next)
			})
		}
		delay := time.Minute
		if next != nil && time.Until(*next) > 0 {
			delay = min(time.Until(*next), time.Minute)
		}
		return r.block(ctx, args, reservation.Reason, next, delay)
	}
	key, err := r.key(ctx, args.UserID, reservation.KeyID)
	if err != nil {
		// The key is only readable through this attempt, so a failure still
		// releases one reservation; cap retries like other uncharged failures.
		zero := int64(0)
		retry := len(attempts) < 4
		if err := r.settle(ctx, args, id, modelapi.Result{Amount: &zero}, "", "confirmed_uncharged", retry, "encryption_unavailable"); err != nil {
			return err
		}
		if retry {
			return river.JobSnooze(time.Minute)
		}
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	var result modelapi.Result
	if decision != nil {
		result, err = r.Provider.(modelapi.DecisionProvider).Decide(callCtx, key, *decision, quote)
	} else {
		result, err = r.Provider.Generate(callCtx, key, quote)
	}
	cancel()
	// Keep a bounded cleanup context after cancellation so a charged result can be
	// recorded during drain instead of triggering a second external call.
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer stop()
	if result.Amount != nil {
		if *result.Amount > quote.Reserved && r.Log != nil {
			r.Log.Error("provider charge exceeded reservation", "user_id", args.UserID, "operation_id", args.OperationID, "attempt_id", id, "reserved_usd", modelapi.USD(quote.Reserved), "actual_usd", modelapi.USD(*result.Amount))
		}
		return r.settleCharged(ctx, args, id, result)
	}
	var pe *modelapi.ProviderError
	if errors.As(err, &pe) && pe.Uncharged {
		zero := int64(0)
		result.Amount = &zero
		retry := len(attempts) < 4 && (pe.Code == "rate_limited" || pe.Code == "provider_balance" || pe.Code == "provider_budget" || pe.Code == "request_rejected" || pe.Code == "stale_quote")
		e := r.settle(cleanup, args, id, result, "", "confirmed_uncharged", retry, pe.Code)
		if e != nil {
			return e
		}
		if retry {
			return river.JobSnooze(max(time.Minute, min(pe.RetryAfter, 5*time.Minute)))
		}
		return nil
	}
	code := "accounting_pending"
	if pe != nil {
		code = pe.Code
	}
	var generation *string
	if result.GenerationID != "" {
		generation = &result.GenerationID
	}
	if e := r.uncertain(cleanup, args, id, generation, code, result.Text); e != nil {
		return e
	}
	return river.JobSnooze(time.Minute)
}

// reconcileDelay backs off as an unresolved attempt ages: one minute at first,
// growing to an hour about four hours after the deadline.
func reconcileDelay(deadline time.Time) time.Duration {
	return min(time.Hour, max(time.Minute, time.Since(deadline)/4))
}

func (r *Runtime) key(ctx context.Context, u, id uuid.UUID) (string, error) {
	var encrypted []byte
	var version string
	err := r.tx(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT ciphertext,encryption_version FROM platform.load_key($1)", id).Scan(&encrypted, &version)
	})
	if err != nil {
		return "", err
	}
	return r.Keys.Decrypt(id.String(), version, encrypted)
}
func (r *Runtime) uncertain(ctx context.Context, args Args, id uuid.UUID, generation *string, reason, result string) error {
	return r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT platform.uncertain($1,$2,$3)", id, generation, reason); err != nil {
			return err
		}
		if result != "" {
			_, err := tx.Exec(ctx, "UPDATE model_operation SET result=$1 WHERE id=$2", stripNUL(result), args.OperationID)
			return err
		}
		return nil
	})
}

// stripNUL removes bytes Postgres text cannot store, which would fail every retry.
func stripNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

// settleCharged records a provider-reported charge. Losing it would turn a
// known cost into a manual resolution, so transient database failures are
// retried; if the charge still cannot be saved, the generation ID is kept so the
// reconciliation lookup can recover the cost.
func (r *Runtime) settleCharged(ctx context.Context, args Args, id uuid.UUID, result modelapi.Result) error {
	text := stripNUL(result.Text)
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer stop()
	var err error
	for wait := 250 * time.Millisecond; ; wait *= 2 {
		if err = r.settle(cleanup, args, id, result, text, "provider_response", false, ""); err == nil {
			return nil
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) && strings.HasPrefix(pe.Code, "ZA") {
			break // Application rejections are deterministic.
		}
		select {
		case <-cleanup.Done():
		case <-time.After(wait):
			continue
		}
		break
	}
	if r.Log != nil {
		r.Log.Error("provider charge could not be saved", "user_id", args.UserID, "operation_id", args.OperationID, "attempt_id", id, "error", err)
	}
	var generation *string
	if result.GenerationID != "" {
		generation = &result.GenerationID
	}
	last, stopLast := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stopLast()
	if e := r.uncertain(last, args, id, generation, "settlement_failed", text); e != nil {
		return errors.Join(err, e)
	}
	return river.JobSnooze(time.Minute)
}
func (r *Runtime) settle(ctx context.Context, args Args, id uuid.UUID, result modelapi.Result, text, source string, retry bool, reason string) error {
	var generation *string
	if result.GenerationID != "" {
		generation = &result.GenerationID
	}
	usage := result.Usage
	if !json.Valid(usage) {
		usage = json.RawMessage(`{}`)
	}
	err := r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT platform.settle($1,$2,$3,$4,$5,$6)", id, *result.Amount, generation, usage, text, source); err != nil {
			return err
		}
		if retry {
			_, err := tx.Exec(ctx, "UPDATE model_operation SET status='blocked',blocked_reason=$1,next_eligible_at=NULL WHERE id=$2", reason, args.OperationID)
			return err
		}
		if reason != "" {
			// Keep why a final uncharged attempt failed, not only that it did.
			_, err := tx.Exec(ctx, "UPDATE model_operation SET blocked_reason=$1 WHERE id=$2 AND status='failed'", reason, args.OperationID)
			if err != nil {
				return err
			}
			return r.terminal(ctx, tx, args)
		}
		return r.terminal(ctx, tx, args)
	})
	return err
}
func (r *Runtime) block(ctx context.Context, args Args, reason string, next *time.Time, delay time.Duration) error {
	err := r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE model_operation SET status='blocked',blocked_reason=$1,next_eligible_at=$2,version=version+1,updated_at=now() WHERE id=$3 AND status IN('pending','blocked')", reason, next, args.OperationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return emit(ctx, tx, args.UserID, args.OperationID, "model_operation.changed")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return river.JobSnooze(delay)
}
func emit(ctx context.Context, tx pgx.Tx, u, id uuid.UUID, topic string) error {
	payload, _ := json.Marshal(map[string]any{"id": id})
	_, err := tx.Exec(ctx, "INSERT INTO outbox(id,user_id,topic,payload) VALUES($1,$2,$3,$4)", uuid.Must(uuid.NewV7()), u, topic, json.RawMessage(payload))
	return err
}

func (r *Runtime) terminal(ctx context.Context, tx pgx.Tx, args Args) error {
	if r.OnTerminal == nil {
		return nil
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT to_jsonb(o) FROM model_operation o WHERE id=$1 AND user_id=$2", args.OperationID, args.UserID).Scan(&raw); err != nil {
		return err
	}
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return err
	}
	return r.OnTerminal(ctx, tx, op)
}

// InsertTx shares the insert-only queue with feature jobs.
func (q *Queue) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs) error {
	_, err := q.client.InsertTx(ctx, tx, args, insertOptions())
	return err
}
