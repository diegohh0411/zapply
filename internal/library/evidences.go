package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/store/db"
)

// ListFilter selects evidence for List. Zero values mean "any".
type ListFilter struct {
	Category        Category
	Form            Form
	Kind            string
	ParentID        *uuid.UUID
	TopLevel        bool
	IncludeArchived bool
	Cursor          string
	Limit           int
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

type cursor struct {
	Category Category  `json:"c"`
	Position string    `json:"p"`
	ID       uuid.UUID `json:"i"`
}

// ListEvidences returns full documents ordered by category, then position,
// and the cursor for the next page ("" when there is none).
func (s *Service) ListEvidences(ctx context.Context, uid uuid.UUID, f ListFilter) ([]Evidence, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	p := db.ListEvidencesParams{
		UserID: uid, ParentID: f.ParentID, TopLevel: f.TopLevel,
		IncludeArchived: f.IncludeArchived, RowLimit: int32(limit + 1),
	}
	if f.Category != "" {
		c := db.EvidenceCategory(f.Category)
		p.Category = &c
	}
	if f.Form != "" {
		fm := db.EvidenceForm(f.Form)
		p.Form = &fm
	}
	if f.Kind != "" {
		p.Kind = &f.Kind
	}
	if f.Cursor != "" {
		var c cursor
		raw, err := base64.RawURLEncoding.DecodeString(f.Cursor)
		if err != nil || json.Unmarshal(raw, &c) != nil || c.Category == "" {
			return nil, "", invalid("cursor", "is invalid")
		}
		cat := db.EvidenceCategory(c.Category)
		p.AfterCategory, p.AfterPosition, p.AfterID = &cat, &c.Position, &c.ID
	}

	var out []Evidence
	var next string
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		rows, err := q.ListEvidences(ctx, p)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			rows = rows[:limit]
			last := rows[limit-1]
			b, _ := json.Marshal(cursor{Category(last.Category), last.Position, last.ID})
			next = base64.RawURLEncoding.EncodeToString(b)
		}
		out, err = loadDocuments(ctx, q, uid, rows)
		return err
	})
	return out, next, err
}

// GetEvidence returns one document with summaries of its children.
func (s *Service) GetEvidence(ctx context.Context, uid, id uuid.UUID) (Evidence, error) {
	var out Evidence
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		var err error
		out, err = getDocument(ctx, q, uid, id)
		return err
	})
	return out, err
}

func getDocument(ctx context.Context, q *db.Queries, uid, id uuid.UUID) (Evidence, error) {
	row, err := q.GetEvidence(ctx, db.GetEvidenceParams{UserID: uid, ID: id})
	if err != nil {
		return Evidence{}, notFound(err)
	}
	docs, err := loadDocuments(ctx, q, uid, []db.Evidence{row})
	if err != nil {
		return Evidence{}, err
	}
	doc := docs[0]
	children, err := q.ListChildren(ctx, db.ListChildrenParams{UserID: uid, ParentIds: []uuid.UUID{id}})
	if err != nil {
		return Evidence{}, err
	}
	doc.Children = make([]ChildSummary, len(children))
	for i, c := range children {
		doc.Children[i] = ChildSummary{ID: c.ID, Kind: c.Kind, Form: Form(c.Form), Title: c.Title, Archived: c.ArchivedAt != nil}
	}
	return doc, nil
}

// Idempotency identifies a retried create (the Idempotency-Key header).
type Idempotency struct {
	Key         string
	RequestHash []byte
}

