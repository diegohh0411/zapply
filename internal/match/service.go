package match

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/diegohh0411/zapply/internal/decide"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/membership"
	modelapi "github.com/diegohh0411/zapply/internal/model"
	"github.com/diegohh0411/zapply/internal/model/billing"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/store/db"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"math"
	"sort"
)

// AdvanceArgs is unique per run and triggering operation. River keeps a running
// job unique, so a run-only key would drop the wake-up from a request that
// finishes while another request's Advance is running. Jobs for one run
// serialize on the job lock.
type AdvanceArgs struct {
	UserID      uuid.UUID `json:"user_id" river:"unique"`
	RunID       uuid.UUID `json:"run_id" river:"unique"`
	OperationID uuid.UUID `json:"operation_id" river:"unique"`
}

func (AdvanceArgs) Kind() string { return "match_advance" }

type Service struct {
	store *store.Store
	queue *worker.Queue
}

func New(st *store.Store, q *worker.Queue) *Service { return &Service{st, q} }

type Conflict struct{ ExistingID uuid.UUID }

func (e *Conflict) Error() string { return "a match run is already active" }

type Run struct {
	ID                 uuid.UUID  `json:"id"`
	JobID              uuid.UUID  `json:"job_id"`
	Status             string     `json:"status"`
	Stage              string     `json:"stage"`
	Model              *string    `json:"model"`
	Pipeline           int        `json:"pipeline"`
	Trigger            string     `json:"trigger"`
	BlockedReason      string     `json:"blocked_reason"`
	NextEligibleAt     *time.Time `json:"next_eligible_at"`
	Version            int        `json:"version"`
	JobHash            string     `json:"job_hash"`
	LibraryFingerprint string     `json:"library_fingerprint"`
	SkillsHash         string     `json:"skills_hash"`
	Snapshot           Snapshot   `json:"-"`
	Result             Result     `json:"result"`
	Score              *float64   `json:"score"`
	MustMet            int        `json:"must_met"`
	MustTotal          int        `json:"must_total"`
	Total              int        `json:"total"`
	Answered           int        `json:"answered"`
	Cached             int        `json:"cached"`
	Unanswered         int        `json:"unanswered"`
	CreatedAt          time.Time  `json:"created_at"`
	FinishedAt         *time.Time `json:"finished_at"`
	Stale              bool       `json:"stale"`
	Truncated          bool       `json:"truncated"`
	ObsoleteOverrides  []string   `json:"obsolete_overrides"`
	Changed            int        `json:"changed"`
	Added              int        `json:"added"`
	Removed            int        `json:"removed"`
	CostUSD            string     `json:"cost_usd"`
	RerunRequested     bool       `json:"rerun_requested"`
}

func loadRun(ctx context.Context, tx pgx.Tx, u, id uuid.UUID, lock bool) (Run, error) {
	sql := `SELECT to_jsonb(r),snapshot FROM match_run r WHERE user_id=$1 AND id=$2`
	if lock {
		sql += " FOR UPDATE"
	}
	var raw, snap []byte
	r := Run{Result: emptyResult()}
	if e := tx.QueryRow(ctx, sql, u, id).Scan(&raw, &snap); e != nil {
		return r, e
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return r, e
	}
	return r, json.Unmarshal(snap, &r.Snapshot)
}

