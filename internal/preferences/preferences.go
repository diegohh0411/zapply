// Package preferences keeps what each member is looking for: a seniority
// range, areas, countries with remote, and employment types to skip
// (migration 00047, docs/parts/12-job-preferences.md).
//
// A starting profile is derived from the member's Experience library: plain
// rules for seniority, location and employment (Derive), and one decision,
// charged to the member, that reads their titles and skills for areas. A
// library change marks the preferences stale; the next read or the worker's
// recovery derives again. Each field group records whether it follows the
// derivation or the member set it, and re-deriving never overwrites the
// member's.
package preferences

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/decide"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/worker"
)

// Field groups, each with its own source.
const (
	FieldSeniority  = "seniority"
	FieldAreas      = "areas"
	FieldLocation   = "location"
	FieldEmployment = "employment"
)

var Fields = []string{FieldSeniority, FieldAreas, FieldLocation, FieldEmployment}

// Sources.
const (
	SourceDerived = "derived"
	SourceMember  = "member"
)

const (
	// minArea is how probable an area must be to count; a library spanning
	// two areas splits its probability between them.
	minArea = 0.2
	// maxAreas bounds the areas suggested.
	maxAreas = 3
	// recentWork is how many recent entries the area question reads.
	recentWork = 8
	topSkills  = 20
)

// Preferences are a member's current choices, with what derivation suggests.
type Preferences struct {
	SeniorityMin       *string
	SeniorityMax       *string
	Areas              []string
	Countries          []string
	RemoteOK           bool
	ExcludedEmployment []string
	Sources            map[string]string
	Suggested          Suggestion
	DerivedAt          *time.Time
	// Deriving is true while the area question waits for its answer.
	Deriving  bool
	Version   int
	UpdatedAt time.Time
}

// Update sets the given field groups as the member's; nil leaves one alone.
type Update struct {
	Seniority          *[2]string
	Areas              *[]string
	Location           *Location
	ExcludedEmployment *[]string
}

type Location struct {
	Countries []string
	RemoteOK  bool
}

type Service struct {
	store *store.Store
	queue *worker.Queue
	log   *slog.Logger
	now   func() time.Time
}

func New(st *store.Store, q *worker.Queue, log *slog.Logger) *Service {
	return &Service{store: st, queue: q, log: log, now: time.Now}
}

