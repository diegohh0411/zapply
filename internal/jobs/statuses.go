package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/store/db"
)

type Stage string

const (
	StageSaved  Stage = "saved"
	StageActive Stage = "active"
	StageOffer  Stage = "offer"
	StageClosed Stage = "closed"
)

var (
	stages = []string{"saved", "active", "offer", "closed"}
	colors = []string{"gray", "blue", "yellow", "orange", "green", "purple", "pink", "red", "brown"}
)

// countsAsApplied reports whether entering the stage means the user applied.
func (s Stage) countsAsApplied() bool { return s == StageActive || s == StageOffer }

type Status struct {
	ID        uuid.UUID
	Name      string
	Color     string
	Stage     Stage
	CreatedAt time.Time
	UpdatedAt time.Time
}

type StatusInput struct {
	Name  string
	Color string // default gray
	Stage Stage
}

// defaultStatuses follow cvd's board.
var defaultStatuses = []StatusInput{
	{"Bookmarked", "blue", StageSaved},
	{"Applied", "yellow", StageActive},
	{"Interviewing", "purple", StageActive},
	{"Offer", "green", StageOffer},
	{"Dropped", "pink", StageClosed},
	{"Rejected", "red", StageClosed},
	{"Posting closed", "brown", StageClosed},
}

const maxStatuses = 50

func cleanStatus(in *StatusInput) error {
	var f fieldErrors
	f.text("name", &in.Name, 100, true)
	if in.Color == "" {
		in.Color = "gray"
	}
	f.oneOf("color", in.Color, colors)
	if in.Stage == "" {
		f.add("stage", "is required")
	}
	f.oneOf("stage", string(in.Stage), stages)
	return f.err()
}

// lockStatuses serializes seeding and ordering of one user's statuses.
func lockStatuses(ctx context.Context, q *db.Queries, uid uuid.UUID) error {
	return q.LockJobScope(ctx, uid.String()+"/job_status")
}

// ensureStatuses returns the user's statuses in order, seeding the defaults
// on first use. A user can't delete their last status, so an empty list
// means they were never seeded. Seeding emits no events: whoever reads the
// statuses next seeds or sees the same ones.
func ensureStatuses(ctx context.Context, q *db.Queries, uid uuid.UUID) ([]db.JobStatus, error) {
	rows, err := q.ListJobStatuses(ctx, uid)
	if err != nil || len(rows) > 0 {
		return rows, err
	}
	if err := lockStatuses(ctx, q, uid); err != nil {
		return nil, err
	}
	if rows, err = q.ListJobStatuses(ctx, uid); err != nil || len(rows) > 0 {
		return rows, err
	}
	keys := library.PositionsAfter("", len(defaultStatuses))
	for i, d := range defaultStatuses {
		r, err := q.CreateJobStatus(ctx, db.CreateJobStatusParams{
			ID: newID(), UserID: uid, Name: d.Name, NormalizedName: library.NormalizeName(d.Name),
			Color: db.JobStatusColor(d.Color), Stage: db.JobStage(d.Stage), Position: keys[i],
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func (s *Service) ListStatuses(ctx context.Context, uid uuid.UUID) ([]Status, error) {
	var out []Status
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		rows, err := ensureStatuses(ctx, q, uid)
		for _, r := range rows {
			out = append(out, statusFromRow(r))
		}
		return err
	})
	return out, err
}

// CreateStatus appends a status as the last column.
func (s *Service) CreateStatus(ctx context.Context, uid uuid.UUID, in StatusInput) (Status, error) {
	if err := cleanStatus(&in); err != nil {
		return Status{}, err
	}
	var out Status
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		if _, err := ensureStatuses(ctx, q, uid); err != nil {
			return err
		}
		if err := lockStatuses(ctx, q, uid); err != nil {
			return err
		}
		rows, err := q.ListJobStatuses(ctx, uid)
		if err != nil {
			return err
		}
		if len(rows) >= maxStatuses {
			return invalid("", fmt.Sprintf("at most %d statuses", maxStatuses))
		}
		pos, err := library.PositionBetween(rows[len(rows)-1].Position, "")
		if err != nil {
			return err
		}
		r, err := q.CreateJobStatus(ctx, db.CreateJobStatusParams{
			ID: newID(), UserID: uid, Name: in.Name, NormalizedName: library.NormalizeName(in.Name),
			Color: db.JobStatusColor(in.Color), Stage: db.JobStage(in.Stage), Position: pos,
		})
		if err != nil {
			return statusConflict(err, in.Name)
		}
		out = statusFromRow(r)
		return emit(ctx, q, uid, "job_status.created", map[string]any{"id": r.ID})
	})
	return out, err
}

// UpdateStatus replaces a status's name, color and stage. Activity history
// keeps the names it recorded.
func (s *Service) UpdateStatus(ctx context.Context, uid, id uuid.UUID, in StatusInput) (Status, error) {
	if err := cleanStatus(&in); err != nil {
		return Status{}, err
	}
	var out Status
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		r, err := q.UpdateJobStatus(ctx, db.UpdateJobStatusParams{
			UserID: uid, ID: id, Name: in.Name, NormalizedName: library.NormalizeName(in.Name),
			Color: db.JobStatusColor(in.Color), Stage: db.JobStage(in.Stage),
		})
		if err != nil {
			return statusConflict(notFound(err), in.Name)
		}
		out = statusFromRow(r)
		return emit(ctx, q, uid, "job_status.updated", map[string]any{"id": id})
	})
	return out, err
}

