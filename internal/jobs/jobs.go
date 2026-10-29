package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/joburl"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/store/db"
)

// Job is a posting and the user's application to it. Lists leave
// Description and Notes empty.
type Job struct {
	ID              uuid.UUID
	StatusID        uuid.UUID
	Title           string
	Company         string
	URL             string
	Location        string
	WorkMode        string
	EmploymentType  string
	SalaryMin       *int64
	SalaryMax       *int64
	SalaryCurrency  string
	SalaryPeriod    string
	Source          string
	Description     string
	Notes           string
	AppliedOn       *time.Time // a date
	Position        string
	Version         int
	ContentHash     []byte
	StatusChangedAt time.Time
	ArchivedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type JobInput struct {
	StatusID       *uuid.UUID // create: default first status; update: keep
	Title          string
	Company        string
	URL            string
	Location       string
	WorkMode       string
	EmploymentType string
	SalaryMin      *int64
	SalaryMax      *int64
	SalaryCurrency string
	SalaryPeriod   string
	Source         string
	Description    string
	Notes          string
	AppliedOn      *time.Time
}

const (
	maxShortText   = 300
	maxURL         = 2000
	maxDescription = 100_000
	maxNotes       = 20_000
)

var (
	workModes       = []string{"onsite", "remote", "hybrid"}
	employmentTypes = []string{"full_time", "part_time", "contract", "temporary", "internship", "freelance"}
	salaryPeriods   = []string{"year", "month", "hour"}
)

// cleanJob validates and normalizes in, returning the normalized URL.
func cleanJob(in *JobInput) (string, error) {
	var f fieldErrors
	f.text("title", &in.Title, maxShortText, true)
	f.text("company", &in.Company, maxShortText, false)
	f.text("location", &in.Location, maxShortText, false)
	f.text("source", &in.Source, maxShortText, false)
	f.text("url", &in.URL, maxURL, false)
	f.text("description", &in.Description, maxDescription, false)
	f.text("notes", &in.Notes, maxNotes, false)
	f.oneOf("work_mode", in.WorkMode, workModes)
	f.oneOf("employment_type", in.EmploymentType, employmentTypes)
	f.oneOf("salary_period", in.SalaryPeriod, salaryPeriods)

	var norm string
	if in.URL != "" {
		var err error
		if norm, err = NormalizeURL(in.URL); err != nil {
			f.add("url", "must be an http(s) URL")
		}
	}
	in.SalaryCurrency = strings.ToUpper(strings.TrimSpace(in.SalaryCurrency))
	if in.SalaryCurrency != "" && !isCurrency(in.SalaryCurrency) {
		f.add("salary_currency", "must be a three-letter ISO 4217 code")
	}
	for _, s := range []struct {
		name string
		v    *int64
	}{{"salary_min", in.SalaryMin}, {"salary_max", in.SalaryMax}} {
		if s.v != nil && *s.v < 0 {
			f.add(s.name, "must not be negative")
		}
	}
	if in.SalaryMin != nil && in.SalaryMax != nil && *in.SalaryMin > *in.SalaryMax {
		f.add("salary_max", "must be at least salary_min")
	}
	if (in.SalaryMin != nil || in.SalaryMax != nil) && in.SalaryCurrency == "" {
		f.add("salary_currency", "is required with a salary")
	}
	if in.AppliedOn != nil {
		d := in.AppliedOn.UTC().Truncate(24 * time.Hour)
		in.AppliedOn = &d
	}
	return norm, f.err()
}

func isCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := range 3 {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// NormalizeURL is the duplicate key for a posting URL.
func NormalizeURL(raw string) (string, error) { return joburl.Normalize(raw) }

// jobHash covers what matching reads (part 6 caches scores by it).
func jobHash(title, company, description string) []byte {
	raw, _ := json.Marshal(struct {
		Title       string `json:"title"`
		Company     string `json:"company"`
		Description string `json:"description"`
	}{title, company, description})
	h := sha256.Sum256(raw)
	return h[:]
}

func today() *time.Time {
	d := time.Now().UTC().Truncate(24 * time.Hour)
	return &d
}

// CreateJob saves a job first in its status's column.
func (s *Service) CreateJob(ctx context.Context, uid uuid.UUID, in JobInput) (Job, error) {
	norm, err := cleanJob(&in)
	if err != nil {
		return Job{}, err
	}
	var out Job
	err = s.store.InTenant(ctx, uid, func(tx pgx.Tx) error {
		q := db.New(tx)
		statuses, err := ensureStatuses(ctx, q, uid)
		if err != nil {
			return err
		}
		sid := statuses[0].ID
		if in.StatusID != nil {
			sid = *in.StatusID
		}
		st, err := q.GetJobStatusForUpdate(ctx, db.GetJobStatusForUpdateParams{UserID: uid, ID: sid})
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("status_id", "status not found")
		}
		if err != nil {
			return err
		}
		if err := checkDuplicate(ctx, q, uid, norm, uuid.Nil); err != nil {
			return err
		}
		col, err := q.ListColumn(ctx, db.ListColumnParams{UserID: uid, StatusID: sid})
		if err != nil {
			return err
		}
		first := ""
		if len(col) > 0 {
			first = col[0].Position
		}
		pos, err := library.PositionBetween("", first)
		if err != nil {
			return err
		}
		applied := in.AppliedOn
		if applied == nil && Stage(st.Stage).countsAsApplied() {
			applied = today()
		}
		row, err := q.CreateJob(ctx, db.CreateJobParams{
			ID: newID(), UserID: uid, StatusID: sid, Title: in.Title, Company: in.Company,
			Url: in.URL, NormalizedUrl: norm, Location: in.Location,
			WorkMode: ptrIf[db.WorkMode](in.WorkMode), EmploymentType: ptrIf[db.EmploymentType](in.EmploymentType),
			SalaryMin: in.SalaryMin, SalaryMax: in.SalaryMax, SalaryCurrency: in.SalaryCurrency,
			SalaryPeriod: ptrIf[db.SalaryPeriod](in.SalaryPeriod), Source: in.Source,
			Description: in.Description, Notes: in.Notes, AppliedOn: applied, Position: pos,
			ContentHash: jobHash(in.Title, in.Company, in.Description),
		})
		if err != nil {
			return err
		}
		if err := addActivity(ctx, q, uid, row.ID, db.JobActivityTypeCreated, "", st.Name, ""); err != nil {
			return err
		}
		if s.Matcher != nil {
			if err := s.Matcher.JobCreated(ctx, tx, uid, row); err != nil {
				return err
			}
		}
		out = jobFromRow(row)
		return emit(ctx, q, uid, "job.created", map[string]any{"id": row.ID, "version": row.Version})
	})
	err = translate(err)
	var c *library.ConflictError
	if errors.As(err, &c) && norm != "" {
		// Lost a race with a concurrent save of the same posting.
		return Job{}, s.duplicateOf(ctx, uid, norm, err)
	}
	return out, err
}

func checkDuplicate(ctx context.Context, q *db.Queries, uid uuid.UUID, norm string, self uuid.UUID) error {
	if norm == "" {
		return nil
	}
	id, err := q.GetJobIDByURL(ctx, db.GetJobIDByURLParams{UserID: uid, NormalizedUrl: norm})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && id == self) {
		return nil
	}
	if err != nil {
		return err
	}
	return &DuplicateError{ExistingID: id}
}

