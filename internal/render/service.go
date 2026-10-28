package render

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/match"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/templates"
)

// MaxContentBytes caps a snapshot's content (the column checks it too).
const MaxContentBytes = 256 << 10

type Service struct {
	store *store.Store
	// Now is the clock for content dates; tests replace it.
	Now func() time.Time
	// Typst compiles exports on the server; nil disables it.
	Typst *Compiler
}

func NewService(st *store.Store, typst *Compiler) *Service {
	return &Service{store: st, Now: time.Now, Typst: typst}
}

// AssetsPath is where the API serves templates.FS (see api.Handler).
const AssetsPath = "/assets/templates/"

// Template is a template version and the files a browser compiles it with.
type Template struct {
	Name      string   `json:"name"`
	Version   int      `json:"version"`
	Typst     string   `json:"typst"`
	Locales   []string `json:"locales"`
	Published bool     `json:"published"`
	Assets    Assets   `json:"assets"`
}

type Assets struct {
	Main    string   `json:"main"`
	Strings string   `json:"strings"`
	Fonts   []string `json:"fonts"`
}

func info(m templates.Manifest) Template {
	t := Template{Name: m.Name, Version: m.Version, Typst: m.Typst, Locales: m.Locales, Published: m.Published,
		Assets: Assets{Main: AssetsPath + m.Dir() + "/main.typ", Strings: AssetsPath + m.Dir() + "/strings.json", Fonts: []string{}}}
	entries, _ := fs.ReadDir(templates.FS(), m.FontDir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ttf") || strings.HasSuffix(e.Name(), ".otf") {
			t.Assets.Fonts = append(t.Assets.Fonts, AssetsPath+m.FontDir()+"/"+e.Name())
		}
	}
	return t
}

// Templates lists published template versions, including old versions needed
// to recompile saved exports.
func Templates() ([]Template, error) {
	ms, err := templates.Manifests()
	out := make([]Template, 0, len(ms))
	for _, m := range ms {
		if m.Published {
			out = append(out, info(m))
		}
	}
	return out, err
}

// manifest finds a template version; a nil version is the latest.
func manifest(name string, version *int) (templates.Manifest, bool) {
	ms, _ := templates.Manifests()
	var found templates.Manifest
	ok := false
	for _, m := range ms {
		if m.Published && m.Name == name && (version == nil && (!ok || m.Version > found.Version) || version != nil && m.Version == *version) {
			found, ok = m, true
		}
	}
	return found, ok
}