const evidenceSnapshotSQL = `SELECT COALESCE(jsonb_agg(facts ORDER BY position,id),'[]') FROM (
 SELECT e.position,e.id,(to_jsonb(e)-'user_id'-'content_hash'-'created_at'-'updated_at'-'version') || jsonb_build_object(
 'content_hash',encode(e.content_hash,'hex'),
 'organization',(SELECT to_jsonb(o)-'user_id' FROM organization o WHERE o.user_id=e.user_id AND o.id=e.organization_id),
 'skills',COALESCE((SELECT jsonb_agg(to_jsonb(s)-'user_id' ORDER BY s.normalized_name,s.id) FROM skill s JOIN evidence_skill es ON es.user_id=s.user_id AND es.skill_id=s.id WHERE es.user_id=e.user_id AND es.evidence_id=e.id),'[]'),
 'highlights',COALESCE((SELECT jsonb_agg((to_jsonb(h)-'user_id'-'content_hash')||jsonb_build_object('skills',COALESCE((SELECT jsonb_agg(to_jsonb(s)-'user_id' ORDER BY s.normalized_name,s.id) FROM skill s JOIN highlight_skill hs ON hs.user_id=s.user_id AND hs.skill_id=s.id WHERE hs.user_id=h.user_id AND hs.highlight_id=h.id),'[]')) ORDER BY h.position,h.id) FROM highlight h WHERE h.user_id=e.user_id AND h.evidence_id=e.id),'[]')) facts
 FROM evidence e WHERE e.user_id=$1 AND e.archived_at IS NULL AND e.kind<>'career_break' ORDER BY e.position,e.id) x`