func (s *Service) duplicateOf(ctx context.Context, uid uuid.UUID, norm string, orig error) error {
	var dup error = orig
	_ = s.tx(ctx, uid, func(q *db.Queries) error {
		if err := checkDuplicate(ctx, q, uid, norm, uuid.Nil); err != nil {
			dup = err
		}
		return nil
	})
	return dup
}

func (s *Service) GetJob(ctx context.Context, uid, id uuid.UUID) (Job, error) {
	var out Job
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		row, err := q.GetJob(ctx, db.GetJobParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		out = jobFromRow(row)
		return nil
	})
	return out, err
}

// UpdateJob replaces a job if its version is still ifVersion. A new
// StatusID moves it to the top of that column.
func (s *Service) UpdateJob(ctx context.Context, uid, id uuid.UUID, ifVersion int, in JobInput) (Job, error) {
	norm, err := cleanJob(&in)
	if err != nil {
		return Job{}, err
	}
	var out Job
	err = s.store.InTenant(ctx, uid, func(tx pgx.Tx) error {
		q := db.New(tx)
		// Lock order everywhere: status rows, then job rows.
		var to *db.JobStatus
		if in.StatusID != nil {
			st, err := q.GetJobStatusForUpdate(ctx, db.GetJobStatusForUpdateParams{UserID: uid, ID: *in.StatusID})
			if errors.Is(err, pgx.ErrNoRows) {
				return invalid("status_id", "status not found")
			}
			if err != nil {
				return err
			}
			to = &st
		}
		cur, err := q.GetJobForUpdate(ctx, db.GetJobForUpdateParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		if int(cur.Version) != ifVersion {
			return library.ErrVersionMismatch
		}
		if err := checkDuplicate(ctx, q, uid, norm, id); err != nil {
			return err
		}
		row, err := q.UpdateJob(ctx, db.UpdateJobParams{
			UserID: uid, ID: id, Title: in.Title, Company: in.Company,
			Url: in.URL, NormalizedUrl: norm, Location: in.Location,
			WorkMode: ptrIf[db.WorkMode](in.WorkMode), EmploymentType: ptrIf[db.EmploymentType](in.EmploymentType),
			SalaryMin: in.SalaryMin, SalaryMax: in.SalaryMax, SalaryCurrency: in.SalaryCurrency,
			SalaryPeriod: ptrIf[db.SalaryPeriod](in.SalaryPeriod), Source: in.Source,
			Description: in.Description, Notes: in.Notes, AppliedOn: in.AppliedOn,
			ContentHash: jobHash(in.Title, in.Company, in.Description),
		})
		if err != nil {
			return err
		}
		if err := emit(ctx, q, uid, "job.updated", map[string]any{"id": id, "version": row.Version}); err != nil {
			return err
		}
		if to != nil && to.ID != row.StatusID {
			from, err := statusByID(ctx, q, uid, row.StatusID)
			if err != nil {
				return err
			}
			pos, err := topOf(ctx, q, uid, to.ID, id)
			if err != nil {
				return err
			}
			if row, err = changeStatus(ctx, q, uid, row, from, *to, pos); err != nil {
				return err
			}
		}
		out = jobFromRow(row)
		return nil
	})
	err = translate(err)
	var c *library.ConflictError
	if errors.As(err, &c) && norm != "" {
		return Job{}, s.duplicateOf(ctx, uid, norm, err)
	}
	return out, err
}

// JobMove is the board's drag and drop: into StatusID (nil: same column),
// after AfterID or before BeforeID, or first.
type JobMove struct {
	StatusID *uuid.UUID
	MoveInput
}

func (s *Service) MoveJob(ctx context.Context, uid, id uuid.UUID, m JobMove) (Job, error) {
	if m.AfterID != nil && m.BeforeID != nil {
		return Job{}, invalid("after_id", "set after_id or before_id, not both")
	}
	var out Job
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		peek, err := q.GetJob(ctx, db.GetJobParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		dest := peek.StatusID
		if m.StatusID != nil {
			dest = *m.StatusID
		}
		to, err := q.GetJobStatusForUpdate(ctx, db.GetJobStatusForUpdateParams{UserID: uid, ID: dest})
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("status_id", "status not found")
		}
		if err != nil {
			return err
		}
		cur, err := q.GetJobForUpdate(ctx, db.GetJobForUpdateParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		if m.StatusID == nil && cur.StatusID != dest {
			return &library.ConflictError{Message: "the job changed status concurrently; retry"}
		}
		col, err := q.ListColumn(ctx, db.ListColumnParams{UserID: uid, StatusID: dest})
		if err != nil {
			return err
		}
		sibs := make([]positioned, 0, len(col))
		for _, c := range col {
			if c.ID != id {
				sibs = append(sibs, positioned{c.ID, c.Position})
			}
		}
		pos, err := placeAmong(sibs, m.MoveInput, false)
		if err != nil {
			return err
		}
		from, err := statusByID(ctx, q, uid, cur.StatusID)
		if err != nil {
			return err
		}
		row, err := changeStatus(ctx, q, uid, cur, from, to, pos)
		if err != nil {
			return err
		}
		out = jobFromRow(row)
		return nil
	})
	return out, err
}