type Draft struct {
	JobID           uuid.UUID `json:"job_id"`
	Template        string    `json:"template"`
	TemplateVersion *int      `json:"template_version"`
	Locale          string    `json:"locale"`
	PageBudget      int       `json:"page_budget"`
	Picks           Picks     `json:"picks"`
	Version         int       `json:"version"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// DraftInput is a PUT: everything in the draft the user sets.
type DraftInput struct {
	Template        string `json:"template"`
	TemplateVersion *int   `json:"template_version"`
	Locale          string `json:"locale"`
	PageBudget      int    `json:"page_budget"`
	Picks           Picks  `json:"picks"`
}

// MatchInfo names the run that ranked a CV.
type MatchInfo struct {
	RunID uuid.UUID `json:"run_id"`
	Stale bool      `json:"stale"`
}

// Preview is a draft with its built content, before fitting.
type Preview struct {
	Draft       Draft      `json:"draft"`
	Template    Template   `json:"template"`
	Content     Content    `json:"content"`
	ContentHash string     `json:"content_hash"`
	Trim        []string   `json:"trim"`
	Available   Available  `json:"available"`
	Match       *MatchInfo `json:"match"`
}

// ExportInput creates a snapshot of what the client fitted.
type ExportInput struct {
	DraftVersion int `json:"draft_version"`
	// ContentHash is the preview's; a different one means the library or
	// draft changed since, and the client must fit again.
	ContentHash  string   `json:"content_hash"`
	Dropped      []string `json:"dropped"`
	Pages        int      `json:"pages"`
	ClientSHA256 *string  `json:"client_sha256,omitempty"`
}

type Export struct {
	ID              uuid.UUID       `json:"id"`
	JobID           uuid.UUID       `json:"job_id"`
	MatchRunID      *uuid.UUID      `json:"match_run_id"`
	Template        string          `json:"template"`
	TemplateVersion int             `json:"template_version"`
	Locale          string          `json:"locale"`
	DocumentDate    string          `json:"document_date"`
	PageBudget      int             `json:"page_budget"`
	Pages           int             `json:"pages"`
	Overflow        bool            `json:"overflow"`
	ContentHash     string          `json:"content_hash"`
	ClientSHA256    *string         `json:"client_sha256"`
	Dropped         []string        `json:"dropped"`
	CreatedAt       time.Time       `json:"created_at"`
	Content         json.RawMessage `json:"content,omitempty"`
}

func invalid(field, msg string) error {
	return &library.ValidationError{Fields: []library.FieldError{{Field: field, Message: msg}}}
}

func translate(e error) error {
	if errors.Is(e, pgx.ErrNoRows) {
		return library.ErrNotFound
	}
	return e
}

const draftSQL = `SELECT job_id,template,template_version,locale,page_budget,picks,version,updated_at FROM cv_draft WHERE user_id=$1 AND job_id=$2`

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	var picks []byte
	if e := row.Scan(&d.JobID, &d.Template, &d.TemplateVersion, &d.Locale, &d.PageBudget, &picks, &d.Version, &d.UpdatedAt); e != nil {
		return d, e
	}
	d.Picks = Picks{Include: []string{}, Exclude: []string{}}
	return d, json.Unmarshal(picks, &d.Picks)
}

// draft reads the job's draft, creating it on first use.
func draft(ctx context.Context, tx pgx.Tx, u, job uuid.UUID, lock bool) (Draft, error) {
	if _, e := tx.Exec(ctx, `INSERT INTO cv_draft(user_id,job_id) SELECT user_id,id FROM job WHERE user_id=$1 AND id=$2 ON CONFLICT DO NOTHING`, u, job); e != nil {
		return Draft{}, e
	}
	sql := draftSQL
	if lock {
		sql += " FOR UPDATE"
	}
	return scanDraft(tx.QueryRow(ctx, sql, u, job))
}

const librarySQL = `SELECT jsonb_build_object(
 'profile',COALESCE((SELECT jsonb_build_object('name',full_name,'headline',headline,'email',email,'phone',phone,'location',location,'links',links,'summary',summary) FROM profile WHERE user_id=$1),'{}'),
 'evidence',COALESCE((SELECT jsonb_agg(jsonb_build_object(
  'id',e.id,'parent_id',e.parent_id,'kind',e.kind,'category',e.category,'form',e.form,'title',e.title,
  'organization',COALESCE(o.name,''),'summary',e.summary,'location',e.location,'url',e.url,
  'start_date',e.start_date,'start_precision',e.start_precision,'end_date',e.end_date,'end_precision',e.end_precision,
  'is_current',e.is_current,'auto_include',e.auto_include,'details',e.details,
  'highlights',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',h.id,'text',h.text,'auto_include',h.auto_include) ORDER BY h.position,h.id)
   FROM highlight h WHERE h.user_id=e.user_id AND h.evidence_id=e.id),'[]')
 ) ORDER BY e.position,e.id)
 FROM evidence e LEFT JOIN organization o ON o.user_id=e.user_id AND o.id=e.organization_id
 WHERE e.user_id=$1 AND e.archived_at IS NULL),'[]'),
 'skills',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'name',name,'type',type,'proficiency',proficiency) ORDER BY normalized_name,id)
  FROM skill WHERE user_id=$1),'[]'))`

func loadLibrary(ctx context.Context, tx pgx.Tx, u uuid.UUID) (Library, error) {
	var raw []byte
	var lib Library
	if e := tx.QueryRow(ctx, librarySQL, u).Scan(&raw); e != nil {
		return lib, e
	}
	return lib, json.Unmarshal(raw, &lib)
}

// build makes a draft's preview, dated today.
func (s *Service) build(ctx context.Context, tx pgx.Tx, u uuid.UUID, d Draft) (Preview, error) {
	m, ok := manifest(d.Template, d.TemplateVersion)
	if !ok {
		return Preview{}, invalid("template", "no such template version")
	}
	lib, e := loadLibrary(ctx, tx, u)
	if e != nil {
		return Preview{}, e
	}
	run, ok, e := match.LatestCompleted(ctx, tx, u, d.JobID)
	if e != nil {
		return Preview{}, e
	}
	var ranking *match.Result
	var mi *MatchInfo
	if ok {
		ranking = &run.Result
		mi = &MatchInfo{run.ID, run.Stale}
	}
	locale := d.Locale
	if !slices.Contains(m.Locales, locale) {
		locale = m.Locales[0]
	}
	b := Build(lib, ranking, d.Picks, locale, s.Now().UTC())
	return Preview{Draft: d, Template: info(m), Content: b.Content, ContentHash: b.Content.Hash(), Trim: b.Trim, Available: b.Available, Match: mi}, nil
}

func (s *Service) Preview(ctx context.Context, u, job uuid.UUID) (Preview, error) {
	var p Preview
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		d, e := draft(ctx, tx, u, job, false)
		if e != nil {
			return e
		}
		p, e = s.build(ctx, tx, u, d)
		return e
	})
	return p, translate(e)
}

func validPicks(field string, ids []string) error {
	if len(ids) > 1000 {
		return invalid(field, "at most 1000 items")
	}
	for _, id := range ids {
		if _, e := uuid.Parse(id); e != nil {
			return invalid(field, "must be evidence or highlight IDs")
		}
	}
	return nil
}

func (in DraftInput) validate() error {
	m, ok := manifest(in.Template, in.TemplateVersion)
	switch {
	case !ok:
		return invalid("template", "no such template version")
	case !slices.Contains(m.Locales, in.Locale):
		return invalid("locale", "the template supports "+strings.Join(m.Locales, ", "))
	case in.PageBudget < 1 || in.PageBudget > 3:
		return invalid("page_budget", "must be 1 to 3")
	}
	if e := validPicks("picks.include", in.Picks.Include); e != nil {
		return e
	}
	if e := validPicks("picks.exclude", in.Picks.Exclude); e != nil {
		return e
	}
	for _, id := range in.Picks.Include {
		if slices.Contains(in.Picks.Exclude, id) {
			return invalid("picks", "an item can't be both included and excluded")
		}
	}
	return nil
}

// SaveDraft replaces the draft's settings, if version is still current.
func (s *Service) SaveDraft(ctx context.Context, u, job uuid.UUID, version int, in DraftInput) (Preview, error) {
	if in.Picks.Include == nil {
		in.Picks.Include = []string{}
	}
	if in.Picks.Exclude == nil {
		in.Picks.Exclude = []string{}
	}
	if e := in.validate(); e != nil {
		return Preview{}, e
	}
	var p Preview
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		d, e := draft(ctx, tx, u, job, true)
		if e != nil {
			return e
		}
		if d.Version != version {
			return library.ErrVersionMismatch
		}
		picks, _ := json.Marshal(in.Picks)
		d, e = scanDraft(tx.QueryRow(ctx, `UPDATE cv_draft SET template=$3,template_version=$4,locale=$5,page_budget=$6,picks=$7,version=version+1,updated_at=now()
 WHERE user_id=$1 AND job_id=$2 RETURNING job_id,template,template_version,locale,page_budget,picks,version,updated_at`,
			u, job, in.Template, in.TemplateVersion, in.Locale, in.PageBudget, string(picks)))
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO outbox(id,user_id,topic,payload) VALUES($1,$2,'cv_draft.updated',jsonb_build_object('id',$3::uuid,'version',$4::int))`,
			uuid.Must(uuid.NewV7()), u, job, d.Version); e != nil {
			return e
		}
		p, e = s.build(ctx, tx, u, d)
		return e
	})
	return p, translate(e)
}

