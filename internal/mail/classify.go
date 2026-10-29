package mail

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/diegohh0411/zapply/internal/decide"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

const (
	// Attempts per question before giving up. A relevant message whose job-link
	// question keeps failing is still kept, just unlinked.
	categoryAttempts = 3
	jobAttempts      = 2
	// maxInFlight bounds a member's open mail requests, so a backfill (or a
	// member without allowance) does not park hundreds of blocked operations. It
	// stays well under the gateway's per-member limit so matching keeps its slots.
	maxInFlight = 3
	// maxJobOptions bounds the job-link question's choices.
	maxJobOptions = 40
)

// ClassifyArgs is unique per message and waking operation: River keeps a running
// job unique, and a finished request must still be able to wake the message.
type ClassifyArgs struct {
	UserID    uuid.UUID `json:"user_id" river:"unique"`
	MessageID uuid.UUID `json:"message_id" river:"unique"`
	Wake      string    `json:"wake,omitempty" river:"unique"`
}

func (ClassifyArgs) Kind() string { return "mail_classify" }

type pendingMessage struct {
	ID         uuid.UUID
	State      string
	Category   *string
	From       string
	Subject    string
	Snippet    string
	ReceivedAt time.Time
}

type jobOption struct {
	ID    uuid.UUID
	Label string
}

func (s *Service) loadMessage(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) (pendingMessage, error) {
	var m pendingMessage
	e := tx.QueryRow(ctx, `SELECT id,state,category,COALESCE(from_address,''),COALESCE(subject,''),COALESCE(snippet,''),received_at FROM mail_message WHERE user_id=$1 AND id=$2 FOR UPDATE`, u, id).
		Scan(&m.ID, &m.State, &m.Category, &m.From, &m.Subject, &m.Snippet, &m.ReceivedAt)
	return m, e
}