// topOf returns a key before the first job in a column (excluding self).
func topOf(ctx context.Context, q *db.Queries, uid, statusID, self uuid.UUID) (string, error) {
	col, err := q.ListColumn(ctx, db.ListColumnParams{UserID: uid, StatusID: statusID})
	if err != nil {
		return "", err
	}
	col = slices.DeleteFunc(col, func(c db.ListColumnRow) bool { return c.ID == self })
	first := ""
	if len(col) > 0 {
		first = col[0].Position
	}
	return library.PositionBetween("", first)
}

func statusByID(ctx context.Context, q *db.Queries, uid, id uuid.UUID) (db.JobStatus, error) {
	rows, err := q.ListJobStatuses(ctx, uid)
	if err != nil {
		return db.JobStatus{}, err
	}
	i := slices.IndexFunc(rows, func(r db.JobStatus) bool { return r.ID == id })
	if i < 0 {
		return db.JobStatus{}, library.ErrNotFound
	}
	return rows[i], nil
}

// changeStatus places a locked job at pos in to's column. Entering a new
// column records an activity, and entering an active or offer stage sets
// applied_on if it is unset.
func changeStatus(ctx context.Context, q *db.Queries, uid uuid.UUID, job db.Job, from, to db.JobStatus, pos string) (db.Job, error) {
	applied := job.AppliedOn
	moved := from.ID != to.ID
	if moved && applied == nil && Stage(to.Stage).countsAsApplied() {
		applied = today()
	}
	row, err := q.MoveJob(ctx, db.MoveJobParams{UserID: uid, ID: job.ID, StatusID: to.ID, Position: pos, AppliedOn: applied})
	if err != nil {
		return row, err
	}
	if moved {
		if err := addActivity(ctx, q, uid, job.ID, db.JobActivityTypeStatusChanged, from.Name, to.Name, ""); err != nil {
			return row, err
		}
	}
	return row, emit(ctx, q, uid, "job.moved", map[string]any{"id": job.ID, "version": row.Version})
}

