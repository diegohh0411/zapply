package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/riverqueue/river"

	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/summary"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SummaryRequest struct {
	// Rows, when set, is how many lines the summary must wrap to in the
	// template version and locale below; MinCharacters..MaxCharacters is the
	// range the instruction asks for.
	Rows            int    `json:"rows,omitempty"`
	MinCharacters   int    `json:"min_characters,omitempty"`
	Template        string `json:"template,omitempty"`
	TemplateVersion int    `json:"template_version,omitempty"`
	Locale          string `json:"locale,omitempty"`
	// Instruction is the system instruction; empty uses the character cap's.
	Instruction     string            `json:"instruction,omitempty"`
	MaxCharacters   int               `json:"max_characters,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Model           string            `json:"model"`
	Endpoint        string            `json:"endpoint"`
	JobID           uuid.UUID         `json:"job_id"`
	Input           string            `json:"input"`
	Messages        []summary.Message `json:"messages"`
}
type ChatProvider interface {
	modelapi.ChatPricer
	GenerateMessages(context.Context, string, modelapi.Quote, any) (modelapi.Result, error)
}

// Poll durable invalidation across API/worker processes. Cancellation never
// discards provider accounting: Process settles using its cleanup context.
func (r *Runtime) watchSummary(ctx context.Context, cancel context.CancelFunc, args Args, job uuid.UUID) func() {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				var stale bool
				err := r.tx(ctx, args.UserID, func(tx pgx.Tx) error {
					return tx.QueryRow(ctx, "SELECT COALESCE((SELECT dirty OR operation_id IS DISTINCT FROM $1 FROM cv_summary WHERE user_id=$2 AND job_id=$3),true)", args.OperationID, args.UserID, job).Scan(&stale)
				})
				if err != nil || stale {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done); <-exited }
}

func (q *Queue) EnqueueSummary(ctx context.Context, tx pgx.Tx, u uuid.UUID, hash string, request SummaryRequest) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	body, err := json.Marshal(request)
	if err != nil {
		return id, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'summary',$4,$3)`, id, u, hash, id.String())
	if err != nil {
		return id, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_request(user_id,operation_id,body) VALUES($1,$2,$3)`, u, id, json.RawMessage(body))
	if err != nil {
		return id, err
	}
	opts := insertOptions()
	opts.ScheduledAt = time.Now().Add(700 * time.Millisecond)
	_, err = q.client.InsertTx(ctx, tx, Args{UserID: u, OperationID: id}, opts)
	return id, err
}

// outputTokenCap preserves the original bound for requests queued before cap configuration.
func (r SummaryRequest) outputTokenCap() int {
	if r.MaxOutputTokens == 0 {
		return 256
	}
	return r.MaxOutputTokens
}

// SummaryMeasure counts the lines text wraps to as a summary in a template
// version and locale. The render package provides it; the worker can't
// import render, which queues summaries.
type SummaryMeasure func(ctx context.Context, template string, version int, locale, text string) (int, error)

// summaryRevisions is how many times a summary goes back to the model when it
// wraps to the wrong number of lines. After that an overlong one is trimmed.
const summaryRevisions = 2

// summaryTurns loads the conversation after the first request.
func summaryTurns(ctx context.Context, tx pgx.Tx, op uuid.UUID) ([]summary.Message, error) {
	rows, err := tx.Query(ctx, `SELECT role,content FROM summary_turn WHERE operation_id=$1 ORDER BY n`, op)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (summary.Message, error) {
		var m summary.Message
		return m, r.Scan(&m.Role, &m.Content)
	})
}

// sentences splits text after sentence ends, keeping their punctuation.
func sentences(text string) []string {
	var out []string
	runes := []rune(text)
	start := 0
	for i, r := range runes {
		switch r {
		case '.', '!', '?', '。':
			if i+1 == len(runes) || runes[i+1] == ' ' {
				out = append(out, strings.TrimSpace(string(runes[start:i+1])))
				start = i + 1
			}
		}
	}
	if rest := strings.TrimSpace(string(runes[start:])); rest != "" {
		out = append(out, rest)
	}
	return out
}

// trimToRows shortens text until it wraps to at most rows lines: whole
// sentences first, then words with an ellipsis when one sentence is too long.
func trimToRows(ctx context.Context, measure func(string) (int, error), text string, rows int) (string, error) {
	ss := sentences(text)
	for n := len(ss) - 1; n >= 1; n-- {
		t := strings.Join(ss[:n], " ")
		lines, err := measure(t)
		if err != nil {
			return "", err
		}
		if lines <= rows {
			return t, nil
		}
	}
	words := strings.Fields(ss[0])
	for n := len(words) - 1; n >= 1; n-- {
		t := strings.TrimRight(strings.Join(words[:n], " "), ",;:") + "…"
		lines, err := measure(t)
		if err != nil {
			return "", err
		}
		if lines <= rows {
			return t, nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	return "", summary.ErrCharacterLimit
}

// characterCap preserves the original limit for requests queued before configuration.
func (r SummaryRequest) characterCap() int {
	if r.MaxCharacters == 0 {
		return summary.MaxCharacters
	}
	return r.MaxCharacters
}

// conversation is what a call sends after the system instruction: the first
// request's messages, then the turns since.
func (r SummaryRequest) conversation(history []summary.Message) []summary.Message {
	return append(append([]summary.Message{}, r.Messages...), history...)
}

// checkRows measures a summary against its row target. It returns feedback
// to send back to the model, or the text to publish: as written when it fits,
// trimmed when revisions ran out and it is still too long. A measurement
// failure publishes the text unchecked rather than losing a paid answer.
func (r *Runtime) checkRows(ctx context.Context, args Args, chat *SummaryRequest, history []summary.Message, text string) (revise, out, reason string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	measure := func(t string) (int, error) {
		return r.MeasureSummary(ctx, chat.Template, chat.TemplateVersion, chat.Locale, t)
	}
	log := r.logger(ctx)
	lines, err := measure(text)
	if err != nil {
		log.WarnContext(ctx, "summary rows unmeasured", "operation_id", args.OperationID, "error", err.Error())
		return "", text, ""
	}
	revisions := len(history) / 2
	log.InfoContext(ctx, "summary rows measured", "operation_id", args.OperationID, "lines", lines, "rows", chat.Rows, "characters", utf8.RuneCountInString(text), "revision", revisions)
	if lines == chat.Rows || lines < chat.Rows && revisions >= summaryRevisions {
		return "", text, ""
	}
	if revisions < summaryRevisions {
		return summary.Revision(lines, chat.Rows, utf8.RuneCountInString(text), chat.MinCharacters, chat.MaxCharacters), "", ""
	}
	trimmed, err := trimToRows(ctx, measure, text, chat.Rows)
	if err != nil {
		log.WarnContext(ctx, "summary could not be trimmed to its rows", "operation_id", args.OperationID, "error", err.Error())
		return "", "", "summary_row_limit"
	}
	return "", trimmed, ""
}

// settleRevision records a charged answer that wrapped to the wrong number of
// rows, keeps it and the feedback as the next turns, and puts the operation
// back in the queue so the next attempt continues the conversation.
func (r *Runtime) settleRevision(ctx context.Context, args Args, id uuid.UUID, result modelapi.Result, history []summary.Message, feedback string) error {
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer stop()
	var generation *string
	if result.GenerationID != "" {
		generation = &result.GenerationID
	}
	usage := result.Usage
	if !json.Valid(usage) {
		usage = json.RawMessage(`{}`)
	}
	err := r.tx(cleanup, args.UserID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(cleanup, "SELECT platform.settle($1,$2,$3,$4,'','provider_response')", id, *result.Amount, generation, usage); err != nil {
			return err
		}
		n := len(history)
		for i, m := range []summary.Message{{Role: "assistant", Content: stripNUL(result.Text)}, {Role: "user", Content: feedback}} {
			if _, err := tx.Exec(cleanup, `INSERT INTO summary_turn(operation_id,n,user_id,role,content) VALUES($1,$2,$3,$4,$5)`, args.OperationID, n+i, args.UserID, m.Role, m.Content); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(cleanup, "UPDATE model_operation SET status='blocked',blocked_reason='summary_revising',result='',next_eligible_at=NULL,version=version+1,updated_at=now() WHERE id=$1", args.OperationID); err != nil {
			return err
		}
		return emit(cleanup, tx, args.UserID, args.OperationID, "model_operation.changed")
	})
	if err != nil {
		// The charge is known; record it as uncertain so it is reconciled
		// instead of charged twice.
		if e := r.uncertain(cleanup, args, id, generation, "settlement_failed", stripNUL(result.Text)); e != nil {
			return errors.Join(err, e)
		}
		return river.JobSnooze(time.Minute)
	}
	return river.JobSnooze(time.Second)
}
