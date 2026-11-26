package mail

import (
	"context"
	"errors"
	"strings"

	"github.com/diegohh0411/zapply/internal/jobs"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AutoApplyThreshold is the probability at or above which a proposed status
// that moves a job forward is applied without asking. CVD uses the same bar.
const AutoApplyThreshold = 0.85

type statusOption struct {
	ID       uuid.UUID
	Name     string
	Stage    string
	Position string
}

// stageRank orders the board's stages; a job only moves forward on its own.
var stageRank = map[string]int{"saved": 0, "active": 1, "offer": 2, "closed": 3}

// forward reports whether to comes after from on the board: a later stage, or
// a later column within the same stage.
func forward(from, to statusOption) bool {
	if a, b := stageRank[from.Stage], stageRank[to.Stage]; a != b {
		return b > a
	}
	return to.Position > from.Position
}

// decideStatus turns the model's proposal into an outcome. Moving backward, or
// a proposal below the threshold, waits for the member.
func decideStatus(current, proposed statusOption, p float64) string {
	switch {
	case proposed.ID == current.ID:
		return "no_change"
	case p >= AutoApplyThreshold && forward(current, proposed):
		return "applied"
	default:
		return "review"
	}
}

func statuses(ctx context.Context, tx pgx.Tx, u uuid.UUID) ([]statusOption, error) {
	rows, e := tx.Query(ctx, `SELECT id,name,stage::text,position FROM job_status WHERE user_id=$1 ORDER BY position,id`, u)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []statusOption
	for rows.Next() {
		var o statusOption
		if e = rows.Scan(&o.ID, &o.Name, &o.Stage, &o.Position); e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// statusHint explains a status to the model. Statuses are the member's own, so
// well-known names get a specific hint and the rest one from their stage.
func statusHint(o statusOption) string {
	switch strings.ToLower(strings.TrimSpace(o.Name)) {
	case "bookmarked", "saved":
		return "Saved, but the candidate has not applied yet."
	case "applied":
		return "The candidate submitted an application: it was confirmed as sent, received or under review."
	case "interview", "interviewing":
		return "The candidate is invited to, or scheduled for, an interview, call or assessment."
	case "offer":
		return "The candidate received a job offer."
	case "rejected":
		return "The company turned the candidate down or closed the application without an offer."
	case "dropped", "withdrawn":
		return "The candidate withdrew from the process."
	case "posting closed":
		return "The posting was closed or filled before a decision about the candidate."
	}
	switch o.Stage {
	case "saved":
		return "Not applied yet."
	case "active":
		return "The application is in progress."
	case "offer":
		return "An offer was made."
	default:
		return "The process has ended."
	}
}

func statusQuestion(all []statusOption) systemone.Question {
	criteria := map[string]string{}
	for _, o := range all {
		criteria[o.ID.String()] = o.Name + ": " + statusHint(o)
	}
	return systemone.Question{Type: "choice", Instructions: systemone.Raw(map[string]any{
		"question": "What should the status of the job `email` is about be after `email`? Each job in `jobs` lists its current `status`. Keep the current status unless the email clearly shows the application moved on, for example a confirmation that it was received, an interview invitation, an offer or a rejection.",
	}), Criteria: systemone.Raw(criteria)}
}

// settleStatus records the model's proposal for a linked message and applies
// it when it is confident and moves the job forward.
func (s *Service) settleStatus(ctx context.Context, tx pgx.Tx, u, msg, job, proposed uuid.UUID, p float64) error {
	all, e := statuses(ctx, tx, u)
	if e != nil {
		return e
	}
	var current uuid.UUID
	if e = tx.QueryRow(ctx, `SELECT status_id FROM job WHERE user_id=$1 AND id=$2`, u, job).Scan(&current); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return nil // the job was deleted while the question was open
		}
		return e
	}
	var from, to *statusOption
	for i := range all {
		if all[i].ID == current {
			from = &all[i]
		}
		if all[i].ID == proposed {
			to = &all[i]
		}
	}
	if from == nil || to == nil {
		return nil // a status was deleted while the question was open
	}
	outcome := decideStatus(*from, *to, p)
	var previous *uuid.UUID
	if outcome == "applied" {
		moved, e := jobs.MoveToStatus(ctx, tx, u, job, proposed, &current)
		if e != nil {
			return e
		}
		if moved {
			previous = &current
		} else {
			outcome = "review"
		}
	}
	_, e = tx.Exec(ctx, `UPDATE mail_message SET status_outcome=$3,suggested_status_id=$4,previous_status_id=$5,status_confidence=$6,status_auto=$7,updated_at=now() WHERE user_id=$1 AND id=$2`,
		u, msg, outcome, proposed, previous, p, outcome == "applied")
	return e
}

// Resolve acts on a message's status proposal: "apply" moves the job to the
// proposed status, "dismiss" declines it, and "undo" moves an applied job back
// while it still sits in the status the message moved it to.
func (s *Service) Resolve(ctx context.Context, u, id uuid.UUID, action string) (Item, error) {
	var out Item
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var outcome *string
		var job, suggested, previous *uuid.UUID
		e := tx.QueryRow(ctx, `SELECT status_outcome,job_id,suggested_status_id,previous_status_id FROM mail_message WHERE user_id=$1 AND id=$2 AND state='classified' AND category<>'other' FOR UPDATE`, u, id).
			Scan(&outcome, &job, &suggested, &previous)
		if errors.Is(e, pgx.ErrNoRows) {
			return library.ErrNotFound
		}
		if e != nil {
			return e
		}
		want := map[string]string{"apply": "review", "dismiss": "review", "undo": "applied"}[action]
		if want == "" {
			return &library.ValidationError{Fields: []library.FieldError{{Field: "action", Message: "must be apply, dismiss or undo"}}}
		}
		if outcome == nil || *outcome != want {
			return &library.ConflictError{Message: "this message has no status change to " + action}
		}
		next, prev := "dismissed", previous
		switch action {
		case "apply":
			if job == nil || suggested == nil {
				return &library.ConflictError{Message: "the job or the proposed status no longer exists"}
			}
			var current uuid.UUID
			if e = tx.QueryRow(ctx, `SELECT status_id FROM job WHERE user_id=$1 AND id=$2`, u, *job).Scan(&current); e != nil {
				return e
			}
			if _, e = jobs.MoveToStatus(ctx, tx, u, *job, *suggested, nil); e != nil {
				return e
			}
			next, prev = "applied", &current
		case "undo":
			if job == nil || suggested == nil || previous == nil {
				return &library.ConflictError{Message: "the job or its earlier status no longer exists"}
			}
			moved, e := jobs.MoveToStatus(ctx, tx, u, *job, *previous, suggested)
			if e != nil {
				return e
			}
			if !moved {
				return &library.ConflictError{Message: "the job's status was changed after this update"}
			}
			next = "undone"
		}
		if _, e = tx.Exec(ctx, `UPDATE mail_message SET status_outcome=$3,previous_status_id=$4,updated_at=now() WHERE user_id=$1 AND id=$2`, u, id, next, prev); e != nil {
			return e
		}
		if e = emit(ctx, tx, u, "mail.message.classified", id); e != nil {
			return e
		}
		out, e = s.item(ctx, tx, u, id)
		return e
	})
	return out, err
}