// MoveInput places an item after AfterID or before BeforeID among its
// siblings; with neither, a status goes last and a job goes first.
type MoveInput struct {
	AfterID  *uuid.UUID
	BeforeID *uuid.UUID
}

func (s *Service) MoveStatus(ctx context.Context, uid, id uuid.UUID, m MoveInput) (Status, error) {
	if m.AfterID != nil && m.BeforeID != nil {
		return Status{}, invalid("after_id", "set after_id or before_id, not both")
	}
	var out Status
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		if err := lockStatuses(ctx, q, uid); err != nil {
			return err
		}
		rows, err := q.ListJobStatuses(ctx, uid)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(rows, func(r db.JobStatus) bool { return r.ID == id }) {
			return library.ErrNotFound
		}
		sibs := make([]positioned, 0, len(rows))
		for _, r := range rows {
			if r.ID != id {
				sibs = append(sibs, positioned{r.ID, r.Position})
			}
		}
		pos, err := placeAmong(sibs, m, true)
		if err != nil {
			return err
		}
		r, err := q.SetJobStatusPosition(ctx, db.SetJobStatusPositionParams{UserID: uid, ID: id, Position: pos})
		if err != nil {
			return err
		}
		out = statusFromRow(r)
		return emit(ctx, q, uid, "job_status.moved", map[string]any{"id": id})
	})
	return out, err
}

// DeleteStatus deletes a status. Jobs in it block the delete unless moveTo
// names another status; they move there first, each with an activity.
func (s *Service) DeleteStatus(ctx context.Context, uid, id uuid.UUID, moveTo *uuid.UUID) error {
	return s.tx(ctx, uid, func(q *db.Queries) error {
		if err := lockStatuses(ctx, q, uid); err != nil {
			return err
		}
		from, err := q.GetJobStatusForUpdate(ctx, db.GetJobStatusForUpdateParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		rows, err := q.ListJobStatuses(ctx, uid)
		if err != nil {
			return err
		}
		if len(rows) == 1 {
			return &library.ConflictError{Message: "a board needs at least one status"}
		}
		col, err := q.ListColumn(ctx, db.ListColumnParams{UserID: uid, StatusID: id})
		if err != nil {
			return err
		}
		if len(col) > 0 {
			if moveTo == nil {
				return &library.ConflictError{Message: fmt.Sprintf("%d jobs have this status; pass move_to to move them first", len(col))}
			}
			if *moveTo == id {
				return invalid("move_to", "must be another status")
			}
			to, err := q.GetJobStatusForUpdate(ctx, db.GetJobStatusForUpdateParams{UserID: uid, ID: *moveTo})
			if errors.Is(err, pgx.ErrNoRows) {
				return invalid("move_to", "status not found")
			}
			if err != nil {
				return err
			}
			dest, err := q.ListColumn(ctx, db.ListColumnParams{UserID: uid, StatusID: to.ID})
			if err != nil {
				return err
			}
			last := ""
			if n := len(dest); n > 0 {
				last = dest[n-1].Position
			}
			keys := library.PositionsAfter(last, len(col))
			for i, c := range col {
				job, err := q.GetJobForUpdate(ctx, db.GetJobForUpdateParams{UserID: uid, ID: c.ID})
				// Membership may change while we wait for the destination lock.
				// A move out only locks its destination, not this source status.
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if job.StatusID != id {
					continue
				}
				if _, err := changeStatus(ctx, q, uid, job, from, to, keys[i]); err != nil {
					return err
				}
			}
		}
		if _, err := q.DeleteJobStatus(ctx, db.DeleteJobStatusParams{UserID: uid, ID: id}); err != nil {
			return err
		}
		return emit(ctx, q, uid, "job_status.deleted", map[string]any{"id": id})
	})
}

func statusConflict(err error, name string) error {
	var c *library.ConflictError
	if errors.As(translate(err), &c) {
		return &library.ConflictError{Message: fmt.Sprintf("a status named %q already exists", name)}
	}
	return err
}

func statusFromRow(r db.JobStatus) Status {
	return Status{
		ID: r.ID, Name: r.Name, Color: string(r.Color), Stage: Stage(r.Stage),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

type positioned struct {
	ID       uuid.UUID
	Position string
}

// placeAmong returns a key for an item placed among its siblings (which
// exclude it). With no anchor it goes last if defaultLast, else first.
// Allocation is serialized per scope, so siblings never share a key.
func placeAmong(sibs []positioned, m MoveInput, defaultLast bool) (string, error) {
	idx := func(id uuid.UUID) int {
		return slices.IndexFunc(sibs, func(p positioned) bool { return p.ID == id })
	}
	var a, b string
	switch {
	case m.AfterID != nil:
		i := idx(*m.AfterID)
		if i < 0 {
			return "", invalid("after_id", "is not a sibling at the destination")
		}
		a = sibs[i].Position
		if i+1 < len(sibs) {
			b = sibs[i+1].Position
		}
	case m.BeforeID != nil:
		i := idx(*m.BeforeID)
		if i < 0 {
			return "", invalid("before_id", "is not a sibling at the destination")
		}
		b = sibs[i].Position
		if i > 0 {
			a = sibs[i-1].Position
		}
	case defaultLast:
		if n := len(sibs); n > 0 {
			a = sibs[n-1].Position
		}
	default:
		if len(sibs) > 0 {
			b = sibs[0].Position
		}
	}
	return library.PositionBetween(a, b)
}