// RequestHash fingerprints a request body for Idempotency.
func RequestHash(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

// CreateEvidence creates a document. With idem set, a retry of the same
// request returns the evidence it created (replayed = true) instead of a
// duplicate; the same key with a different body is rejected.
func (s *Service) CreateEvidence(ctx context.Context, uid uuid.UUID, in EvidenceInput, idem *Idempotency) (doc Evidence, replayed bool, err error) {
	v, errs := validate(in)
	if len(errs) > 0 {
		return Evidence{}, false, &ValidationError{Fields: errs}
	}
	err = s.tx(ctx, uid, func(q *db.Queries) error {
		if idem != nil {
			id, done, err := claimKey(ctx, q, uid, *idem)
			if err != nil {
				return err
			}
			if done {
				replayed = true
				doc, err = getDocument(ctx, q, uid, id)
				if errors.Is(err, ErrNotFound) {
					return &ConflictError{Message: "the evidence created with this Idempotency-Key no longer exists"}
				}
				return err
			}
		}

		var parent *db.Evidence
		if in.ParentID != nil {
			p, err := q.GetEvidenceForUpdate(ctx, db.GetEvidenceForUpdateParams{UserID: uid, ID: *in.ParentID})
			if errors.Is(err, pgx.ErrNoRows) {
				return invalid("parent_id", "evidence not found")
			}
			if err != nil {
				return err
			}
			parent = &p
			if v.in.Category == "" {
				v.in.Category = Category(p.Category)
			}
			if err := checkNesting(v.kind, v.in.Category, p, nil); err != nil {
				return err
			}
		}

		var last string
		var err error
		if parent != nil {
			last, err = q.LastChildPosition(ctx, db.LastChildPositionParams{UserID: uid, ParentID: in.ParentID})
		} else {
			last, err = q.LastTopLevelPosition(ctx, db.LastTopLevelPositionParams{UserID: uid, Category: db.EvidenceCategory(v.in.Category)})
		}
		if err != nil {
			return err
		}
		pos, err := PositionBetween(last, "")
		if err != nil {
			return err
		}

		doc = Evidence{ID: newID(), ParentID: in.ParentID, Position: pos, Version: 1}
		if err := writeDocument(ctx, q, uid, &doc, v, false); err != nil {
			return err
		}
		if idem != nil {
			if err := q.CompleteIdempotencyKey(ctx, db.CompleteIdempotencyKeyParams{
				UserID: uid, Key: idem.Key, Status: 201, Body: doc.ID[:],
			}); err != nil {
				return err
			}
		}
		if err := emit(ctx, q, uid, "evidence.created", doc.ID, doc.Version); err != nil {
			return err
		}
		doc, err = getDocument(ctx, q, uid, doc.ID)
		return err
	})
	return doc, replayed, err
}

// claimKey records an Idempotency-Key, or reports the evidence a previous
// request with that key created. A concurrent request with the same key
// waits on the unique index until the first one commits.
func claimKey(ctx context.Context, q *db.Queries, uid uuid.UUID, idem Idempotency) (uuid.UUID, bool, error) {
	n, err := q.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{UserID: uid, Key: idem.Key, RequestHash: idem.RequestHash})
	if err != nil {
		return uuid.Nil, false, err
	}
	if n == 1 {
		return uuid.Nil, false, nil
	}
	prev, err := q.GetIdempotencyKey(ctx, db.GetIdempotencyKeyParams{UserID: uid, Key: idem.Key})
	if err != nil {
		return uuid.Nil, false, err
	}
	if !bytes.Equal(prev.RequestHash, idem.RequestHash) {
		return uuid.Nil, false, invalid("Idempotency-Key", "was already used with a different request")
	}
	id, err := uuid.FromBytes(prev.Body)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("stored idempotent response: %w", err)
	}
	return id, true, nil
}