// Relink sets the job a message is about, or clears it with nil. The member's
// choice is final: the status proposal made for the old job is dropped, and the
// classifier will not change the link again. A move already applied to the old
// job stays; undo it first if it was wrong.
func (s *Service) Relink(ctx context.Context, u, id uuid.UUID, job *uuid.UUID) (Item, error) {
	var out Item
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var current *uuid.UUID
		e := tx.QueryRow(ctx, `SELECT job_id FROM mail_message WHERE user_id=$1 AND id=$2 AND state='classified' AND category<>'other' FOR UPDATE`, u, id).Scan(&current)
		if errors.Is(e, pgx.ErrNoRows) {
			return library.ErrNotFound
		}
		if e != nil {
			return e
		}
		if job != nil {
			var ok bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM job WHERE user_id=$1 AND id=$2)`, u, *job).Scan(&ok); e != nil {
				return e
			}
			if !ok {
				return &library.ValidationError{Fields: []library.FieldError{{Field: "job_id", Message: "no such job"}}}
			}
		}
		same := (current == nil && job == nil) || (current != nil && job != nil && *current == *job)
		if same {
			_, e = tx.Exec(ctx, `UPDATE mail_message SET job_manual=true,updated_at=now() WHERE user_id=$1 AND id=$2`, u, id)
		} else {
			_, e = tx.Exec(ctx, `UPDATE mail_message SET job_id=$3,job_manual=true,status_outcome=NULL,suggested_status_id=NULL,previous_status_id=NULL,status_confidence=NULL,status_auto=false,updated_at=now() WHERE user_id=$1 AND id=$2`, u, id, job)
		}
		if e != nil {
			return e
		}
		if e = emit(ctx, tx, u, "mail.message.classified", id); e != nil {
			return e
		}
		out, e = s.item(ctx, tx, u, id)
		return e
	})
	return out, err
}