func snapshot(ctx context.Context, tx pgx.Tx, u, job uuid.UUID) (Snapshot, string, string, string, error) {
	var s Snapshot
	var hash []byte
	e := tx.QueryRow(ctx, `SELECT title,company,description,content_hash FROM job WHERE user_id=$1 AND id=$2`, u, job).Scan(&s.Title, &s.Company, &s.Description, &hash)
	if e != nil {
		return s, "", "", "", e
	}
	var raw []byte
	if e = tx.QueryRow(ctx, evidenceSnapshotSQL, u).Scan(&raw); e != nil {
		return s, "", "", "", e
	}
	if e = json.Unmarshal(raw, &s.Evidence); e != nil {
		return s, "", "", "", e
	}

	if e = tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(s)-'user_id' ORDER BY normalized_name,id),'[]') FROM skill s WHERE user_id=$1`, u).Scan(&raw); e != nil {
		return s, "", "", "", e
	}
	if e = json.Unmarshal(raw, &s.Skills); e != nil {
		return s, "", "", "", e
	}
	text := s.Description
	if strings.TrimSpace(text) == "" {
		text = s.Title
	}
	s.Lines, s.Truncated = SplitLines(text)
	sh, e := systemone.Hash(s.Skills)
	if e != nil {
		return s, "", "", "", e
	}
	fp, e := fingerprint(s)
	return s, hex.EncodeToString(hash), sh, fp, e
}
func fingerprint(s Snapshot) (string, error) {
	// Full documents include linked skill and organization edits even where an
	// older library content_hash does not include those related rows.
	return systemone.Hash(struct {
		Evidence []Evidence `json:"evidence"`
		Skills   []Skill    `json:"skills"`
	}{s.Evidence, s.Skills})
}
func emitRun(ctx context.Context, tx pgx.Tx, u, id, job uuid.UUID) error {
	_, e := tx.Exec(ctx, `INSERT INTO outbox(id,user_id,topic,payload) SELECT $1,$2,'match_run.changed',jsonb_build_object('id',id,'job_id',job_id,'version',version) FROM match_run WHERE id=$3 AND user_id=$2`, uuid.Must(uuid.NewV7()), u, id)
	return e
}
func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) error {
	return s.wake(ctx, tx, u, id, uuid.Nil)
}

// wake enqueues an Advance for the run, keyed by the operation that triggered it.
func (s *Service) wake(ctx context.Context, tx pgx.Tx, u, id, op uuid.UUID) error {
	if s.queue == nil {
		return errors.New("matching queue unavailable")
	}
	return s.queue.InsertTx(ctx, tx, AdvanceArgs{u, id, op})
}
func (s *Service) create(ctx context.Context, tx pgx.Tx, u, job uuid.UUID, key, trigger string, version int) (Run, error) {
	if key == "" || len(key) > 255 || strings.ContainsAny(key, "\x00\r\n") {
		return Run{}, &library.ValidationError{Fields: []library.FieldError{{Field: "Idempotency-Key", Message: "must be 1 to 255 characters"}}}
	}
	// The job lock serializes starts, automatic saves and cancellations.
	var exists uuid.UUID
	if e := tx.QueryRow(ctx, `SELECT platform.lock_match_job($2::uuid) WHERE $1::uuid=app_user_id()`, u, job).Scan(&exists); e != nil {
		return Run{}, e
	}
	var old uuid.UUID
	e := tx.QueryRow(ctx, `SELECT id FROM match_run WHERE user_id=$1 AND idempotency_key=$2`, u, key).Scan(&old)
	if e == nil {
		r, e := loadRun(ctx, tx, u, old, false)
		if r.JobID != job {
			return Run{}, &library.ConflictError{Message: "idempotency key belongs to another job"}
		}
		return r, e
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return Run{}, e
	}
	e = tx.QueryRow(ctx, `SELECT id FROM match_run WHERE user_id=$1 AND job_id=$2 AND status IN ('pending','running','blocked','needs_review')`, u, job).Scan(&old)
	if e == nil {
		return Run{}, &Conflict{old}
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return Run{}, e
	}
	snap, jh, sh, fp, e := snapshot(ctx, tx, u, job)
	if e != nil {
		return Run{}, e
	}
	limited := len(snap.Evidence) > 300
	if limited {
		snap.Evidence = snap.Evidence[:300]
	}
	id := uuid.Must(uuid.NewV7())
	var model *string
	if e = tx.QueryRow(ctx, `SELECT platform.selected_decision_model()`).Scan(&model); e != nil {
		return Run{}, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO match_run(id,user_id,job_id,idempotency_key,trigger,model,job_hash,skills_hash,library_fingerprint,snapshot,pipeline) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, u, job, key, trigger, model, jh, sh, fp, systemone.Raw(snap), version)
	if e != nil {
		return Run{}, e
	}
	for _, item := range snap.Evidence {
		if _, e = tx.Exec(ctx, `INSERT INTO match_run_item(user_id,run_id,evidence_id,content_hash,facts) VALUES($1,$2,$3,$4,$5)`, u, id, item.ID, item.ContentHash, systemone.Raw(item)); e != nil {
			return Run{}, e
		}
	}
	if limited {
		if _, e = tx.Exec(ctx, `UPDATE match_run SET status='failed',blocked_reason='evidence_limit_exceeded',finished_at=now() WHERE user_id=$1 AND id=$2`, u, id); e != nil {
			return Run{}, e
		}
	} else if e = s.enqueue(ctx, tx, u, id); e != nil {
		return Run{}, e
	}
	if e = emitRun(ctx, tx, u, id, job); e != nil {
		return Run{}, e
	}
	return loadRun(ctx, tx, u, id, false)
}
func translate(e error) error {
	if errors.Is(e, pgx.ErrNoRows) {
		return library.ErrNotFound
	}
	return membership.Translate(e)
}

// Start queues a manual run. A zero version selects DefaultPipeline; version 1
// remains available to compare the pipelines on the same job.
func (s *Service) Start(ctx context.Context, u, job uuid.UUID, key string, version int) (Run, error) {
	if version == 0 {
		version = DefaultPipeline
	}
	if _, ok := pipelines[version]; !ok {
		return Run{}, &library.ValidationError{Fields: []library.FieldError{{Field: "pipeline", Message: "must be 1 or 2"}}}
	}
	r := Run{Result: emptyResult()}
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var e error
		r, e = s.create(ctx, tx, u, job, key, "manual", version)
		return e
	})
	return r, translate(e)
}
func (s *Service) JobSaved(ctx context.Context, tx pgx.Tx, u uuid.UUID, job db.Job, created, hashChanged bool) error {
	if !created && !hashChanged || created && strings.TrimSpace(job.Description) == "" {
		return nil
	}
	var eligible bool
	if e := tx.QueryRow(ctx, `SELECT s.stage IN ('saved','active','offer') AND COALESCE((SELECT auto_match FROM profile WHERE user_id=$1),true) FROM job_status s WHERE s.user_id=$1 AND s.id=$2`, u, job.StatusID).Scan(&eligible); e != nil {
		return e
	}
	if !eligible {
		return nil
	}
	var id uuid.UUID
	e := tx.QueryRow(ctx, `SELECT id FROM match_run WHERE user_id=$1 AND job_id=$2 AND status IN ('pending','running','blocked','needs_review') FOR UPDATE`, u, job.ID).Scan(&id)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	if e == nil {
		var dispatched bool
		if e = tx.QueryRow(ctx, `SELECT platform.match_operations_dispatched(COALESCE(array_agg(operation_id),'{}'::uuid[])) FROM match_run_operation WHERE user_id=$1 AND run_id=$2`, u, id).Scan(&dispatched); e != nil {
			return e
		}
		if dispatched {
			_, e = tx.Exec(ctx, `UPDATE match_run SET rerun_requested=true,version=version+1 WHERE user_id=$1 AND id=$2`, u, id)
			if e != nil {
				return e
			}
			return emitRun(ctx, tx, u, id, job.ID)
		}
		if e = s.cancelTx(ctx, tx, u, id); e != nil {
			return e
		}
	}
	_, e = s.create(ctx, tx, u, job.ID, "auto:"+uuid.NewString(), "auto", DefaultPipeline)
	return e
}
func (s *Service) cancelTx(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) error {
	r, e := loadRun(ctx, tx, u, id, true)
	if e != nil {
		return e
	}
	if terminal(r.Status) {
		return nil
	}
	if _, e = tx.Exec(ctx, `SELECT platform.cancel_operation(operation_id) FROM (SELECT operation_id FROM match_run_operation WHERE user_id=$1 AND run_id=$2) x`, u, id); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE match_run SET status='cancelled',finished_at=now(),rerun_requested=false,version=version+1 WHERE user_id=$1 AND id=$2`, u, id); e != nil {
		return e
	}
	return emitRun(ctx, tx, u, id, r.JobID)
}
func terminal(status string) bool {
	return status == "completed" || status == "partial" || status == "failed" || status == "cancelled"
}
func (s *Service) Cancel(ctx context.Context, u, id uuid.UUID) error {
	return translate(s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var job uuid.UUID
		if e := tx.QueryRow(ctx, `SELECT job_id FROM match_run WHERE user_id=$1 AND id=$2`, u, id).Scan(&job); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `SELECT platform.lock_match_job($2::uuid) WHERE $1::uuid=app_user_id()`, u, job); e != nil {
			return e
		}
		return s.cancelTx(ctx, tx, u, id)
	}))
}