// Get returns the member's preferences, deriving them first when the library
// changed since.
func (s *Service) Get(ctx context.Context, uid uuid.UUID) (Preferences, error) {
	var p Preferences
	err := s.store.InTenant(ctx, uid, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO job_preference(user_id) VALUES($1) ON CONFLICT DO NOTHING`, uid); err != nil {
			return err
		}
		var stale bool
		if err := tx.QueryRow(ctx, `SELECT stale FROM job_preference WHERE user_id=$1 FOR UPDATE`, uid).Scan(&stale); err != nil {
			return err
		}
		if stale {
			if err := s.refresh(ctx, tx, uid); err != nil {
				return err
			}
		}
		var err error
		p, err = read(ctx, tx, uid)
		return err
	})
	return p, err
}

// Save stores the member's choices for the groups in u.
func (s *Service) Save(ctx context.Context, uid uuid.UUID, ifVersion int, u Update) (Preferences, error) {
	if err := validate(u); err != nil {
		return Preferences{}, err
	}
	var p Preferences
	err := s.store.InTenant(ctx, uid, func(tx pgx.Tx) error {
		if err := lockVersion(ctx, tx, uid, ifVersion); err != nil {
			return err
		}
		if u.Seniority != nil {
			if _, err := tx.Exec(ctx, `UPDATE job_preference SET seniority_min=$2,seniority_max=$3,seniority_source='member' WHERE user_id=$1`, uid, u.Seniority[0], u.Seniority[1]); err != nil {
				return err
			}
		}
		if u.Areas != nil {
			if _, err := tx.Exec(ctx, `UPDATE job_preference SET areas=$2,areas_source='member' WHERE user_id=$1`, uid, sortedUnique(*u.Areas)); err != nil {
				return err
			}
		}
		if u.Location != nil {
			if _, err := tx.Exec(ctx, `UPDATE job_preference SET countries=$2,remote_ok=$3,location_source='member' WHERE user_id=$1`, uid, sortedUnique(u.Location.Countries), u.Location.RemoteOK); err != nil {
				return err
			}
		}
		if u.ExcludedEmployment != nil {
			if _, err := tx.Exec(ctx, `UPDATE job_preference SET excluded_employment=$2,employment_source='member' WHERE user_id=$1`, uid, sortedUnique(*u.ExcludedEmployment)); err != nil {
				return err
			}
		}
		if err := bump(ctx, tx, uid); err != nil {
			return err
		}
		var err error
		p, err = read(ctx, tx, uid)
		return err
	})
	return p, err
}

// Reset makes the given groups follow the derivation again.
func (s *Service) Reset(ctx context.Context, uid uuid.UUID, ifVersion int, fields []string) (Preferences, error) {
	for _, f := range fields {
		if !contains(Fields, f) {
			return Preferences{}, &library.ValidationError{Fields: []library.FieldError{{Field: "fields", Message: "Unknown field " + f + "."}}}
		}
	}
	var p Preferences
	err := s.store.InTenant(ctx, uid, func(tx pgx.Tx) error {
		if err := lockVersion(ctx, tx, uid, ifVersion); err != nil {
			return err
		}
		for _, f := range fields {
			if _, err := tx.Exec(ctx, `UPDATE job_preference SET `+f+`_source='derived' WHERE user_id=$1`, uid); err != nil {
				return err
			}
		}
		if err := apply(ctx, tx, uid); err != nil {
			return err
		}
		if err := bump(ctx, tx, uid); err != nil {
			return err
		}
		var err error
		p, err = read(ctx, tx, uid)
		return err
	})
	return p, err
}

func lockVersion(ctx context.Context, tx pgx.Tx, uid uuid.UUID, ifVersion int) error {
	if _, err := tx.Exec(ctx, `INSERT INTO job_preference(user_id) VALUES($1) ON CONFLICT DO NOTHING`, uid); err != nil {
		return err
	}
	var v int
	if err := tx.QueryRow(ctx, `SELECT version FROM job_preference WHERE user_id=$1 FOR UPDATE`, uid).Scan(&v); err != nil {
		return err
	}
	if v != ifVersion {
		return library.ErrVersionMismatch
	}
	return nil
}

func bump(ctx context.Context, tx pgx.Tx, uid uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE job_preference SET version=version+1,updated_at=now() WHERE user_id=$1`, uid)
	return err
}

func validate(u Update) error {
	var fe []library.FieldError
	if u.Seniority != nil {
		lo, hi := level(u.Seniority[0]), level(u.Seniority[1])
		if lo < 0 || hi < 0 || lo > hi {
			fe = append(fe, library.FieldError{Field: "seniority", Message: "Pick a range from one level to the same or a higher one."})
		}
	}
	if u.Areas != nil {
		for _, a := range *u.Areas {
			if !contains(catalog.Areas, a) {
				fe = append(fe, library.FieldError{Field: "areas", Message: "Unknown area " + a + "."})
			}
		}
	}
	if u.Location != nil {
		if len(u.Location.Countries) > 50 {
			fe = append(fe, library.FieldError{Field: "countries", Message: "Pick at most 50 countries."})
		}
		for _, c := range u.Location.Countries {
			if catalog.CountryCode(c) != c {
				fe = append(fe, library.FieldError{Field: "countries", Message: "Unknown country code " + c + "."})
			}
		}
	}
	if u.ExcludedEmployment != nil {
		for _, e := range *u.ExcludedEmployment {
			if !contains(catalog.EmploymentTypes, e) {
				fe = append(fe, library.FieldError{Field: "excluded_employment", Message: "Unknown employment type " + e + "."})
			}
		}
	}
	if fe != nil {
		return &library.ValidationError{Fields: fe}
	}
	return nil
}

