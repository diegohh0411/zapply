package jobs

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/store/db"
)

// Activity is one entry of a job's timeline. Only notes are writable.
type Activity struct {
	ID         uuid.UUID
	JobID      uuid.UUID
	Type       string // created, status_changed, note
	FromStatus string
	ToStatus   string
	Body       string
	OccurredAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type NoteInput struct {
	Body       string
	OccurredAt *time.Time // default now
}

func cleanNote(in *NoteInput) error {
	var f fieldErrors
	f.text("body", &in.Body, maxNotes, true)
	return f.err()
}

func addActivity(ctx context.Context, q *db.Queries, uid, jobID uuid.UUID, typ db.JobActivityType, from, to, body string) error {
	a, err := q.InsertJobActivity(ctx, db.InsertJobActivityParams{
		ID: newID(), UserID: uid, JobID: jobID, Type: typ, FromStatus: from, ToStatus: to,
		Body: body, OccurredAt: time.Now(),
	})
	if err != nil {
		return err
	}
	return emit(ctx, q, uid, "job_activity.created", map[string]any{"id": a.ID, "job_id": jobID})
}

// ListActivities returns a job's timeline, newest first.
func (s *Service) ListActivities(ctx context.Context, uid, jobID uuid.UUID) ([]Activity, error) {
	out := []Activity{}
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		if _, err := q.GetJob(ctx, db.GetJobParams{UserID: uid, ID: jobID}); err != nil {
			return notFound(err)
		}
		rows, err := q.ListJobActivities(ctx, db.ListJobActivitiesParams{UserID: uid, JobID: jobID})
		for _, r := range rows {
			out = append(out, activityFromRow(r))
		}
		return err
	})
	return out, err
}

func (s *Service) AddNote(ctx context.Context, uid, jobID uuid.UUID, in NoteInput) (Activity, error) {
	if err := cleanNote(&in); err != nil {
		return Activity{}, err
	}
	at := time.Now()
	if in.OccurredAt != nil {
		at = *in.OccurredAt
	}
	var out Activity
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		if _, err := q.GetJob(ctx, db.GetJobParams{UserID: uid, ID: jobID}); err != nil {
			return notFound(err)
		}
		r, err := q.InsertJobActivity(ctx, db.InsertJobActivityParams{
			ID: newID(), UserID: uid, JobID: jobID, Type: db.JobActivityTypeNote, Body: in.Body, OccurredAt: at,
		})
		if err != nil {
			return err
		}
		out = activityFromRow(r)
		return emit(ctx, q, uid, "job_activity.created", map[string]any{"id": r.ID, "job_id": jobID})
	})
	return out, err
}

func (s *Service) UpdateNote(ctx context.Context, uid, jobID, id uuid.UUID, in NoteInput) (Activity, error) {
	if err := cleanNote(&in); err != nil {
		return Activity{}, err
	}
	var out Activity
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		cur, err := q.GetJobActivity(ctx, db.GetJobActivityParams{UserID: uid, JobID: jobID, ID: id})
		if err != nil {
			return notFound(err)
		}
		if cur.Type != db.JobActivityTypeNote {
			return &library.ConflictError{Message: "only notes can be edited"}
		}
		at := cur.OccurredAt
		if in.OccurredAt != nil {
			at = *in.OccurredAt
		}
		r, err := q.UpdateJobActivity(ctx, db.UpdateJobActivityParams{UserID: uid, JobID: jobID, ID: id, Body: in.Body, OccurredAt: at})
		if err != nil {
			return notFound(err)
		}
		out = activityFromRow(r)
		return emit(ctx, q, uid, "job_activity.updated", map[string]any{"id": id, "job_id": jobID})
	})
	return out, err
}

func (s *Service) DeleteNote(ctx context.Context, uid, jobID, id uuid.UUID) error {
	return s.tx(ctx, uid, func(q *db.Queries) error {
		cur, err := q.GetJobActivity(ctx, db.GetJobActivityParams{UserID: uid, JobID: jobID, ID: id})
		if err != nil {
			return notFound(err)
		}
		if cur.Type != db.JobActivityTypeNote {
			return &library.ConflictError{Message: "only notes can be deleted"}
		}
		if _, err := q.DeleteJobActivity(ctx, db.DeleteJobActivityParams{UserID: uid, JobID: jobID, ID: id}); err != nil {
			return err
		}
		return emit(ctx, q, uid, "job_activity.deleted", map[string]any{"id": id, "job_id": jobID})
	})
}

func activityFromRow(r db.JobActivity) Activity {
	return Activity{
		ID: r.ID, JobID: r.JobID, Type: string(r.Type), FromStatus: r.FromStatus, ToStatus: r.ToStatus,
		Body: r.Body, OccurredAt: r.OccurredAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