const exportColumns = `id,job_id,match_run_id,template,template_version,locale,to_char(document_date,'YYYY-MM-DD'),page_budget,pages,overflow,content_hash,client_sha256,dropped,created_at`

func scanExport(row pgx.Row, withContent bool) (Export, error) {
	var x Export
	var dropped []byte
	dest := []any{&x.ID, &x.JobID, &x.MatchRunID, &x.Template, &x.TemplateVersion, &x.Locale, &x.DocumentDate, &x.PageBudget, &x.Pages, &x.Overflow, &x.ContentHash, &x.ClientSHA256, &dropped, &x.CreatedAt}
	if withContent {
		dest = append(dest, &x.Content)
	}
	if e := row.Scan(dest...); e != nil {
		return x, e
	}
	return x, json.Unmarshal(dropped, &x.Dropped)
}

// CreateExport stores the snapshot the client fitted: the draft's content,
// rebuilt here from the library, without the dropped items. key makes
// retries return the first result.
func (s *Service) CreateExport(ctx context.Context, u, job uuid.UUID, key string, in ExportInput) (Export, error) {
	if key == "" {
		key = uuid.NewString()
	}
	if len(key) > 255 {
		return Export{}, invalid("Idempotency-Key", "must be 1 to 255 characters")
	}
	if in.Pages < 1 || in.Pages > 50 {
		return Export{}, invalid("pages", "must be 1 to 50")
	}
	if in.Dropped == nil {
		in.Dropped = []string{}
	}
	if in.ClientSHA256 != nil {
		b, err := hex.DecodeString(*in.ClientSHA256)
		if err != nil || len(b) != 32 {
			return Export{}, invalid("client_sha256", "must be a SHA-256 hex digest")
		}
	}
	var out Export
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		// Serialize retries before looking up the key, including requests
		// that arrive while the first export is still being committed.
		if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, u.String()+":"+key); e != nil {
			return e
		}
		x, e := scanExport(tx.QueryRow(ctx, `SELECT `+exportColumns+`,content FROM cv_export WHERE user_id=$1 AND idempotency_key=$2`, u, key), true)
		if e == nil {
			if x.JobID != job {
				return &library.ConflictError{Message: "idempotency key belongs to another job"}
			}
			out = x
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		d, e := draft(ctx, tx, u, job, true)
		if e != nil {
			return e
		}
		if d.Version != in.DraftVersion {
			return library.ErrVersionMismatch
		}
		p, e := s.build(ctx, tx, u, d)
		if e != nil {
			return e
		}
		if p.ContentHash != in.ContentHash {
			return &library.ConflictError{Message: "the CV changed since it was previewed; reload it and export again"}
		}
		seen := map[string]bool{}
		for _, id := range in.Dropped {
			if seen[id] || !slices.Contains(p.Trim, id) {
				return invalid("dropped", "only optional items can be dropped, each once")
			}
			seen[id] = true
		}
		fitted := Apply(p.Content, in.Dropped)
		content, _ := json.Marshal(fitted)
		var storedBytes int
		if e = tx.QueryRow(ctx, `SELECT octet_length($1::jsonb::text)`, string(content)).Scan(&storedBytes); e != nil {
			return e
		}
		if storedBytes > MaxContentBytes {
			return invalid("content", "the CV is too large to store")
		}
		var run *uuid.UUID
		if p.Match != nil {
			run = &p.Match.RunID
		}
		dropped, _ := json.Marshal(in.Dropped)
		id := uuid.Must(uuid.NewV7())
		out, e = scanExport(tx.QueryRow(ctx, `INSERT INTO cv_export(id,user_id,job_id,idempotency_key,match_run_id,template,template_version,locale,document_date,page_budget,pages,overflow,content,content_hash,dropped,client_sha256)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::date,$10,$11,$12,$13,$14,$15,$16) RETURNING `+exportColumns+`,content`,
			id, u, job, key, run, p.Template.Name, p.Template.Version, p.Content.Locale, p.Content.Date, d.PageBudget, in.Pages, in.Pages > d.PageBudget, string(content), fitted.Hash(), string(dropped), in.ClientSHA256), true)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO outbox(id,user_id,topic,payload) VALUES($1,$2,'cv_export.created',jsonb_build_object('id',$3::uuid,'job_id',$4::uuid))`,
			uuid.Must(uuid.NewV7()), u, id, job)
		return e
	})
	return out, translate(e)
}

// Exports lists a job's snapshots, newest first, without their content.
func (s *Service) Exports(ctx context.Context, u, job uuid.UUID) ([]Export, error) {
	out := []Export{}
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var exists bool
		if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM job WHERE user_id=$1 AND id=$2)`, u, job).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return library.ErrNotFound
		}
		rows, e := tx.Query(ctx, `SELECT `+exportColumns+` FROM cv_export WHERE user_id=$1 AND job_id=$2 ORDER BY created_at DESC,id DESC LIMIT 100`, u, job)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			x, e := scanExport(rows, false)
			if e != nil {
				return e
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, translate(e)
}

// Export reads one snapshot with its content.
func (s *Service) Export(ctx context.Context, u, id uuid.UUID) (Export, error) {
	var x Export
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var e error
		x, e = scanExport(tx.QueryRow(ctx, `SELECT `+exportColumns+`,content FROM cv_export WHERE user_id=$1 AND id=$2`, u, id), true)
		return e
	})
	return x, translate(e)
}