func read(ctx context.Context, tx pgx.Tx, uid uuid.UUID) (Preferences, error) {
	p := Preferences{Sources: map[string]string{}}
	var sen, area, loc, emp string
	var derived []byte
	var op *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT seniority_min,seniority_max,seniority_source,areas,areas_source,countries,remote_ok,location_source,
 excluded_employment,employment_source,derived,derived_at,area_operation,version,updated_at FROM job_preference WHERE user_id=$1`, uid).Scan(
		&p.SeniorityMin, &p.SeniorityMax, &sen, &p.Areas, &area, &p.Countries, &p.RemoteOK, &loc,
		&p.ExcludedEmployment, &emp, &derived, &p.DerivedAt, &op, &p.Version, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.Sources[FieldSeniority], p.Sources[FieldAreas], p.Sources[FieldLocation], p.Sources[FieldEmployment] = sen, area, loc, emp
	if len(derived) > 2 {
		if err := json.Unmarshal(derived, &p.Suggested); err != nil {
			return p, err
		}
	}
	for _, l := range []*[]string{&p.Suggested.Areas, &p.Suggested.Countries, &p.Suggested.ExcludedEmployment} {
		if *l == nil {
			*l = []string{}
		}
	}
	p.Deriving = op != nil
	return p, nil
}

// apply copies the derivation into every group that follows it.
func apply(ctx context.Context, tx pgx.Tx, uid uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE job_preference SET
 seniority_min=CASE WHEN seniority_source='derived' THEN derived->>'seniority_min' ELSE seniority_min END,
 seniority_max=CASE WHEN seniority_source='derived' THEN derived->>'seniority_max' ELSE seniority_max END,
 areas=CASE WHEN areas_source='derived' THEN COALESCE(ARRAY(SELECT jsonb_array_elements_text(derived->'areas')),'{}') ELSE areas END,
 countries=CASE WHEN location_source='derived' THEN COALESCE(ARRAY(SELECT jsonb_array_elements_text(derived->'countries')),'{}') ELSE countries END,
 remote_ok=CASE WHEN location_source='derived' THEN COALESCE((derived->>'remote_ok')::boolean,true) ELSE remote_ok END,
 excluded_employment=CASE WHEN employment_source='derived' THEN COALESCE(ARRAY(SELECT jsonb_array_elements_text(derived->'excluded_employment')),'{}') ELSE excluded_employment END
 WHERE user_id=$1 AND derived<>'{}'`, uid)
	return err
}

