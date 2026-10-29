package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/diegohh0411/zapply/internal/decide"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	ID     uuid.UUID
	Label  string
	Status string
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

		// The job stage asks which job the email is about and what that job's
		// status should be now, in one request over the same state.
		var (
			stage string
			qs    []systemone.Question
			about map[string]any
		)
		switch {
		case m.Category == nil && attempts["category"] >= categoryAttempts:
			return s.giveUp(ctx, tx, u, m)
		case m.Category == nil:
			stage, qs = "category", []systemone.Question{categoryQuestion()}
		case *m.Category != "other" && !jobDone && attempts["job"] < jobAttempts:
			open, e := s.openJobs(ctx, tx, u)
			if e != nil {
				return e
			}
			all, e := statuses(ctx, tx, u)
			if e != nil {
				return e
			}
			if len(open) > 0 {
				stage, qs = "job", []systemone.Question{jobQuestion(open), statusQuestion(all)}
				jobs := make([]map[string]string, len(open))
				for i, j := range open {
					jobs[i] = map[string]string{"id": j.ID.String(), "job": j.Label, "status": j.Status}
				}
				about = map[string]any{"jobs": jobs}
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
		return s.dispatch(ctx, tx, u, m, *model, stage, qs, about)
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
	rows, e := tx.Query(ctx, `SELECT j.id,j.title,j.company,st.name FROM job j JOIN job_status st ON st.user_id=j.user_id AND st.id=j.status_id
 WHERE j.user_id=$1 AND j.archived_at IS NULL AND st.stage IN ('saved','active','offer') ORDER BY j.updated_at DESC,j.id LIMIT $2`, u, maxJobOptions)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []jobOption
	for rows.Next() {
		var o jobOption
		var title, company string
		if e = rows.Scan(&o.ID, &title, &company, &o.Status); e != nil {
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
		"question": "Which of the candidate's tracked jobs in `jobs` is `email` about? Choose none unless the sender, subject or preview clearly names the company or role.",
	}), Criteria: systemone.Raw(criteria)}
}

// dispatch sends one question about one message through the model gateway.
// dispatch sends a stage's questions about one message through the model
// gateway as a single request; they share one state, the email plus about.
// The answers come back as q1, q2, … in the order of qs.
func (s *Service) dispatch(ctx context.Context, tx pgx.Tx, u uuid.UUID, m pendingMessage, model, stage string, qs []systemone.Question, about map[string]any) error {
	if s.queue == nil {
		return errors.New("mail queue unavailable")
	}
	st := map[string]any{"email": map[string]string{"from": m.From, "subject": m.Subject, "preview": m.Snippet}}
	for k, v := range about {
		st[k] = v
	}
	state := systemone.Raw(st)
	pending := make([]decide.Pending, len(qs))
	for i, q := range qs {
		p, e := decide.New(fmt.Sprintf("q%d", i+1), state, q)
		if e != nil {
			return e
		}
		pending[i] = p
	}
	packed, e := decide.Pack(model, pending, 0, len(qs))
	if e != nil || len(packed) != 1 {
		return errors.Join(errors.New("mail questions too large for one request"), e)
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
	if *m.Category == "other" {
		return nil
	}
	return emit(ctx, tx, u, "mail.message.classified", m.ID)
}

// giveUp stops trying to classify a message and drops what it held.
func (s *Service) giveUp(ctx context.Context, tx pgx.Tx, u uuid.UUID, m pendingMessage) error {
	_, e := tx.Exec(ctx, `UPDATE mail_message SET state='failed',from_address=NULL,subject=NULL,snippet=NULL,updated_at=now() WHERE user_id=$1 AND id=$2 AND state='pending'`, u, m.ID)
	return e
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
	var got []choice
	if op.Status == "completed" && m.State == "pending" {
		if got, e = s.answers(ctx, tx, op); e != nil {
			return true, e
		}
	}
	pick := func(i int) choice {
		if i < len(got) {
			return got[i]
		}
		return choice{}
	}
	ok := false
	switch choice := pick(0).Choice; {
	case choice == "":
	case stage == "category" && m.Category == nil && isCategory(choice):
		ok = true
		if choice == "other" {
			_, e = tx.Exec(ctx, `UPDATE mail_message SET category='other',state='classified',classified_at=now(),from_address=NULL,subject=NULL,snippet=NULL,job_id=NULL,updated_at=now() WHERE user_id=$1 AND id=$2`, op.UserID, msg)
		} else {
			_, e = tx.Exec(ctx, `UPDATE mail_message SET category=$3,updated_at=now() WHERE user_id=$1 AND id=$2`, op.UserID, msg, choice)
		}
	case stage == "job":
		ok = true
		job, perr := uuid.Parse(choice)
		if perr != nil {
			break
		}
		// The job may have been deleted since the question was asked.
		var tag pgconn.CommandTag
		tag, e = tx.Exec(ctx, `UPDATE mail_message SET job_id=$3,updated_at=now() WHERE user_id=$1 AND id=$2 AND EXISTS(SELECT FROM job WHERE user_id=$1 AND id=$3)`, op.UserID, msg, job)
		// An unusable status answer still keeps the link.
		if e == nil && tag.RowsAffected() == 1 {
			if to, perr := uuid.Parse(pick(1).Choice); perr == nil {
				e = s.settleStatus(ctx, tx, op.UserID, msg, job, to, pick(1).P)
			}
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

type choice struct {
	Choice string
	P      float64 // the probability the model gave Choice
}

// answers returns the answers in a finished request in question order (q1,
// q2, …). An answer that is missing or does not fit its question is empty.
func (s *Service) answers(ctx context.Context, tx pgx.Tx, op worker.Operation) ([]choice, error) {
	var body []byte
	if e := tx.QueryRow(ctx, `SELECT body FROM model_request WHERE user_id=$1 AND operation_id=$2`, op.UserID, op.ID).Scan(&body); e != nil {
		return nil, e
	}
	var req systemone.Request
	if e := json.Unmarshal(body, &req); e != nil {
		return nil, e
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(op.Result), &raw) != nil {
		return nil, nil
	}
	out := make([]choice, len(req.Questions))
	for i := range out {
		key := fmt.Sprintf("q%d", i+1)
		q, ok := req.Questions[key]
		var a systemone.Answer
		if !ok || json.Unmarshal(raw[key], &a) != nil || systemone.Validate(q, a) != nil {
			continue
		}
		p, ok := a.Probabilities[a.Choice]
		if !ok && a.Confidence != nil {
			p = *a.Confidence
		}
		out[i] = choice{a.Choice, min(1, max(0, p))}
	}
	return out, nil
}