// UpdateEvidence replaces a document if its version is still ifVersion.
// Highlights sent with an id keep that id (and so their cached scores).
func (s *Service) UpdateEvidence(ctx context.Context, uid, id uuid.UUID, ifVersion int, in EvidenceInput) (Evidence, error) {
	in.ParentID = nil
	cur := Evidence{ID: id}
	var doc Evidence
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		row, err := q.GetEvidenceForUpdate(ctx, db.GetEvidenceForUpdateParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		if int(row.Version) != ifVersion {
			return ErrVersionMismatch
		}
		// A child's category is its parent's; validate as a child.
		in.ParentID = row.ParentID
		if row.ParentID != nil && in.Category == "" {
			in.Category = Category(row.Category)
		}
		v, errs := validate(in)
		if len(errs) > 0 {
			return &ValidationError{Fields: errs}
		}
		if row.ParentID != nil {
			p, err := q.GetEvidence(ctx, db.GetEvidenceParams{UserID: uid, ID: *row.ParentID})
			if err != nil {
				return err
			}
			if err := checkNesting(v.kind, v.in.Category, p, &row); err != nil {
				return err
			}
		}
		children, err := q.CountChildren(ctx, db.CountChildrenParams{UserID: uid, ParentID: &id})
		if err != nil {
			return err
		}
		if children > 0 && v.kind.Form != Engagement {
			return invalid("kind", "evidence with children must stay an engagement")
		}

		existing, err := q.ListHighlights(ctx, db.ListHighlightsParams{UserID: uid, EvidenceIds: []uuid.UUID{id}})
		if err != nil {
			return err
		}
		known := map[uuid.UUID]bool{}
		for _, h := range existing {
			known[h.ID] = true
		}
		for i, h := range v.in.Highlights {
			if h.ID != nil && !known[*h.ID] {
				return invalid(fmt.Sprintf("highlights[%d].id", i), "is not a highlight of this evidence")
			}
		}

		cur.ParentID, cur.Position, cur.Version = row.ParentID, row.Position, ifVersion
		if err := writeDocument(ctx, q, uid, &cur, v, true); err != nil {
			return err
		}
		if Category(row.Category) != v.in.Category && children > 0 {
			moved, err := q.SetChildrenCategory(ctx, db.SetChildrenCategoryParams{
				UserID: uid, ParentID: &id, Category: db.EvidenceCategory(v.in.Category),
			})
			if err != nil {
				return err
			}
			if err := rehash(ctx, q, uid, moved); err != nil {
				return err
			}
		}
		if err := emit(ctx, q, uid, "evidence.updated", id, cur.Version); err != nil {
			return err
		}
		doc, err = getDocument(ctx, q, uid, id)
		return err
	})
	return doc, err
}

// checkNesting applies the nesting rules for a child under parent. self is
// the child's current row when it already exists.
func checkNesting(k *Kind, category Category, parent db.Evidence, self *db.Evidence) error {
	switch {
	case parent.ParentID != nil:
		return invalid("parent_id", "evidence nests at most two levels")
	case Form(parent.Form) != Engagement:
		return invalid("parent_id", "only engagements can have children")
	case k.Form != Output && k.Form != Recognition && k.Name != "assignment":
		return invalid("kind", fmt.Sprintf("kind %s cannot be nested", k.Name))
	case category != Category(parent.Category):
		return invalid("category", fmt.Sprintf("a child must have its parent's category (%s)", parent.Category))
	case self != nil && self.ID == parent.ID:
		return invalid("parent_id", "evidence cannot be its own parent")
	}
	return nil
}