func (s *Service) SetJobArchived(ctx context.Context, uid, id uuid.UUID, archived bool) (Job, error) {
	var out Job
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		row, err := q.SetJobArchived(ctx, db.SetJobArchivedParams{UserID: uid, ID: id, Archived: archived})
		if err != nil {
			return notFound(err)
		}
		topic := "job.archived"
		if !archived {
			topic = "job.unarchived"
		}
		out = jobFromRow(row)
		return emit(ctx, q, uid, topic, map[string]any{"id": id, "version": row.Version})
	})
	return out, err
}

func (s *Service) DeleteJob(ctx context.Context, uid, id uuid.UUID) error {
	return s.tx(ctx, uid, func(q *db.Queries) error {
		n, err := q.DeleteJob(ctx, db.DeleteJobParams{UserID: uid, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return library.ErrNotFound
		}
		return emit(ctx, q, uid, "job.deleted", map[string]any{"id": id})
	})
}

// Sort orders job lists.
type Sort string

const (
	SortUpdated  Sort = "-updated_at"
	SortCreated  Sort = "-created_at"
	SortPosition Sort = "position"
)

var sorts = []string{string(SortUpdated), string(SortCreated), string(SortPosition)}

// ListFilter selects jobs. Zero values mean "any".
type ListFilter struct {
	StatusIDs       []uuid.UUID
	Stages          []string
	Q               string
	IncludeArchived bool
	Sort            Sort // default -updated_at
	Cursor          string
	Limit           int
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
	maxFilterIDs    = 100
)

type cursor struct {
	Sort     Sort      `json:"s"`
	Time     time.Time `json:"t,omitzero"`
	Position string    `json:"p,omitempty"`
	ID       uuid.UUID `json:"i"`
}

// ListJobs returns job summaries (no description or notes) and the cursor
// for the next page ("" when there is none).
func (s *Service) ListJobs(ctx context.Context, uid uuid.UUID, f ListFilter) ([]Job, string, error) {
	var fe fieldErrors
	if f.Sort == "" {
		f.Sort = SortUpdated
	}
	fe.oneOf("sort", string(f.Sort), sorts)
	for i, st := range f.Stages {
		fe.oneOf(fmt.Sprintf("stage[%d]", i), st, stages)
	}
	if len(f.StatusIDs) > maxFilterIDs {
		fe.add("status_id", fmt.Sprintf("at most %d", maxFilterIDs))
	}
	fe.text("q", &f.Q, maxShortText, false)
	var c *cursor
	if f.Cursor != "" {
		c = new(cursor)
		raw, err := base64.RawURLEncoding.DecodeString(f.Cursor)
		if err != nil || json.Unmarshal(raw, c) != nil || c.Sort != f.Sort ||
			(c.Sort == SortPosition && !library.ValidPosition(c.Position)) {
			fe.add("cursor", "is invalid")
		}
	}
	if err := fe.err(); err != nil {
		return nil, "", err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)
	var qp *string
	if f.Q != "" {
		p := "%" + escapeLike(f.Q) + "%"
		qp = &p
	}

	var out []Job
	var next string
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		var ids []uuid.UUID
		var cursors []cursor
		switch f.Sort {
		case SortUpdated:
			p := db.ListJobIDsByUpdatedParams{UserID: uid, IncludeArchived: f.IncludeArchived,
				StatusIds: f.StatusIDs, Stages: f.Stages, Q: qp, RowLimit: int32(limit + 1)}
			if c != nil {
				p.AfterTime, p.AfterID = &c.Time, &c.ID
			}
			rows, err := q.ListJobIDsByUpdated(ctx, p)
			if err != nil {
				return err
			}
			for _, r := range rows {
				ids, cursors = append(ids, r.ID), append(cursors, cursor{Sort: f.Sort, Time: r.UpdatedAt, ID: r.ID})
			}
		case SortCreated:
			p := db.ListJobIDsByCreatedParams{UserID: uid, IncludeArchived: f.IncludeArchived,
				StatusIds: f.StatusIDs, Stages: f.Stages, Q: qp, RowLimit: int32(limit + 1)}
			if c != nil {
				p.AfterTime, p.AfterID = &c.Time, &c.ID
			}
			rows, err := q.ListJobIDsByCreated(ctx, p)
			if err != nil {
				return err
			}
			for _, r := range rows {
				ids, cursors = append(ids, r.ID), append(cursors, cursor{Sort: f.Sort, Time: r.CreatedAt, ID: r.ID})
			}
		case SortPosition:
			p := db.ListJobIDsByPositionParams{UserID: uid, IncludeArchived: f.IncludeArchived,
				StatusIds: f.StatusIDs, Stages: f.Stages, Q: qp, RowLimit: int32(limit + 1)}
			if c != nil {
				p.AfterPosition, p.AfterID = &c.Position, &c.ID
			}
			rows, err := q.ListJobIDsByPosition(ctx, p)
			if err != nil {
				return err
			}
			for _, r := range rows {
				ids, cursors = append(ids, r.ID), append(cursors, cursor{Sort: f.Sort, Position: r.Position, ID: r.ID})
			}
		}
		if len(ids) > limit {
			ids = ids[:limit]
			b, _ := json.Marshal(cursors[limit-1])
			next = base64.RawURLEncoding.EncodeToString(b)
		}
		rows, err := q.GetJobSummaries(ctx, db.GetJobSummariesParams{UserID: uid, Ids: ids})
		if err != nil {
			return err
		}
		byID := make(map[uuid.UUID]db.GetJobSummariesRow, len(rows))
		for _, r := range rows {
			byID[r.ID] = r
		}
		out = make([]Job, 0, len(ids))
		for _, id := range ids {
			if r, ok := byID[id]; ok {
				out = append(out, summaryFromRow(r))
			}
		}
		return nil
	})
	return out, next, err
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func jobFromRow(r db.Job) Job {
	return Job{
		ID: r.ID, StatusID: r.StatusID, Title: r.Title, Company: r.Company, URL: r.Url,
		Location: r.Location, WorkMode: str(r.WorkMode), EmploymentType: str(r.EmploymentType),
		SalaryMin: r.SalaryMin, SalaryMax: r.SalaryMax, SalaryCurrency: r.SalaryCurrency,
		SalaryPeriod: str(r.SalaryPeriod), Source: r.Source, Description: r.Description, Notes: r.Notes,
		AppliedOn: r.AppliedOn, Position: r.Position, Version: int(r.Version), ContentHash: r.ContentHash,
		StatusChangedAt: r.StatusChangedAt, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func summaryFromRow(r db.GetJobSummariesRow) Job {
	return Job{
		ID: r.ID, StatusID: r.StatusID, Title: r.Title, Company: r.Company, URL: r.Url,
		Location: r.Location, WorkMode: str(r.WorkMode), EmploymentType: str(r.EmploymentType),
		SalaryMin: r.SalaryMin, SalaryMax: r.SalaryMax, SalaryCurrency: r.SalaryCurrency,
		SalaryPeriod: str(r.SalaryPeriod), Source: r.Source, AppliedOn: r.AppliedOn, Position: r.Position,
		Version: int(r.Version), StatusChangedAt: r.StatusChangedAt, ArchivedAt: r.ArchivedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