// LatestCompleted returns the job's newest completed run, with Stale set when
// the job or library changed since it ran. ok is false when there is none.
// Rendering (part 7) ranks a CV with it.
func LatestCompleted(ctx context.Context, tx pgx.Tx, u, job uuid.UUID) (r Run, ok bool, e error) {
	var id uuid.UUID
	e = tx.QueryRow(ctx, `SELECT id FROM match_run WHERE user_id=$1 AND job_id=$2 AND status='completed' ORDER BY created_at DESC,id DESC LIMIT 1`, u, job).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return r, false, nil
	}
	if e != nil {
		return r, false, e
	}
	if r, e = loadRun(ctx, tx, u, id, false); e != nil {
		return r, false, e
	}
	_, jh, _, fp, e := snapshot(ctx, tx, u, job)
	r.Stale = jh != r.JobHash || fp != r.LibraryFingerprint
	return r, e == nil, e
}

func (s *Service) Detail(ctx context.Context, u, id uuid.UUID) (Run, error) {
	var out Run
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var e error
		out, e = loadRun(ctx, tx, u, id, false)
		if e != nil {
			return e
		}
		cur, jh, _, fp, e := snapshot(ctx, tx, u, out.JobID)
		if e != nil {
			return e
		}
		out.Stale = jh != out.JobHash || fp != out.LibraryFingerprint
		out.Truncated = out.Snapshot.Truncated
		out.ObsoleteOverrides = []string{}
		currentLines := map[string]bool{}
		for _, line := range cur.Lines {
			h, _ := systemone.Hash(strings.ToLower(strings.Join(strings.Fields(line.Text), " ")))
			currentLines[h] = true
		}
		corrections, e := overrides(ctx, tx, u, out.JobID)
		if e != nil {
			return e
		}
		for hash := range corrections {
			if !currentLines[hash] {
				out.ObsoleteOverrides = append(out.ObsoleteOverrides, hash)
			}
		}
		sort.Strings(out.ObsoleteOverrides)
		if out.Status == "completed" || out.Status == "partial" {
			a, e := answers(ctx, tx, u, id)
			if e != nil {
				return e
			}
			corrections, e := overrides(ctx, tx, u, out.JobID)
			if e != nil {
				return e
			}
			asOf := out.CreatedAt
			if out.FinishedAt != nil {
				asOf = *out.FinishedAt
			}
			p := pipelineFor(out.Pipeline)
			req, incomplete := p.requirements(out.Snapshot, a, corrections)
			out.Result = p.compute(out.Snapshot, req, a, corrections, asOf, incomplete)
			out.Score = out.Result.Score
			out.MustMet = out.Result.MustMet
			out.MustTotal = out.Result.MustTotal
		}
		old := map[string]string{}
		now := map[string]bool{}
		for _, item := range out.Snapshot.Evidence {
			h, _ := systemone.Hash(item)
			old[item.ID] = h
		}
		for _, item := range cur.Evidence {
			now[item.ID] = true
			h, _ := systemone.Hash(item)
			if before, ok := old[item.ID]; !ok {
				out.Added++
			} else if before != h {
				out.Changed++
			}
		}
		for id := range old {
			if !now[id] {
				out.Removed++
			}
		}
		highlightExists := map[string]bool{}
		skillExists := map[string]bool{}
		for _, item := range cur.Evidence {
			for _, h := range item.Highlights {
				highlightExists[h.ID] = true
			}
		}
		for _, skill := range cur.Skills {
			skillExists[skill.ID] = true
		}
		filterHighlights := func(ids []string) []string {
			visible := []string{}
			for _, id := range ids {
				if highlightExists[id] {
					visible = append(visible, id)
				}
			}
			return visible
		}
		for i, c := range out.Result.Requirements {
			if !now[c.EvidenceID] {
				out.Result.Requirements[i].EvidenceID = ""
				out.Result.Requirements[i].EvidenceTitle = ""
			}
			out.Result.Requirements[i].HighlightIDs = filterHighlights(c.HighlightIDs)
			visible := []Citation{}
			for _, citation := range c.Citations {
				if highlightExists[citation.ID] {
					visible = append(visible, citation)
				}
			}
			out.Result.Requirements[i].Citations = visible
		}
		visibleSkills := []RankedSkill{}
		for _, skill := range out.Result.Skills {
			if skillExists[skill.SkillID] {
				visibleSkills = append(visibleSkills, skill)
			}
		}
		out.Result.Skills = visibleSkills
		filter := func(items []Selection) []Selection {
			visible := []Selection{}
			for _, item := range items {
				if now[item.EvidenceID] {
					item.HighlightIDs = filterHighlights(item.HighlightIDs)
					visible = append(visible, item)
				}
			}
			return visible
		}
		out.Result.Selection = filter(out.Result.Selection)
		out.Result.Suggestions = filter(out.Result.Suggestions)
		out.Result.Evidence = filter(out.Result.Evidence)
		ids := []uuid.UUID{}
		rows, e := tx.Query(ctx, `SELECT operation_id FROM match_run_operation WHERE user_id=$1 AND run_id=$2`, u, id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var op uuid.UUID
			if e = rows.Scan(&op); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, op)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}
		return tx.QueryRow(ctx, `SELECT platform.operation_costs($1)`, ids).Scan(&out.CostUSD)
	})
	return out, translate(e)
}
func (s *Service) List(ctx context.Context, u, job uuid.UUID, before *uuid.UUID, limit int) ([]Run, error) {
	out := []Run{}
	limit = min(100, max(1, limit))
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var exists uuid.UUID
		if e := tx.QueryRow(ctx, `SELECT id FROM job WHERE user_id=$1 AND id=$2`, u, job).Scan(&exists); e != nil {
			return e
		}
		rows, e := tx.Query(ctx, `SELECT to_jsonb(r)-'snapshot' FROM match_run r WHERE user_id=$1 AND job_id=$2 AND ($3::uuid IS NULL OR id<$3) ORDER BY id DESC LIMIT $4`, u, job, before, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if e = rows.Scan(&raw); e != nil {
				return e
			}
			r := Run{Result: emptyResult()}
			if e = json.Unmarshal(raw, &r); e != nil {
				return e
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, translate(e)
}
func (s *Service) OnTerminal(ctx context.Context, tx pgx.Tx, op worker.Operation) error {
	if op.Kind != "decision" {
		return nil
	}
	if op.Status == "completed" {
		if _, e := decide.Ingest(ctx, tx, op.UserID, op.ID, op.Result); e != nil {
			return e
		}
	}
	rows, e := tx.Query(ctx, `SELECT run_id FROM match_run_operation WHERE user_id=$1 AND operation_id=$2`, op.UserID, op.ID)
	if e != nil {
		return e
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, id := range ids {
		if e = s.wake(ctx, tx, op.UserID, id, op.ID); e != nil {
			return e
		}
	}
	return nil
}

type advanceWorker struct {
	river.WorkerDefaults[AdvanceArgs]
	service *Service
}

func (w *advanceWorker) Work(ctx context.Context, j *river.Job[AdvanceArgs]) error {
	return w.service.Advance(ctx, j.Args.UserID, j.Args.RunID)
}
func (s *Service) AddWorkers(w *river.Workers) { river.AddWorker(w, &advanceWorker{service: s}) }
func (s *Service) Recover(ctx context.Context, client *river.Client[pgx.Tx]) error {
	var raw []byte
	if e := s.store.Pool().QueryRow(ctx, `SELECT platform.active_match_runs(1000)`).Scan(&raw); e != nil {
		return e
	}
	var runs []AdvanceArgs
	if e := json.Unmarshal(raw, &runs); e != nil {
		return e
	}
	for _, r := range runs {
		if _, e := client.Insert(ctx, r, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}); e != nil {
			return e
		}
	}
	return nil
}
func answers(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) (map[string]systemone.Answer, error) {
	out := map[string]systemone.Answer{}
	rows, e := tx.Query(ctx, `SELECT subject,answer FROM match_run_question WHERE user_id=$1 AND run_id=$2 AND status IN ('answered','cached')`, u, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if e = rows.Scan(&key, &raw); e != nil {
			return nil, e
		}
		var a systemone.Answer
		if e = json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		out[key] = a
	}
	return out, rows.Err()
}
func overrides(ctx context.Context, tx pgx.Tx, u, job uuid.UUID) (map[string]string, error) {
	out := map[string]string{}
	rows, e := tx.Query(ctx, `SELECT text_hash,class FROM match_override WHERE user_id=$1 AND job_id=$2`, u, job)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var h, c string
		if e = rows.Scan(&h, &c); e != nil {
			return nil, e
		}
		out[h] = c
	}
	return out, rows.Err()
}
func (s *Service) Override(ctx context.Context, u, job uuid.UUID, hash string, class *string) error {
	if len(hash) != 64 {
		return &library.ValidationError{Fields: []library.FieldError{{Field: "text_hash", Message: "invalid requirement hash"}}}
	}
	if _, e := hex.DecodeString(hash); e != nil {
		return &library.ValidationError{Fields: []library.FieldError{{Field: "text_hash", Message: "invalid requirement hash"}}}
	}
	if class != nil && *class != "must" && *class != "preferred" && *class != "duty" && *class != "trait" && *class != "ignore" {
		return &library.ValidationError{Fields: []library.FieldError{{Field: "class", Message: "must be must, preferred, duty, trait or ignore"}}}
	}
	return translate(s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var exists uuid.UUID
		if e := tx.QueryRow(ctx, `SELECT platform.lock_match_job($2::uuid) WHERE $1::uuid=app_user_id()`, u, job).Scan(&exists); e != nil {
			return e
		}
		if class == nil {
			if _, e := tx.Exec(ctx, `DELETE FROM match_override WHERE user_id=$1 AND job_id=$2 AND text_hash=$3`, u, job, hash); e != nil {
				return e
			}
		} else {
			if _, e := tx.Exec(ctx, `INSERT INTO match_override(user_id,job_id,text_hash,class) VALUES($1,$2,$3,$4) ON CONFLICT(user_id,job_id,text_hash) DO UPDATE SET class=EXCLUDED.class`, u, job, hash, *class); e != nil {
				return e
			}
		}
		var id uuid.UUID
		e := tx.QueryRow(ctx, `SELECT id FROM match_run WHERE user_id=$1 AND job_id=$2 AND status IN ('completed','partial') ORDER BY created_at DESC,id DESC LIMIT 1`, u, job).Scan(&id)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		r, e := loadRun(ctx, tx, u, id, true)
		if e != nil {
			return e
		}
		return s.finalize(ctx, tx, u, r, false)
	}))
}