// writeDocument stores a validated document: the evidence row, its skills,
// and its highlights (replacing the previous set on update).
func writeDocument(ctx context.Context, q *db.Queries, uid uuid.UUID, doc *Evidence, v validated, update bool) error {
	in := v.in
	var orgName string
	if in.OrganizationID != nil {
		org, err := q.GetOrganization(ctx, db.GetOrganizationParams{UserID: uid, ID: *in.OrganizationID})
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("organization_id", "organization not found")
		}
		if err != nil {
			return err
		}
		orgName = org.Name
	}

	names := slices.Clone(in.Skills)
	for _, h := range in.Highlights {
		names = append(names, h.Skills...)
	}
	skills, err := ensureSkills(ctx, q, uid, names)
	if err != nil {
		return err
	}
	resolve := func(list []string) ([]string, []uuid.UUID) {
		display := make([]string, len(list))
		ids := make([]uuid.UUID, len(list))
		for i, n := range list {
			s := skills[NormalizeName(n)]
			display[i], ids[i] = s.Name, s.ID
		}
		return display, ids
	}

	doc.OrganizationID = in.OrganizationID
	doc.Category, doc.Form, doc.Kind = in.Category, v.kind.Form, v.kind.Name
	doc.Title, doc.Summary, doc.Location, doc.URL = in.Title, in.Summary, in.Location, in.URL
	doc.Start, doc.End, doc.IsCurrent = v.start, v.end, in.IsCurrent
	doc.Details, doc.AutoInclude = in.Details, *in.AutoInclude
	var skillIDs []uuid.UUID
	doc.Skills, skillIDs = resolve(in.Skills)

	positions := PositionsAfter("", len(in.Highlights))
	doc.Highlights = make([]Highlight, len(in.Highlights))
	highlightSkillIDs := make([][]uuid.UUID, len(in.Highlights))
	for i, h := range in.Highlights {
		id := newID()
		if h.ID != nil {
			id = *h.ID
		}
		hl := Highlight{ID: id, Text: h.Text, MetricValue: h.MetricValue, MetricUnit: h.MetricUnit, AutoInclude: *h.AutoInclude}
		hl.Skills, highlightSkillIDs[i] = resolve(h.Skills)
		hl.ContentHash = highlightHash(hl)
		doc.Highlights[i] = hl
	}
	doc.ContentHash = evidenceHash(*doc, orgName)

	details, err := json.Marshal(doc.Details)
	if err != nil {
		return err
	}
	sd, sp := dateColumns(doc.Start)
	ed, ep := dateColumns(doc.End)
	if update {
		ver, err := q.UpdateEvidence(ctx, db.UpdateEvidenceParams{
			UserID: uid, ID: doc.ID, Version: int32(doc.Version), OrganizationID: doc.OrganizationID,
			Category: db.EvidenceCategory(doc.Category), Form: db.EvidenceForm(doc.Form), Kind: doc.Kind,
			Title: doc.Title, Summary: doc.Summary, Location: doc.Location, Url: doc.URL,
			StartDate: sd, StartPrecision: sp, EndDate: ed, EndPrecision: ep, IsCurrent: doc.IsCurrent,
			Details: details, AutoInclude: doc.AutoInclude, ContentHash: doc.ContentHash,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrVersionMismatch
		}
		if err != nil {
			return err
		}
		doc.Version = int(ver)
		if err := q.ClearEvidenceSkills(ctx, db.ClearEvidenceSkillsParams{UserID: uid, EvidenceID: doc.ID}); err != nil {
			return err
		}
		keep := make([]uuid.UUID, len(doc.Highlights))
		for i, h := range doc.Highlights {
			keep[i] = h.ID
		}
		if err := q.DeleteHighlightsExcept(ctx, db.DeleteHighlightsExceptParams{UserID: uid, EvidenceID: doc.ID, KeepIds: keep}); err != nil {
			return err
		}
	} else {
		if err := q.InsertEvidence(ctx, db.InsertEvidenceParams{
			ID: doc.ID, UserID: uid, ParentID: doc.ParentID, OrganizationID: doc.OrganizationID,
			Category: db.EvidenceCategory(doc.Category), Form: db.EvidenceForm(doc.Form), Kind: doc.Kind,
			Title: doc.Title, Summary: doc.Summary, Location: doc.Location, Url: doc.URL,
			StartDate: sd, StartPrecision: sp, EndDate: ed, EndPrecision: ep, IsCurrent: doc.IsCurrent,
			Details: details, AutoInclude: doc.AutoInclude, Position: doc.Position, ContentHash: doc.ContentHash,
		}); err != nil {
			return err
		}
	}
	if err := q.LinkEvidenceSkills(ctx, db.LinkEvidenceSkillsParams{UserID: uid, EvidenceID: doc.ID, SkillIds: skillIDs}); err != nil {
		return err
	}
	for i, h := range doc.Highlights {
		n, err := q.UpsertHighlight(ctx, db.UpsertHighlightParams{
			ID: h.ID, UserID: uid, EvidenceID: doc.ID, Text: h.Text, MetricValue: h.MetricValue,
			MetricUnit: h.MetricUnit, AutoInclude: h.AutoInclude, Position: positions[i], ContentHash: h.ContentHash,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return invalid(fmt.Sprintf("highlights[%d].id", i), "is not a highlight of this evidence")
		}
		if err := q.ClearHighlightSkills(ctx, db.ClearHighlightSkillsParams{UserID: uid, HighlightID: h.ID}); err != nil {
			return err
		}
		if err := q.LinkHighlightSkills(ctx, db.LinkHighlightSkillsParams{UserID: uid, HighlightID: h.ID, SkillIds: highlightSkillIDs[i]}); err != nil {
			return err
		}
	}
	return nil
}

// MoveInput places evidence under ParentID (nil: top level), after AfterID
// or before BeforeID among its new siblings, or last when both are nil.
type MoveInput struct {
	ParentID *uuid.UUID
	AfterID  *uuid.UUID
	BeforeID *uuid.UUID
}

// MoveEvidence re-parents and/or reorders. Moving under a parent adopts the
// parent's category.
func (s *Service) MoveEvidence(ctx context.Context, uid, id uuid.UUID, m MoveInput) (Evidence, error) {
	if m.AfterID != nil && m.BeforeID != nil {
		return Evidence{}, invalid("after_id", "set after_id or before_id, not both")
	}
	var doc Evidence
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		row, err := q.GetEvidenceForUpdate(ctx, db.GetEvidenceForUpdateParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		k, _ := LookupKind(row.Kind)
		category := Category(row.Category)
		if m.ParentID != nil {
			p, err := q.GetEvidenceForUpdate(ctx, db.GetEvidenceForUpdateParams{UserID: uid, ID: *m.ParentID})
			if errors.Is(err, pgx.ErrNoRows) {
				return invalid("parent_id", "evidence not found")
			}
			if err != nil {
				return err
			}
			if *m.ParentID == id {
				return invalid("parent_id", "evidence cannot be its own parent")
			}
			n, err := q.CountChildren(ctx, db.CountChildrenParams{UserID: uid, ParentID: &id})
			if err != nil {
				return err
			}
			if n > 0 {
				return invalid("parent_id", "evidence with children cannot be nested")
			}
			category = Category(p.Category)
			if err := checkNesting(k, category, p, &row); err != nil {
				return err
			}
		} else if k.ChildOnly {
			return invalid("parent_id", fmt.Sprintf("kind %s must be nested under an engagement", k.Name))
		}

		siblings, err := q.ListSiblings(ctx, db.ListSiblingsParams{UserID: uid, ParentID: m.ParentID, Category: db.EvidenceCategory(category)})
		if err != nil {
			return err
		}
		siblings = slices.DeleteFunc(siblings, func(r db.ListSiblingsRow) bool { return r.ID == id })
		pos, err := placeAmong(siblings, m)
		if err != nil {
			return err
		}

		if !k.Allows(category) {
			return invalid("category", fmt.Sprintf("kind %s does not allow %s", k.Name, category))
		}
		ver, err := q.MoveEvidence(ctx, db.MoveEvidenceParams{
			UserID: uid, ID: id, ParentID: m.ParentID, Position: pos, Category: db.EvidenceCategory(category),
		})
		if err != nil {
			return err
		}
		if category != Category(row.Category) {
			if err := rehash(ctx, q, uid, []uuid.UUID{id}); err != nil {
				return err
			}
		}
		if err := emit(ctx, q, uid, "evidence.moved", id, int(ver)); err != nil {
			return err
		}
		doc, err = getDocument(ctx, q, uid, id)
		return err
	})
	return doc, err
}