// refresh derives the preferences again from the library, inside the
// member's transaction, and asks the area question when its input changed.
func (s *Service) refresh(ctx context.Context, tx pgx.Tx, uid uuid.UUID) error {
	lib, err := loadLibrary(ctx, tx)
	if err != nil {
		return err
	}
	sg := Derive(lib.entries, lib.location, s.now())
	var prev []byte
	var hash string
	if err := tx.QueryRow(ctx, `SELECT derived,area_input_hash FROM job_preference WHERE user_id=$1`, uid).Scan(&prev, &hash); err != nil {
		return err
	}
	var old Suggestion
	_ = json.Unmarshal(prev, &old)
	sg.Areas = old.Areas
	if sg.Areas == nil {
		sg.Areas = []string{}
	}

	state, empty := areaState(lib)
	var model *string
	if err := tx.QueryRow(ctx, `SELECT decision FROM platform.posting_models()`).Scan(&model); err != nil {
		return err
	}
	if empty {
		sg.Areas = []string{}
		hash = ""
	} else if model != nil {
		h, err := systemone.Hash(map[string]any{"model": *model, "state": state})
		if err != nil {
			return err
		}
		if h != hash {
			hash = h
			cached, err := s.askAreas(ctx, tx, uid, *model, state)
			if err != nil {
				return err
			}
			if cached != nil {
				sg.Areas = cached
			}
		}
	}
	body, err := json.Marshal(sg)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE job_preference SET derived=$2,derived_at=now(),area_input_hash=$3,stale=false WHERE user_id=$1`, uid, json.RawMessage(body), hash); err != nil {
		return err
	}
	return apply(ctx, tx, uid)
}

// areaQuestion asks which area a person's experience is in.
func areaQuestion() systemone.Question {
	return systemone.Question{Type: "choice",
		Instructions: systemone.Raw(map[string]any{
			"question": "Which professional area does the person in `candidate` work in, or study toward? Judge from their job titles, organizations, degree and skills, weighing recent work most. A person who spans two areas belongs to the one they are first, as the boundary cases in the options say.",
			"note":     "Entries may be written in English or Spanish.",
		}),
		Criteria: systemone.Raw(catalog.AreaCriteria())}
}

// askAreas returns the cached areas for state, or queues the question,
// charged to the member, and returns nil.
func (s *Service) askAreas(ctx context.Context, tx pgx.Tx, uid uuid.UUID, model string, state json.RawMessage) ([]string, error) {
	item, err := decide.New("q1", state, areaQuestion())
	if err != nil {
		return nil, err
	}
	cached, err := decide.Lookup(ctx, tx, uid, model, item)
	if err != nil {
		return nil, err
	}
	if cached != nil {
		if _, err := tx.Exec(ctx, `UPDATE job_preference SET area_operation=NULL WHERE user_id=$1`, uid); err != nil {
			return nil, err
		}
		return areasFrom(*cached), nil
	}
	if s.queue == nil {
		return nil, errors.New("preferences queue unavailable")
	}
	packed, err := decide.Pack(model, []decide.Pending{item}, 0, 1)
	if err != nil || len(packed) != 1 {
		return nil, errors.Join(errors.New("area question does not fit one request"), err)
	}
	h, err := systemone.Hash(packed[0].Request)
	if err != nil {
		return nil, err
	}
	op := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'decision',$3,$4)`,
		op, uid, "preferences:areas:"+op.String(), h); err != nil {
		return nil, err
	}
	if err := decide.Store(ctx, tx, uid, op, packed[0]); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE job_preference SET area_operation=$2 WHERE user_id=$1`, uid, op); err != nil {
		return nil, err
	}
	return nil, s.queue.InsertTx(ctx, tx, worker.Args{UserID: uid, OperationID: op})
}

// areasFrom reads a choice: every area at least minArea probable, most
// probable first, never other.
func areasFrom(a systemone.Answer) []string {
	type kv struct {
		k string
		p float64
	}
	var all []kv
	for k, p := range a.Probabilities {
		if p >= minArea && k != "other" && contains(catalog.Areas, k) {
			all = append(all, kv{k, p})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].p > all[j].p || (all[i].p == all[j].p && all[i].k < all[j].k) })
	out := []string{}
	for _, x := range all {
		if len(out) < maxAreas {
			out = append(out, x.k)
		}
	}
	if len(out) == 0 && a.Choice != "other" && contains(catalog.Areas, a.Choice) {
		out = []string{a.Choice}
	}
	return out
}

// OnTerminal records the answer to an area question, inside the transaction
// that finished it. It reports false for operations that are not one.
func (s *Service) OnTerminal(ctx context.Context, tx pgx.Tx, op worker.Operation) (bool, error) {
	if op.Kind != "decision" {
		return false, nil
	}
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM job_preference WHERE user_id=$1 AND area_operation=$2)`, op.UserID, op.ID).Scan(&found); err != nil || !found {
		return false, err
	}
	switch op.Status {
	case "completed":
	case "failed", "cancelled", "needs_review":
		// Keep the areas suggested before; the next library change asks again.
		_, err := tx.Exec(ctx, `UPDATE job_preference SET area_operation=NULL,area_input_hash='' WHERE user_id=$1 AND area_operation=$2`, op.UserID, op.ID)
		return true, err
	default:
		return true, nil
	}
	answers, err := decide.Ingest(ctx, tx, op.UserID, op.ID, op.Result)
	if err != nil {
		return true, err
	}
	areas := []string{}
	if a, ok := answers["q1"]; ok {
		areas = areasFrom(a)
	}
	body, _ := json.Marshal(areas)
	if _, err := tx.Exec(ctx, `UPDATE job_preference SET area_operation=NULL,derived=jsonb_set(derived,'{areas}',$3::jsonb)
 WHERE user_id=$1 AND area_operation=$2`, op.UserID, op.ID, string(body)); err != nil {
		return true, err
	}
	return true, apply(ctx, tx, op.UserID)
}

