// Package worker runs durable model tasks. Administration and usage queries run
// synchronously in the API, independently of this process.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/membership"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/summary"
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
	// MeasureSummary checks a summary's rows; nil accepts any length.
	MeasureSummary SummaryMeasure
}

func (r *Runtime) Client(concurrency int) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, r)
	if r.AddWorkers != nil {
		r.AddWorkers(workers)
	}
	return river.NewClient(riverpgxv5.New(r.Store.Pool()), &river.Config{Schema: "river", Workers: workers,
		// Inserts wake fetchers through LISTEN; polling is only the fallback.
		FetchPollInterval: 10 * time.Second,
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: concurrency}}, Logger: r.Log, JobTimeout: 2 * time.Minute})
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
	var chat *SummaryRequest
	var history []summary.Message
	var decision *systemone.Request
	var calibration []byte
	if err := r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, "SELECT to_jsonb(o) FROM model_operation o WHERE id=$1 AND user_id=$2", args.OperationID, args.UserID).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &op); err != nil {
			return err
		}
		if op.Kind == "summary" {
			chat = &SummaryRequest{}
			var body []byte
			if err := tx.QueryRow(ctx, "SELECT body FROM model_request WHERE operation_id=$1", op.ID).Scan(&body); err != nil {
				return err
			}
			if err := json.Unmarshal(body, chat); err != nil {
				return err
			}
			var err error
			if history, err = summaryTurns(ctx, tx, op.ID); err != nil {
				return err
			}
			var dirty bool
			if err := tx.QueryRow(ctx, "SELECT COALESCE((SELECT dirty OR operation_id IS DISTINCT FROM $1 FROM cv_summary WHERE user_id=$2 AND job_id=$3),true)", op.ID, args.UserID, chat.JobID).Scan(&dirty); err != nil {
				return err
			}
			if dirty {
				var cancelled bool
				if err := tx.QueryRow(ctx, "SELECT platform.cancel_operation($1)", op.ID).Scan(&cancelled); err != nil {
					return err
				}
				if cancelled {
					op.Status = "cancelled"
					return nil
				}
			}
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
			if err := tx.QueryRow(ctx, "SELECT platform.model_calibration($1)", req.Model).Scan(&calibration); err != nil {
				return err
			}
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
			model := ""
			if decision != nil {
				model = decision.Model
				dp, ok := r.Provider.(modelapi.DecisionProvider)
				lookupAllowed = ok && dp.DecisionLookupSupported(decision.Model)
			} else if chat != nil {
				model = chat.Model
			}
			// Every way out of this loop that leaves the attempt unsettled is
			// logged with everything known about it, so a stuck charge can be
			// diagnosed from the logs instead of by hand in the database.
			unsettled := func(why string, err error, extra ...any) error {
				fields := append([]any{
					"why", why,
					"user_id", args.UserID,
					"operation_id", args.OperationID,
					"operation_kind", op.Kind,
					"operation_status", op.Status,
					"operation_blocked_reason", op.BlockedReason,
					"operation_created_at", op.CreatedAt,
					"operation_age", time.Since(op.CreatedAt).String(),
					"attempt_id", a.ID,
					"attempt_number", a.Number,
					"attempt_state", a.State,
					"attempt_key_id", a.KeyID,
					"attempt_generation_id", a.GenerationID,
					"attempt_deadline", a.Deadline,
					"past_deadline", time.Since(a.Deadline).String(),
					"attempts_total", len(attempts),
					"model", model,
					"lookup_supported", lookupAllowed,
					"provider", fmt.Sprintf("%T", r.Provider),
					"next_check_in", wait.String(),
				}, extra...)
				if err != nil {
					fields = append(fields, "error", err.Error(), "error_type", fmt.Sprintf("%T", err))
					var pe *modelapi.ProviderError
					if errors.As(err, &pe) {
						fields = append(fields, "provider_code", pe.Code, "provider_uncharged", pe.Uncharged, "provider_retry_after", pe.RetryAfter.String())
					}
				}
				r.logger(ctx).WarnContext(ctx, "model attempt still unsettled", fields...)
				return river.JobSnooze(wait)
			}
			if !lookupAllowed {
				return unsettled("provider_lookup_unsupported", nil)
			}
			if a.GenerationID == nil {
				return unsettled("no_generation_id", nil)
			}
			key, err := r.key(ctx, args.UserID, a.KeyID)
			if err != nil {
				return unsettled("key_unavailable", err)
			}
			result, err := r.Provider.Lookup(ctx, key, *a.GenerationID)
			if err != nil {
				return unsettled("lookup_failed", err)
			}
			if result.Amount == nil {
				return unsettled("lookup_without_amount", nil, "lookup_generation_id", result.GenerationID, "lookup_usage", string(result.Usage), "lookup_has_text", result.Text != "")
			}
			r.logger(ctx).InfoContext(ctx, "model attempt settled by generation lookup", "user_id", args.UserID, "operation_id", args.OperationID, "attempt_id", a.ID, "generation_id", *a.GenerationID, "amount", *result.Amount, "usage", string(result.Usage), "past_deadline", time.Since(a.Deadline).String())
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
		quote, err = modelapi.QuoteDecision(ctx, dp, *decision, modelapi.CalibrationOf(calibration))
	} else if chat != nil {
		p, ok := r.Provider.(ChatProvider)
		if !ok {
			return r.block(ctx, args, "chat_provider_unavailable", nil, time.Minute)
		}
		quote, err = modelapi.QuoteChat(ctx, p, modelapi.ChatRequest{Model: chat.Model, Endpoint: chat.Endpoint, Messages: chat.conversation(history), MaxOutput: chat.outputTokenCap(), ReasoningEffort: chat.ReasoningEffort}, billing.Calibration{})
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
		// The member's other calls hold every slot; wait for one without blocking.
		if reservation.Reason == "in_flight" {
			return river.JobSnooze(5 * time.Second)
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
	logger := r.Log
	if logger == nil {
		logger = logx.From(ctx)
	}
	callCtx = logx.With(callCtx, logger.With("operation_id", args.OperationID, "attempt_id", id, "model", quote.Model))
	var result modelapi.Result
	summaryReason := ""
	if decision != nil {
		result, err = r.Provider.(modelapi.DecisionProvider).Decide(callCtx, key, *decision, quote)
	} else if chat != nil {
		stop := r.watchSummary(callCtx, cancel, args, chat.JobID)
		var text string
		text, err = summary.Run(callCtx, summary.Config{Model: chat.Model, MaxOutputTokens: chat.outputTokenCap(), MaxCharacters: chat.characterCap(), Instruction: chat.Instruction, History: history}, chat.Input, func(ctx context.Context, _ string, messages []summary.Message) (string, error) {
			raw, _ := json.Marshal(messages)
			reserved, _ := json.Marshal(chat.conversation(history))
			if len(raw) > len(reserved) {
				return "", &modelapi.ProviderError{Code: "request_too_large", Uncharged: true}
			}
			result, err = r.Provider.(ChatProvider).GenerateMessages(ctx, key, quote, messages)
			return result.Text, err
		})
		stop()
		if err != nil {
			switch {
			case errors.Is(err, summary.ErrCharacterLimit):
				summaryReason = "summary_character_limit"
			case errors.Is(err, summary.ErrEmptyOutput):
				summaryReason = "summary_empty_output"
			}
			if r.Log != nil {
				r.Log.Warn("summary generation failed", "operation_id", args.OperationID, "job_id", chat.JobID, "model", chat.Model, "reason", summaryReason, "output_characters", utf8.RuneCountInString(result.Text), "max_characters", chat.characterCap())
			}
			result.Text = ""
		} else {
			result.Text = text
		}
	} else {
		result, err = r.Provider.Generate(callCtx, key, quote)
	}
	cancel()
	// Keep a bounded cleanup context after cancellation so a charged result can be
	// recorded during drain instead of triggering a second external call.
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer stop()
	if chat != nil && result.Amount != nil && result.Text != "" && chat.Rows > 0 && r.MeasureSummary != nil {
		revise, text, reason := r.checkRows(cleanup, args, chat, history, result.Text)
		if revise != "" {
			return r.settleRevision(ctx, args, id, result, history, revise)
		}
		result.Text, summaryReason = text, reason
	}
	if result.Amount != nil {
		if *result.Amount > quote.Reserved && r.Log != nil {
			r.Log.Error("provider charge exceeded reservation", "user_id", args.UserID, "operation_id", args.OperationID, "attempt_id", id, "reserved_usd", modelapi.USD(quote.Reserved), "actual_usd", modelapi.USD(*result.Amount))
		}
		return r.settleCharged(ctx, args, id, result, summaryReason)
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
	fields := []any{"user_id", args.UserID, "operation_id", args.OperationID, "operation_kind", op.Kind, "attempt_id", id, "attempt_number", len(attempts) + 1, "key_id", reservation.KeyID, "model", quote.Model, "reserved", quote.Reserved, "reason", code, "generation_id", result.GenerationID, "has_result", result.Text != "", "result_characters", utf8.RuneCountInString(result.Text), "usage", string(result.Usage), "provider", fmt.Sprintf("%T", r.Provider), "context_error", fmt.Sprint(callCtx.Err())}
	if err != nil {
		fields = append(fields, "error", err.Error(), "error_type", fmt.Sprintf("%T", err))
	}
	if pe != nil {
		fields = append(fields, "provider_code", pe.Code, "provider_uncharged", pe.Uncharged, "provider_retry_after", pe.RetryAfter.String())
	}
	logger.WarnContext(cleanup, "model request accounting uncertain", fields...)
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
func (r *Runtime) settleCharged(ctx context.Context, args Args, id uuid.UUID, result modelapi.Result, reason string) error {
	text := stripNUL(result.Text)
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer stop()
	var err error
	for wait := 250 * time.Millisecond; ; wait *= 2 {
		if err = r.settle(cleanup, args, id, result, text, "provider_response", false, reason); err == nil {
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
		r.Log.Error("provider charge could not be saved", "user_id", args.UserID, "operation_id", args.OperationID, "attempt_id", id, "generation_id", result.GenerationID, "amount", fmt.Sprint(derefAmount(result.Amount)), "usage", string(result.Usage), "has_result", result.Text != "", "reason", reason, "error", fmt.Sprint(err), "error_type", fmt.Sprintf("%T", err))
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

// logger returns the runtime's logger, falling back to the request's.
func (r *Runtime) logger(ctx context.Context) *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return logx.From(ctx)
}

// derefAmount reports a charge for logs; nil means the provider gave none.
func derefAmount(a *int64) any {
	if a == nil {
		return nil
	}
	return *a
}