func placeAmong(siblings []db.ListSiblingsRow, m MoveInput) (string, error) {
	idx := func(id uuid.UUID) int {
		return slices.IndexFunc(siblings, func(r db.ListSiblingsRow) bool { return r.ID == id })
	}
	var a, b string
	switch {
	case m.AfterID != nil:
		i := idx(*m.AfterID)
		if i < 0 {
			return "", invalid("after_id", "is not a sibling at the destination")
		}
		a = siblings[i].Position
		if i+1 < len(siblings) {
			b = siblings[i+1].Position
		}
	case m.BeforeID != nil:
		i := idx(*m.BeforeID)
		if i < 0 {
			return "", invalid("before_id", "is not a sibling at the destination")
		}
		b = siblings[i].Position
		if i > 0 {
			a = siblings[i-1].Position
		}
	default:
		if n := len(siblings); n > 0 {
			a = siblings[n-1].Position
		}
	}
	if a == b && a != "" {
		// Equal keys can only come from legacy ties; append after both.
		b = ""
	}
	return PositionBetween(a, b)
}

// SetArchived archives or restores evidence. Archived evidence is hidden
// from lists and never matched.
func (s *Service) SetArchived(ctx context.Context, uid, id uuid.UUID, archived bool) (Evidence, error) {
	var doc Evidence
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		ver, err := q.SetEvidenceArchived(ctx, db.SetEvidenceArchivedParams{UserID: uid, ID: id, Archived: archived})
		if err != nil {
			return notFound(err)
		}
		topic := "evidence.archived"
		if !archived {
			topic = "evidence.unarchived"
		}
		if err := emit(ctx, q, uid, topic, id, int(ver)); err != nil {
			return err
		}
		doc, err = getDocument(ctx, q, uid, id)
		return err
	})
	return doc, err
}

