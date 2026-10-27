package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/jobs"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/match"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/model/openrouter"
	"github.com/diegohh0411/zapply/internal/worker"
)

// server implements gen.StrictServerInterface. Handlers convert between the
// generated contract types and the library; errors are mapped centrally in
// writeError.
type server struct {
	deps           Deps
	lib            *library.Service
	jobs           *jobs.Service
	members        *membership.Service
	queue          *worker.Queue
	matcher        *match.Service
	decisionModels *openrouter.Client
}

var _ gen.StrictServerInterface = (*server)(nil)

func uid(ctx context.Context) uuid.UUID { return auth.UserID(ctx) }

func etag(version int) *string {
	s := `"` + strconv.Itoa(version) + `"`
	return &s
}

// parseIfMatch accepts the ETag returned by a read: "3" (quotes optional).
func parseIfMatch(v string) (int, error) {
	n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(v), `"`))
	if err != nil || n < 0 {
		return 0, badRequest("If-Match must be the ETag from a previous read")
	}
	return n, nil
}

func (s *server) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	me, err := s.deps.Store.GetUser(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	role, err := s.members.Role(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	usage, err := decodePlatform[gen.Usage](s.members.Usage(ctx, uid(ctx)))
	if err != nil {
		return nil, err
	}
	return gen.GetMe200JSONResponse{Id: me.ID, CreatedAt: me.CreatedAt, PlatformRole: gen.MePlatformRole(role), Membership: usage.Membership}, nil
}

func (s *server) ListKinds(context.Context, gen.ListKindsRequestObject) (gen.ListKindsResponseObject, error) {
	out := make(gen.ListKinds200JSONResponse, len(library.Kinds))
	for i := range library.Kinds {
		k := &library.Kinds[i]
		cats := make([]gen.Category, len(k.Categories))
		for j, c := range k.Categories {
			cats[j] = gen.Category(c)
		}
		out[i] = gen.Kind{
			Kind: k.Name, Form: gen.Form(k.Form), Categories: cats,
			ChildOnly: k.ChildOnly, DetailsSchema: k.JSONSchema(),
		}
	}
	return out, nil
}

// Profile

func (s *server) GetProfile(ctx context.Context, _ gen.GetProfileRequestObject) (gen.GetProfileResponseObject, error) {
	p, err := s.lib.GetProfile(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	return gen.GetProfile200JSONResponse{Body: toProfile(p), Headers: gen.GetProfile200ResponseHeaders{ETag: etag(p.Version)}}, nil
}

func (s *server) PutProfile(ctx context.Context, req gen.PutProfileRequestObject) (gen.PutProfileResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := library.Profile{
		FullName: deref(b.FullName), Headline: deref(b.Headline), Email: deref(b.Email), Phone: deref(b.Phone),
		Location: deref(b.Location), Summary: deref(b.Summary), AutoMatch: b.AutoMatch == nil || *b.AutoMatch,
	}
	if b.Links != nil {
		for _, l := range *b.Links {
			in.Links = append(in.Links, library.Link{Label: l.Label, URL: l.Url})
		}
	}
	p, err := s.lib.PutProfile(ctx, uid(ctx), ver, in)
	if err != nil {
		return nil, err
	}
	return gen.PutProfile200JSONResponse{Body: toProfile(p), Headers: gen.PutProfile200ResponseHeaders{ETag: etag(p.Version)}}, nil
}

func toProfile(p library.Profile) gen.Profile {
	out := gen.Profile{
		AutoMatch: &p.AutoMatch, FullName: p.FullName, Headline: p.Headline, Email: p.Email, Phone: p.Phone,
		Location: p.Location, Summary: p.Summary, Version: p.Version, Links: []gen.Link{},
	}
	for _, l := range p.Links {
		out.Links = append(out.Links, gen.Link{Label: l.Label, Url: l.URL})
	}
	if p.Version > 0 {
		t := p.UpdatedAt
		out.UpdatedAt = &t
	}
	return out
}

// Organizations

func (s *server) ListOrganizations(ctx context.Context, _ gen.ListOrganizationsRequestObject) (gen.ListOrganizationsResponseObject, error) {
	orgs, err := s.lib.ListOrganizations(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	out := make(gen.ListOrganizations200JSONResponse, len(orgs))
	for i, o := range orgs {
		out[i] = toOrganization(o)
	}
	return out, nil
}

func (s *server) GetOrganization(ctx context.Context, req gen.GetOrganizationRequestObject) (gen.GetOrganizationResponseObject, error) {
	o, err := s.lib.GetOrganization(ctx, uid(ctx), req.Id)
	if err != nil {
		return nil, err
	}
	return gen.GetOrganization200JSONResponse(toOrganization(o)), nil
}

func (s *server) CreateOrganization(ctx context.Context, req gen.CreateOrganizationRequestObject) (gen.CreateOrganizationResponseObject, error) {
	o, err := s.lib.CreateOrganization(ctx, uid(ctx), organizationInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.CreateOrganization201JSONResponse(toOrganization(o)), nil
}

func (s *server) UpdateOrganization(ctx context.Context, req gen.UpdateOrganizationRequestObject) (gen.UpdateOrganizationResponseObject, error) {
	o, err := s.lib.UpdateOrganization(ctx, uid(ctx), req.Id, organizationInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.UpdateOrganization200JSONResponse(toOrganization(o)), nil
}

func (s *server) DeleteOrganization(ctx context.Context, req gen.DeleteOrganizationRequestObject) (gen.DeleteOrganizationResponseObject, error) {
	if err := s.lib.DeleteOrganization(ctx, uid(ctx), req.Id); err != nil {
		return nil, err
	}
	return gen.DeleteOrganization204Response{}, nil
}

func organizationInput(b gen.OrganizationInput) library.OrganizationInput {
	in := library.OrganizationInput{Name: b.Name, Website: deref(b.Website), Location: deref(b.Location)}
	if b.Type != nil {
		in.Type = string(*b.Type)
	}
	return in
}

func toOrganization(o library.Organization) gen.Organization {
	return gen.Organization{
		Id: o.ID, Name: o.Name, Type: gen.OrganizationType(o.Type), Website: o.Website,
		Location: o.Location, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
}

// Skills

func (s *server) ListSkills(ctx context.Context, _ gen.ListSkillsRequestObject) (gen.ListSkillsResponseObject, error) {
	skills, err := s.lib.ListSkills(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	out := make(gen.ListSkills200JSONResponse, len(skills))
	for i, sk := range skills {
		out[i] = toSkill(sk)
	}
	return out, nil
}

func (s *server) CreateSkill(ctx context.Context, req gen.CreateSkillRequestObject) (gen.CreateSkillResponseObject, error) {
	sk, err := s.lib.CreateSkill(ctx, uid(ctx), skillInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.CreateSkill201JSONResponse(toSkill(sk)), nil
}

func (s *server) UpdateSkill(ctx context.Context, req gen.UpdateSkillRequestObject) (gen.UpdateSkillResponseObject, error) {
	sk, err := s.lib.UpdateSkill(ctx, uid(ctx), req.Id, skillInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.UpdateSkill200JSONResponse(toSkill(sk)), nil
}

func (s *server) DeleteSkill(ctx context.Context, req gen.DeleteSkillRequestObject) (gen.DeleteSkillResponseObject, error) {
	if err := s.lib.DeleteSkill(ctx, uid(ctx), req.Id); err != nil {
		return nil, err
	}
	return gen.DeleteSkill204Response{}, nil
}

func (s *server) MergeSkill(ctx context.Context, req gen.MergeSkillRequestObject) (gen.MergeSkillResponseObject, error) {
	sk, err := s.lib.MergeSkill(ctx, uid(ctx), req.Id, req.Body.Into)
	if err != nil {
		return nil, err
	}
	return gen.MergeSkill200JSONResponse(toSkill(sk)), nil
}

func skillInput(b gen.SkillInput) library.SkillInput {
	in := library.SkillInput{Name: b.Name, Proficiency: deref(b.Proficiency)}
	if b.Type != nil {
		in.Type = string(*b.Type)
	}
	return in
}

func toSkill(s library.Skill) gen.Skill {
	return gen.Skill{Id: s.ID, Name: s.Name, Type: gen.SkillType(s.Type), Proficiency: s.Proficiency}
}

// Evidence

func (s *server) ListEvidences(ctx context.Context, req gen.ListEvidencesRequestObject) (gen.ListEvidencesResponseObject, error) {
	p := req.Params
	f := library.ListFilter{
		Kind: deref(p.Kind), ParentID: p.ParentId, TopLevel: deref(p.TopLevel),
		IncludeArchived: deref(p.IncludeArchived), Cursor: deref(p.Cursor), Limit: deref(p.Limit),
	}
	if p.Category != nil {
		f.Category = library.Category(*p.Category)
	}
	if p.Form != nil {
		f.Form = library.Form(*p.Form)
	}
	docs, next, err := s.lib.ListEvidences(ctx, uid(ctx), f)
	if err != nil {
		return nil, err
	}
	out := gen.ListEvidences200JSONResponse{Items: make([]gen.Evidence, len(docs))}
	for i, d := range docs {
		out.Items[i] = toEvidence(d)
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

func (s *server) GetEvidence(ctx context.Context, req gen.GetEvidenceRequestObject) (gen.GetEvidenceResponseObject, error) {
	d, err := s.lib.GetEvidence(ctx, uid(ctx), req.Id)
	if err != nil {
		return nil, err
	}
	return gen.GetEvidence200JSONResponse{Body: toEvidence(d), Headers: gen.GetEvidence200ResponseHeaders{ETag: etag(d.Version)}}, nil
}

func (s *server) CreateEvidence(ctx context.Context, req gen.CreateEvidenceRequestObject) (gen.CreateEvidenceResponseObject, error) {
	b := req.Body
	in := evidenceInput(b.Kind, b.Category, b.OrganizationId, b.Title, b.Summary, b.Location, b.Url,
		b.Start, b.End, b.IsCurrent, b.Details, b.AutoInclude, b.Skills, b.Highlights)
	in.ParentID = b.ParentId

	var idem *library.Idempotency
	if k := req.Params.IdempotencyKey; k != nil {
		raw, _ := json.Marshal(b) // the decoded body, so formatting doesn't matter
		idem = &library.Idempotency{Key: *k, RequestHash: library.RequestHash(raw)}
	}
	d, _, err := s.lib.CreateEvidence(ctx, uid(ctx), in, idem)
	if err != nil {
		return nil, err
	}
	return gen.CreateEvidence201JSONResponse{Body: toEvidence(d), Headers: gen.CreateEvidence201ResponseHeaders{ETag: etag(d.Version)}}, nil
}

func (s *server) PutEvidence(ctx context.Context, req gen.PutEvidenceRequestObject) (gen.PutEvidenceResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := evidenceInput(b.Kind, b.Category, b.OrganizationId, b.Title, b.Summary, b.Location, b.Url,
		b.Start, b.End, b.IsCurrent, b.Details, b.AutoInclude, b.Skills, b.Highlights)
	d, err := s.lib.UpdateEvidence(ctx, uid(ctx), req.Id, ver, in)
	if err != nil {
		return nil, err
	}
	return gen.PutEvidence200JSONResponse{Body: toEvidence(d), Headers: gen.PutEvidence200ResponseHeaders{ETag: etag(d.Version)}}, nil
}

func (s *server) MoveEvidence(ctx context.Context, req gen.MoveEvidenceRequestObject) (gen.MoveEvidenceResponseObject, error) {
	b := req.Body
	d, err := s.lib.MoveEvidence(ctx, uid(ctx), req.Id, library.MoveInput{ParentID: b.ParentId, AfterID: b.AfterId, BeforeID: b.BeforeId})
	if err != nil {
		return nil, err
	}
	return gen.MoveEvidence200JSONResponse{Body: toEvidence(d), Headers: gen.MoveEvidence200ResponseHeaders{ETag: etag(d.Version)}}, nil
}

func (s *server) ArchiveEvidence(ctx context.Context, req gen.ArchiveEvidenceRequestObject) (gen.ArchiveEvidenceResponseObject, error) {
	d, err := s.lib.SetArchived(ctx, uid(ctx), req.Id, true)
	if err != nil {
		return nil, err
	}
	return gen.ArchiveEvidence200JSONResponse{Body: toEvidence(d), Headers: gen.ArchiveEvidence200ResponseHeaders{ETag: etag(d.Version)}}, nil
}

func (s *server) UnarchiveEvidence(ctx context.Context, req gen.UnarchiveEvidenceRequestObject) (gen.UnarchiveEvidenceResponseObject, error) {
	d, err := s.lib.SetArchived(ctx, uid(ctx), req.Id, false)
	if err != nil {
		return nil, err
	}
	return gen.UnarchiveEvidence200JSONResponse{Body: toEvidence(d), Headers: gen.UnarchiveEvidence200ResponseHeaders{ETag: etag(d.Version)}}, nil
}

func (s *server) DeleteEvidence(ctx context.Context, req gen.DeleteEvidenceRequestObject) (gen.DeleteEvidenceResponseObject, error) {
	if err := s.lib.DeleteEvidence(ctx, uid(ctx), req.Id, deref(req.Params.Cascade)); err != nil {
		return nil, err
	}
	return gen.DeleteEvidence204Response{}, nil
}

func evidenceInput(kind string, cat *gen.Category, org *uuid.UUID, title string, summary, location, url *string,
	start, end *gen.PartialDate, current *bool, details *map[string]any, auto *bool, skills *[]string,
	highlights *[]gen.HighlightInput) library.EvidenceInput {
	in := library.EvidenceInput{
		Kind: kind, OrganizationID: org, Title: title, Summary: deref(summary), Location: deref(location),
		URL: deref(url), Start: deref(start), End: deref(end), IsCurrent: deref(current),
		AutoInclude: auto, Skills: deref(skills),
	}
	if cat != nil {
		in.Category = library.Category(*cat)
	}
	if details != nil {
		in.Details = *details
	}
	for _, h := range deref(highlights) {
		in.Highlights = append(in.Highlights, library.HighlightInput{
			ID: h.Id, Text: h.Text, MetricValue: h.MetricValue, MetricUnit: deref(h.MetricUnit),
			AutoInclude: h.AutoInclude, Skills: deref(h.Skills),
		})
	}
	return in
}

func toEvidence(d library.Evidence) gen.Evidence {
	out := gen.Evidence{
		Id: d.ID, ParentId: d.ParentID, OrganizationId: d.OrganizationID,
		Category: gen.Category(d.Category), Form: gen.Form(d.Form), Kind: d.Kind,
		Title: d.Title, Summary: d.Summary, Location: d.Location, Url: d.URL,
		IsCurrent: d.IsCurrent, Details: d.Details, AutoInclude: d.AutoInclude,
		Version: d.Version, Archived: d.ArchivedAt != nil, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		Skills: d.Skills, Highlights: make([]gen.Highlight, len(d.Highlights)),
	}
	if out.Details == nil {
		out.Details = map[string]any{}
	}
	if d.Start != nil {
		s := d.Start.String()
		out.Start = &s
	}
	if d.End != nil {
		e := d.End.String()
		out.End = &e
	}
	for i, h := range d.Highlights {
		out.Highlights[i] = gen.Highlight{
			Id: h.ID, Text: h.Text, MetricValue: h.MetricValue, MetricUnit: h.MetricUnit,
			AutoInclude: h.AutoInclude, Skills: h.Skills,
		}
	}
	if d.Children != nil {
		children := make([]gen.ChildSummary, len(d.Children))
		for i, c := range d.Children {
			children[i] = gen.ChildSummary{Id: c.ID, Kind: c.Kind, Form: gen.Form(c.Form), Title: c.Title, Archived: c.Archived}
		}
		out.Children = &children
	}
	return out
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