// Estimate prices each packed request as the gateway will reserve it, after
// cache hits. Before reading completes, all lines may qualify; scoring reserves for up to 40 longest source lines and
// deliberately assumes no score-cache hits so an unknown classification cannot
// make the upper estimate smaller than the requests actually needed.
type Estimate struct {
	Available    bool    `json:"available"`
	Reason       string  `json:"reason"`
	CostUSD      *string `json:"cost_usd"`
	Questions    int     `json:"questions"`
	Requests     int     `json:"requests"`
	Available7D  string  `json:"available_7d_usd"`
	Available30D string  `json:"available_30d_usd"`
}

func (s *Service) Estimate(ctx context.Context, u, job uuid.UUID) (Estimate, error) {
	out := Estimate{Reason: "estimate_unavailable"}
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		snap, _, _, _, e := snapshot(ctx, tx, u, job)
		if e != nil {
			return e
		}
		var model *string
		if e = tx.QueryRow(ctx, `SELECT platform.selected_decision_model()`).Scan(&model); e != nil {
			return e
		}
		var usage []byte
		if e = tx.QueryRow(ctx, `SELECT platform.usage()`).Scan(&usage); e != nil {
			return e
		}
		var available struct {
			Week  string `json:"available_7d_usd"`
			Month string `json:"available_30d_usd"`
		}
		if e = json.Unmarshal(usage, &available); e != nil {
			return e
		}
		out.Available7D = available.Week
		out.Available30D = available.Month
		if model == nil {
			return nil
		}
		var raw []byte
		if e = tx.QueryRow(ctx, `SELECT platform.decision_quote($1)`, *model).Scan(&raw); e != nil {
			return e
		}
		if len(raw) == 0 {
			return nil
		}
		var quote modelapi.Quote
		if e = json.Unmarshal(raw, &quote); e != nil {
			return e
		}
		if !quote.Decision || quote.Sheet == nil || quote.MaxRequestBytes <= 0 || time.Since(quote.FetchedAt) > 15*time.Minute {
			return nil
		}
		var calibration []byte
		if e = tx.QueryRow(ctx, `SELECT platform.model_calibration($1)`, *model).Scan(&calibration); e != nil {
			return e
		}
		p := pipelineFor(DefaultPipeline)
		read, e := p.read(snap)
		if e != nil {
			return e
		}
		found, e := decide.LookupAll(ctx, tx, u, *model, read)
		if e != nil {
			return e
		}
		cached := map[string]systemone.Answer{}
		pending := []decide.Pending{}
		for i, q := range read {
			if found[i] == nil {
				pending = append(pending, q)
			} else {
				cached[q.Subject] = *found[i]
			}
		}
		requirements, incomplete := p.requirements(snap, cached)
		if incomplete {
			type line struct {
				Line
				index int
			}
			lines := []line{}
			for i, l := range snap.Lines {
				lines = append(lines, line{l, i})
			}
			sort.SliceStable(lines, func(i, j int) bool { return len(lines[i].Text) > len(lines[j].Text) })
			if len(lines) > 40 {
				lines = lines[:40]
			}
			requirements = []Requirement{}
			for _, l := range lines {
				h, _ := systemone.Hash(strings.ToLower(strings.Join(strings.Fields(l.Text), " ")))
				requirements = append(requirements, Requirement{Text: l.Text, TextHash: h, Line: l.index, Class: "must"})
			}
		}
		scores, e := p.score(snap, requirements)
		if e != nil {
			return e
		}
		if incomplete {
			pending = append(pending, scores...)
		} else {
			found, e = decide.LookupAll(ctx, tx, u, *model, scores)
			if e != nil {
				return e
			}
			for i, q := range scores {
				if found[i] == nil {
					pending = append(pending, q)
				}
			}
		}
		packed, e := decide.Pack(*model, pending, quote.MaxRequestBytes, requestQuestions(quote.MaxQuestions))
		if e != nil {
			return nil
		}
		var total int64
		// Each request is priced exactly as the gateway will reserve it.
		for _, request := range packed {
			raw, e := json.Marshal(request.Request)
			if e != nil {
				return e
			}
			p, e := billing.Predict(*quote.Sheet, billing.Shape{RequestBytes: len(raw), Questions: len(request.Items)}, modelapi.CalibrationOf(calibration))
			if e != nil || total > math.MaxInt64-p.Reserved {
				return nil
			}
			total += p.Reserved
		}
		cost := modelapi.USD(total)
		out.Available = true
		out.Reason = ""
		out.CostUSD = &cost
		out.Questions = len(pending)
		out.Requests = len(packed)
		return nil
	})
	return out, translate(e)
}