// DeleteEvidence deletes evidence. With children it fails unless cascade is
// set, in which case the children go too.
func (s *Service) DeleteEvidence(ctx context.Context, uid, id uuid.UUID, cascade bool) error {
	return s.tx(ctx, uid, func(q *db.Queries) error {
		if _, err := q.GetEvidenceForUpdate(ctx, db.GetEvidenceForUpdateParams{UserID: uid, ID: id}); err != nil {
			return notFound(err)
		}
		children, err := q.ListChildren(ctx, db.ListChildrenParams{UserID: uid, ParentIds: []uuid.UUID{id}})
		if err != nil {
			return err
		}
		if len(children) > 0 && !cascade {
			return &ConflictError{Message: fmt.Sprintf("evidence has %d children; delete them first or pass cascade=true", len(children))}
		}
		if _, err := q.DeleteEvidence(ctx, db.DeleteEvidenceParams{UserID: uid, ID: id}); err != nil {
			return err
		}
		for _, c := range children {
			if err := emit(ctx, q, uid, "evidence.deleted", c.ID, 0); err != nil {
				return err
			}
		}
		return emit(ctx, q, uid, "evidence.deleted", id, 0)
	})
}

// loadDocuments assembles documents (skills, highlights) for evidence rows,
// preserving their order.
func loadDocuments(ctx context.Context, q *db.Queries, uid uuid.UUID, rows []db.Evidence) ([]Evidence, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	eskills, err := q.ListEvidenceSkills(ctx, db.ListEvidenceSkillsParams{UserID: uid, EvidenceIds: ids})
	if err != nil {
		return nil, err
	}
	hrows, err := q.ListHighlights(ctx, db.ListHighlightsParams{UserID: uid, EvidenceIds: ids})
	if err != nil {
		return nil, err
	}
	hskills, err := q.ListHighlightSkills(ctx, db.ListHighlightSkillsParams{UserID: uid, EvidenceIds: ids})
	if err != nil {
		return nil, err
	}

	skillsByEvidence := map[uuid.UUID][]string{}
	for _, s := range eskills {
		skillsByEvidence[s.EvidenceID] = append(skillsByEvidence[s.EvidenceID], s.Name)
	}
	skillsByHighlight := map[uuid.UUID][]string{}
	for _, s := range hskills {
		skillsByHighlight[s.HighlightID] = append(skillsByHighlight[s.HighlightID], s.Name)
	}
	highlights := map[uuid.UUID][]Highlight{}
	for _, h := range hrows {
		highlights[h.EvidenceID] = append(highlights[h.EvidenceID], Highlight{
			ID: h.ID, Text: h.Text, MetricValue: h.MetricValue, MetricUnit: h.MetricUnit,
			AutoInclude: h.AutoInclude, Skills: orEmpty(skillsByHighlight[h.ID]), ContentHash: h.ContentHash,
		})
	}

	out := make([]Evidence, len(rows))
	for i, r := range rows {
		d := Evidence{
			ID: r.ID, ParentID: r.ParentID, OrganizationID: r.OrganizationID,
			Category: Category(r.Category), Form: Form(r.Form), Kind: r.Kind,
			Title: r.Title, Summary: r.Summary, Location: r.Location, URL: r.Url,
			Start: dateFromColumns(r.StartDate, r.StartPrecision), End: dateFromColumns(r.EndDate, r.EndPrecision),
			IsCurrent: r.IsCurrent, AutoInclude: r.AutoInclude, Position: r.Position,
			Version: int(r.Version), ContentHash: r.ContentHash, ArchivedAt: r.ArchivedAt,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			Skills: orEmpty(skillsByEvidence[r.ID]), Highlights: highlights[r.ID],
		}
		if d.Highlights == nil {
			d.Highlights = []Highlight{}
		}
		if err := json.Unmarshal(r.Details, &d.Details); err != nil {
			return nil, err
		}
		out[i] = d
	}
	return out, nil
}