// Recover derives the preferences of members whose library changed.
func (s *Service) Recover(ctx context.Context, _ *river.Client[pgx.Tx]) error {
	var ids []uuid.UUID
	if err := s.store.Pool().QueryRow(ctx, `SELECT platform.stale_job_preferences(100)`).Scan(&ids); err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		err := s.store.InTenant(ctx, id, func(tx pgx.Tx) error {
			var stale bool
			if err := tx.QueryRow(ctx, `SELECT stale FROM job_preference WHERE user_id=$1 FOR UPDATE SKIP LOCKED`, id).Scan(&stale); errors.Is(err, pgx.ErrNoRows) || (err == nil && !stale) {
				return nil
			} else if err != nil {
				return err
			}
			return s.refresh(ctx, tx, id)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("preferences %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

type libraryData struct {
	entries  []Entry
	location string
	headline string
	degree   string
	skills   []string
}

func loadLibrary(ctx context.Context, tx pgx.Tx) (libraryData, error) {
	var l libraryData
	if err := tx.QueryRow(ctx, `SELECT location,headline FROM profile`).Scan(&l.location, &l.headline); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return l, err
	}
	rows, err := tx.Query(ctx, `SELECT e.kind,e.category::text,e.title,COALESCE(o.name,''),e.location,e.start_date,e.end_date,e.is_current
 FROM evidence e LEFT JOIN organization o ON o.user_id=e.user_id AND o.id=e.organization_id
 WHERE e.parent_id IS NULL AND e.archived_at IS NULL AND e.form='engagement'
 ORDER BY e.is_current DESC,COALESCE(e.end_date,e.start_date) DESC NULLS LAST,e.start_date DESC NULLS LAST,e.id`)
	if err != nil {
		return l, err
	}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Kind, &e.Category, &e.Title, &e.Org, &e.Location, &e.Start, &e.End, &e.Current); err != nil {
			rows.Close()
			return l, err
		}
		l.entries = append(l.entries, e)
		if e.Kind == "degree_program" && l.degree == "" {
			l.degree = e.Title
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return l, err
	}
	rows, err = tx.Query(ctx, `SELECT s.name FROM skill s LEFT JOIN evidence_skill es ON es.user_id=s.user_id AND es.skill_id=s.id
 WHERE s.type IN ('hard','tool') GROUP BY s.id,s.name ORDER BY count(es.evidence_id) DESC,s.name LIMIT $1`, topSkills)
	if err != nil {
		return l, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return l, err
		}
		l.skills = append(l.skills, n)
	}
	rows.Close()
	return l, rows.Err()
}

// areaState is what the area question reads: recent work and internships,
// the latest degree, the headline and the most used skills. No contact
// details, summaries or highlights. empty reports nothing to read.
func areaState(l libraryData) (json.RawMessage, bool) {
	type item struct {
		Kind  string `json:"kind"`
		Title string `json:"title"`
		Org   string `json:"organization,omitempty"`
		From  string `json:"from,omitempty"`
		To    string `json:"to,omitempty"`
	}
	work := []item{}
	for _, e := range l.entries {
		if len(work) == recentWork {
			break
		}
		if !(fullWork[e.Kind] || halfWork[e.Kind]) {
			continue
		}
		it := item{Kind: e.Kind, Title: e.Title, Org: e.Org}
		if e.Start != nil {
			it.From = e.Start.Format("2006-01")
		}
		if e.Current {
			it.To = "present"
		} else if e.End != nil {
			it.To = e.End.Format("2006-01")
		}
		work = append(work, it)
	}
	skills := l.skills
	if skills == nil {
		skills = []string{}
	}
	c := map[string]any{"experience": work, "skills": skills}
	if l.headline != "" {
		c["headline"] = l.headline
	}
	if l.degree != "" {
		c["degree"] = l.degree
	}
	empty := len(work) == 0 && len(skills) == 0 && l.headline == "" && l.degree == ""
	return systemone.Raw(map[string]any{"candidate": c}), empty
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}