type Summary struct {
	RunID  uuid.UUID `json:"run_id"`
	Status string    `json:"status"`
	Score  *float64  `json:"score"`
	Stale  bool      `json:"stale"`
}

// Summaries hashes the library once per board request, then reads all latest runs.
func (s *Service) Summaries(ctx context.Context, u uuid.UUID, jobs []uuid.UUID) (map[uuid.UUID]Summary, error) {
	out := map[uuid.UUID]Summary{}
	if len(jobs) == 0 {
		return out, nil
	}
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		_, _, _, fp, e := snapshot(ctx, tx, u, jobs[0])
		if e != nil {
			return e
		}
		rows, e := tx.Query(ctx, `SELECT DISTINCT ON (r.job_id) r.job_id,r.id,r.status,r.score,r.job_hash<>encode(j.content_hash,'hex') OR r.library_fingerprint<>$3 FROM match_run r JOIN job j ON j.user_id=r.user_id AND j.id=r.job_id WHERE r.user_id=$1 AND r.job_id=ANY($2) ORDER BY r.job_id,r.created_at DESC,r.id DESC`, u, jobs, fp)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var job uuid.UUID
			var v Summary
			if e = rows.Scan(&job, &v.RunID, &v.Status, &v.Score, &v.Stale); e != nil {
				return e
			}
			out[job] = v
		}
		return rows.Err()
	})
	return out, e
}

func emptyResult() Result {
	return Result{Requirements: []Coverage{}, Evidence: []Selection{}, Selection: []Selection{}, Suggestions: []Selection{}, Skills: []RankedSkill{}, Insights: []Insight{}}
}

// MarshalJSON exposes the Part 7 contract at the top level as well as the
// complete grouped result used by the Match tab.
func (r Run) MarshalJSON() ([]byte, error) {
	type plain Run
	raw, e := json.Marshal(plain(r))
	if e != nil {
		return nil, e
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal(raw, &fields); e != nil {
		return nil, e
	}
	result, e := json.Marshal(r.Result)
	if e != nil {
		return nil, e
	}
	var computed map[string]json.RawMessage
	if e = json.Unmarshal(result, &computed); e != nil {
		return nil, e
	}
	for key, value := range computed {
		if _, ok := fields[key]; !ok {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}