// rehash recomputes content hashes after something they cover changed
// outside the document (an organization or skill rename, a merge).
func rehash(ctx context.Context, q *db.Queries, uid uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.GetEvidences(ctx, db.GetEvidencesParams{UserID: uid, Ids: ids})
	if err != nil {
		return err
	}
	docs, err := loadDocuments(ctx, q, uid, rows)
	if err != nil {
		return err
	}
	var orgIDs []uuid.UUID
	for _, d := range docs {
		if d.OrganizationID != nil {
			orgIDs = append(orgIDs, *d.OrganizationID)
		}
	}
	orgs, err := q.GetOrganizationNames(ctx, db.GetOrganizationNamesParams{UserID: uid, Ids: orgIDs})
	if err != nil {
		return err
	}
	orgName := map[uuid.UUID]string{}
	for _, o := range orgs {
		orgName[o.ID] = o.Name
	}
	for _, d := range docs {
		for i := range d.Highlights {
			h := &d.Highlights[i]
			if sum := highlightHash(*h); !bytes.Equal(sum, h.ContentHash) {
				h.ContentHash = sum
				if err := q.SetHighlightHash(ctx, db.SetHighlightHashParams{UserID: uid, ID: h.ID, ContentHash: sum}); err != nil {
					return err
				}
			}
		}
		var name string
		if d.OrganizationID != nil {
			name = orgName[*d.OrganizationID]
		}
		if sum := evidenceHash(d, name); !bytes.Equal(sum, d.ContentHash) {
			if err := q.SetEvidenceHash(ctx, db.SetEvidenceHashParams{UserID: uid, ID: d.ID, ContentHash: sum}); err != nil {
				return err
			}
		}
	}
	return nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func dateColumns(d *PartialDate) (*time.Time, *db.DatePrecision) {
	if d == nil {
		return nil, nil
	}
	t, p := d.Time, db.DatePrecision(d.Precision)
	return &t, &p
}

func dateFromColumns(t *time.Time, p *db.DatePrecision) *PartialDate {
	if t == nil || p == nil {
		return nil
	}
	return &PartialDate{Time: t.UTC(), Precision: Precision(*p)}
}