// Classify moves one message toward a final state. It is safe to run any number
// of times: it only dispatches a question when none is open, and finishes the
// message from the answers OnTerminal recorded.
func (s *Service) Classify(ctx context.Context, u, id uuid.UUID) error {
	var snooze time.Duration
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		m, e := s.loadMessage(ctx, tx, u, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if m.State != "pending" {
			return nil
		}
		// A paused mailbox sends nothing to the model, including mail already read;
		// resuming picks the waiting messages up again. A disconnected one drops them.
		var account string
		if e = tx.QueryRow(ctx, `SELECT a.status FROM mail_message m JOIN mail_account a ON a.user_id=m.user_id AND a.id=m.account_id WHERE m.user_id=$1 AND m.id=$2`, u, id).Scan(&account); e != nil {
			return e
		}
		switch account {
		case "paused":
			return nil
		case "disconnected":
			return s.giveUp(ctx, tx, u, m)
		}
		var open, inFlight int
		if e = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE message_id=$2),count(*) FROM mail_operation WHERE user_id=$1 AND NOT ingested`, u, id).Scan(&open, &inFlight); e != nil {
			return e
		}
		if open > 0 {
			return nil // OnTerminal wakes this message when the request ends
		}
		attempts := map[string]int{}
		jobDone := false
		rows, e := tx.Query(ctx, `SELECT stage,count(*),bool_or(ok) FROM mail_operation WHERE user_id=$1 AND message_id=$2 GROUP BY stage`, u, id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var stage string
			var n int
			var ok bool
			if e = rows.Scan(&stage, &n, &ok); e != nil {
				rows.Close()
				return e
			}
			attempts[stage] = n
			jobDone = jobDone || (stage == "job" && ok)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}

		stage := ""
		var jobs []jobOption
		switch {
		case m.Category == nil && attempts["category"] >= categoryAttempts:
			return s.giveUp(ctx, tx, u, m)
		case m.Category == nil:
			stage = "category"
		case *m.Category != "other" && !jobDone && attempts["job"] < jobAttempts:
			if jobs, e = s.openJobs(ctx, tx, u); e != nil {
				return e
			}
			if len(jobs) > 0 {
				stage = "job"
			}
		}
		if stage == "" {
			return s.finish(ctx, tx, u, m)
		}
		if inFlight >= maxInFlight {
			snooze = 2 * time.Minute
			return nil
		}
		var model *string
		if e = tx.QueryRow(ctx, `SELECT platform.selected_decision_model()`).Scan(&model); e != nil {
			return e
		}
		if model == nil {
			snooze = 5 * time.Minute
			return nil
		}
		return s.dispatch(ctx, tx, u, m, *model, stage, jobs)
	})
	if err != nil {
		return err
	}
	if snooze > 0 {
		return river.JobSnooze(snooze)
	}
	return nil
}

func (s *Service) openJobs(ctx context.Context, tx pgx.Tx, u uuid.UUID) ([]jobOption, error) {
	rows, e := tx.Query(ctx, `SELECT j.id,j.title,j.company FROM job j JOIN job_status st ON st.user_id=j.user_id AND st.id=j.status_id
 WHERE j.user_id=$1 AND j.archived_at IS NULL AND st.stage IN ('saved','active','offer') ORDER BY j.updated_at DESC,j.id LIMIT $2`, u, maxJobOptions)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []jobOption
	for rows.Next() {
		var o jobOption
		var title, company string
		if e = rows.Scan(&o.ID, &title, &company); e != nil {
			return nil, e
		}
		o.Label = title
		if company != "" {
			o.Label += " at " + company
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func categoryQuestion() systemone.Question {
	return systemone.Question{Type: "choice", Instructions: systemone.Raw(map[string]any{
		"question": "Which stage of a job application does `email` belong to? The reader is a job seeker tracking their applications. Judge only the supplied content.",
	}), Criteria: systemone.Raw(map[string]string{
		"received":   "Confirms that an application was received or submitted.",
		"assessment": "Asks the candidate to complete a test, coding challenge, questionnaire, assignment or recorded screening.",
		"interview":  "Invites the candidate to, or schedules or confirms, an interview or a call with a recruiter or hiring team.",
		"rejection":  "Says the candidate was not selected, the position was filled or the process will not continue.",
		"offer":      "Extends a job offer or discusses offer terms.",
		"other":      "Anything else: newsletters, job alerts, marketing, personal mail, receipts, or mail unrelated to the candidate's own application.",
	})}
}

func jobQuestion(jobs []jobOption) systemone.Question {
	criteria := map[string]string{"none": "None of the listed jobs; the email does not concern any of them."}
	for _, j := range jobs {
		criteria[j.ID.String()] = j.Label
	}
	return systemone.Question{Type: "choice", Instructions: systemone.Raw(map[string]any{
		"question": "Which of the candidate's tracked jobs is `email` about? Choose none unless the sender, subject or preview clearly names the company or role.",
	}), Criteria: systemone.Raw(criteria)}
}

// dispatch sends one question about one message through the model gateway.
func (s *Service) dispatch(ctx context.Context, tx pgx.Tx, u uuid.UUID, m pendingMessage, model, stage string, jobs []jobOption) error {
	if s.queue == nil {
		return errors.New("mail queue unavailable")
	}
	q := categoryQuestion()
	if stage == "job" {
		q = jobQuestion(jobs)
	}
	state := systemone.Raw(map[string]any{"email": map[string]string{"from": m.From, "subject": m.Subject, "preview": m.Snippet}})
	p, e := decide.New("q1", state, q)
	if e != nil {
		return e
	}
	packed, e := decide.Pack(model, []decide.Pending{p}, 0, 1)
	if e != nil || len(packed) != 1 {
		return errors.Join(errors.New("mail question too large"), e)
	}
	hash, e := systemone.Hash(packed[0].Request)
	if e != nil {
		return e
	}
	op := uuid.Must(uuid.NewV7())
	if _, e = tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'decision',$3,$4)`, op, u, "mail:"+m.ID.String()+":"+op.String(), hash); e != nil {
		return e
	}
	if e = decide.Store(ctx, tx, u, op, packed[0]); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO mail_operation(user_id,message_id,operation_id,stage) VALUES($1,$2,$3,$4)`, u, m.ID, op, stage); e != nil {
		return e
	}
	return s.queue.InsertTx(ctx, tx, worker.Args{UserID: u, OperationID: op})
}

// finish marks a relevant message classified. Its content stays.
func (s *Service) finish(ctx context.Context, tx pgx.Tx, u uuid.UUID, m pendingMessage) error {
	if m.Category == nil {
		return nil
	}
	if _, e := tx.Exec(ctx, `UPDATE mail_message SET state='classified',classified_at=now(),updated_at=now() WHERE user_id=$1 AND id=$2 AND state='pending'`, u, m.ID); e != nil {
		return e
	}
	if e := s.counted(ctx, tx, u, m.ID); e != nil {
		return e
	}
	if *m.Category == "other" {
		return nil
	}
	return emit(ctx, tx, u, "mail.message.classified", m.ID)
}

// giveUp stops trying to classify a message and drops what it held.
func (s *Service) giveUp(ctx context.Context, tx pgx.Tx, u uuid.UUID, m pendingMessage) error {
	if _, e := tx.Exec(ctx, `UPDATE mail_message SET state='failed',from_address=NULL,subject=NULL,snippet=NULL,updated_at=now() WHERE user_id=$1 AND id=$2 AND state='pending'`, u, m.ID); e != nil {
		return e
	}
	return s.counted(ctx, tx, u, m.ID)
}

// counted tells open pages that a mailbox's classified count moved. Mail judged
// irrelevant has no event of its own, yet it still counts toward the progress.
func (s *Service) counted(ctx context.Context, tx pgx.Tx, u, msg uuid.UUID) error {
	var account uuid.UUID
	if e := tx.QueryRow(ctx, `SELECT account_id FROM mail_message WHERE user_id=$1 AND id=$2`, u, msg).Scan(&account); e != nil {
		return e
	}
	return emit(ctx, tx, u, "mail.account.updated", account)
}

// OnTerminal records the answer to a finished mail request, inside the same
// transaction that finished it. It reports handled=false for operations that
// are not mail requests, which belong to other features.
func (s *Service) OnTerminal(ctx context.Context, tx pgx.Tx, op worker.Operation) (bool, error) {
	if op.Kind != "decision" {
		return false, nil
	}
	var msg uuid.UUID
	var stage string
	var ingested bool
	e := tx.QueryRow(ctx, `SELECT message_id,stage,ingested FROM mail_operation WHERE user_id=$1 AND operation_id=$2 FOR UPDATE`, op.UserID, op.ID).Scan(&msg, &stage, &ingested)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return true, e
	}
	if ingested {
		return true, nil // a replay: the request body is already blanked
	}
	m, e := s.loadMessage(ctx, tx, op.UserID, msg)
	if e != nil {
		return true, e
	}
	var choice string
	if op.Status == "completed" && m.State == "pending" {
		choice, e = s.answer(ctx, tx, op)
		if e != nil {
			return true, e
		}
	}
	ok := false
	switch {
	case choice == "":
	case stage == "category" && m.Category == nil && isCategory(choice):
		ok = true
		if choice == "other" {
			_, e = tx.Exec(ctx, `UPDATE mail_message SET category='other',state='classified',classified_at=now(),from_address=NULL,subject=NULL,snippet=NULL,job_id=NULL,updated_at=now() WHERE user_id=$1 AND id=$2`, op.UserID, msg)
			if e == nil {
				e = s.counted(ctx, tx, op.UserID, msg)
			}
		} else {
			_, e = tx.Exec(ctx, `UPDATE mail_message SET category=$3,updated_at=now() WHERE user_id=$1 AND id=$2`, op.UserID, msg, choice)
		}
	case stage == "job":
		ok = true
		if job, perr := uuid.Parse(choice); perr == nil {
			// The job may have been deleted since the question was asked.
			_, e = tx.Exec(ctx, `UPDATE mail_message SET job_id=$3,updated_at=now() WHERE user_id=$1 AND id=$2 AND EXISTS(SELECT FROM job WHERE user_id=$1 AND id=$3)`, op.UserID, msg, job)
		}
	}
	if e != nil {
		return true, e
	}
	if _, e = tx.Exec(ctx, `UPDATE mail_operation SET ingested=true,ok=$3 WHERE user_id=$1 AND operation_id=$2`, op.UserID, op.ID, ok); e != nil {
		return true, e
	}
	if _, e = tx.Exec(ctx, `SELECT platform.scrub_mail_request($1)`, op.ID); e != nil {
		return true, e
	}
	if s.queue == nil {
		return true, errors.New("mail queue unavailable")
	}
	return true, s.queue.InsertTx(ctx, tx, ClassifyArgs{UserID: op.UserID, MessageID: msg, Wake: op.ID.String()})
}

func isCategory(c string) bool {
	for _, k := range Categories {
		if k == c {
			return true
		}
	}
	return false
}

// answer returns the validated choice in a finished request, or "" when the
// model's answer is missing or does not fit the question that was asked.
func (s *Service) answer(ctx context.Context, tx pgx.Tx, op worker.Operation) (string, error) {
	var body []byte
	if e := tx.QueryRow(ctx, `SELECT body FROM model_request WHERE user_id=$1 AND operation_id=$2`, op.UserID, op.ID).Scan(&body); e != nil {
		return "", e
	}
	var req systemone.Request
	if e := json.Unmarshal(body, &req); e != nil {
		return "", e
	}
	q, ok := req.Questions["q1"]
	if !ok {
		return "", nil
	}
	var answers map[string]json.RawMessage
	if json.Unmarshal([]byte(op.Result), &answers) != nil || len(answers) != 1 {
		return "", nil
	}
	var a systemone.Answer
	if json.Unmarshal(answers["q1"], &a) != nil || systemone.Validate(q, a) != nil {
		return "", nil
	}
	return a.Choice, nil
}
