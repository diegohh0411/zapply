package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/store/db"
)

// View is a saved way of looking at jobs.
type View struct {
	ID        uuid.UUID
	Name      string
	Layout    string // board, table
	Filters   ViewFilters
	Sort      Sort
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ViewFilters is stored as JSON. Status ids that no longer exist are
// ignored when the view is applied.
type ViewFilters struct {
	StatusIDs       []uuid.UUID `json:"status_ids,omitempty"`
	Stages          []string    `json:"stages,omitempty"`
	Q               string      `json:"q,omitempty"`
	IncludeArchived bool        `json:"include_archived,omitempty"`
}

type ViewInput struct {
	Name    string
	Layout  string // default board
	Filters ViewFilters
	Sort    Sort // default -updated_at
}

const maxViews = 50

var layouts = []string{"board", "table"}

func cleanView(in *ViewInput) error {
	var f fieldErrors
	f.text("name", &in.Name, 100, true)
	if in.Layout == "" {
		in.Layout = "board"
	}
	f.oneOf("layout", in.Layout, layouts)
	if in.Sort == "" {
		in.Sort = SortUpdated
	}
	f.oneOf("sort", string(in.Sort), sorts)
	if len(in.Filters.StatusIDs) > maxFilterIDs {
		f.add("filters.status_ids", fmt.Sprintf("at most %d", maxFilterIDs))
	}
	if len(in.Filters.Stages) > len(stages) {
		f.add("filters.stages", fmt.Sprintf("at most %d", len(stages)))
	} else {
		for i, st := range in.Filters.Stages {
			f.oneOf(fmt.Sprintf("filters.stages[%d]", i), st, stages)
		}
	}
	f.text("filters.q", &in.Filters.Q, maxShortText, false)
	return f.err()
}

func (s *Service) ListViews(ctx context.Context, uid uuid.UUID) ([]View, error) {
	out := []View{}
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		rows, err := q.ListJobViews(ctx, uid)
		if err != nil {
			return err
		}
		for _, r := range rows {
			v, err := viewFromRow(r)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// CreateView appends a view.
func (s *Service) CreateView(ctx context.Context, uid uuid.UUID, in ViewInput) (View, error) {
	if err := cleanView(&in); err != nil {
		return View{}, err
	}
	filters, _ := json.Marshal(in.Filters)
	var out View
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		if err := q.LockJobScope(ctx, uid.String()+"/job_view"); err != nil {
			return err
		}
		rows, err := q.ListJobViews(ctx, uid)
		if err != nil {
			return err
		}
		if len(rows) >= maxViews {
			return invalid("", fmt.Sprintf("at most %d views", maxViews))
		}
		last := ""
		if n := len(rows); n > 0 {
			last = rows[n-1].Position
		}
		pos, err := library.PositionBetween(last, "")
		if err != nil {
			return err
		}
		r, err := q.CreateJobView(ctx, db.CreateJobViewParams{
			ID: newID(), UserID: uid, Name: in.Name, Layout: db.JobViewLayout(in.Layout),
			Filters: filters, Sort: string(in.Sort), Position: pos,
		})
		if err != nil {
			return err
		}
		if out, err = viewFromRow(r); err != nil {
			return err
		}
		return emit(ctx, q, uid, "job_view.created", map[string]any{"id": r.ID})
	})
	return out, err
}

func (s *Service) UpdateView(ctx context.Context, uid, id uuid.UUID, in ViewInput) (View, error) {
	if err := cleanView(&in); err != nil {
		return View{}, err
	}
	filters, _ := json.Marshal(in.Filters)
	var out View
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		r, err := q.UpdateJobView(ctx, db.UpdateJobViewParams{
			UserID: uid, ID: id, Name: in.Name, Layout: db.JobViewLayout(in.Layout),
			Filters: filters, Sort: string(in.Sort),
		})
		if err != nil {
			return notFound(err)
		}
		if out, err = viewFromRow(r); err != nil {
			return err
		}
		return emit(ctx, q, uid, "job_view.updated", map[string]any{"id": id})
	})
	return out, err
}

func (s *Service) DeleteView(ctx context.Context, uid, id uuid.UUID) error {
	return s.tx(ctx, uid, func(q *db.Queries) error {
		n, err := q.DeleteJobView(ctx, db.DeleteJobViewParams{UserID: uid, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return library.ErrNotFound
		}
		return emit(ctx, q, uid, "job_view.deleted", map[string]any{"id": id})
	})
}

func viewFromRow(r db.JobView) (View, error) {
	v := View{ID: r.ID, Name: r.Name, Layout: string(r.Layout), Sort: Sort(r.Sort), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	return v, json.Unmarshal(r.Filters, &v.Filters)
}
