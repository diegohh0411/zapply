package library

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/store/db"
)

// Organizations and skills: small per-user catalogs that evidence links to.
// Renames change linked evidence content, so they recompute content hashes.

type Organization struct {
	ID        uuid.UUID
	Name      string
	Type      string
	Website   string
	Location  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type OrganizationInput struct {
	Name     string
	Type     string // default "other"
	Website  string
	Location string
}

var organizationTypes = []string{"company", "education", "nonprofit", "government", "client", "other"}

func cleanOrganization(in *OrganizationInput) error {
	var errs []FieldError
	add := func(f, m string) { errs = append(errs, FieldError{f, m}) }
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	checkText(add, "name", in.Name, maxShortText, true)
	if in.Type == "" {
		in.Type = "other"
	}
	if !slices.Contains(organizationTypes, in.Type) {
		add("type", "must be one of "+strings.Join(organizationTypes, ", "))
	}
	in.Website = strings.TrimSpace(in.Website)
	if in.Website != "" {
		if u, err := url.Parse(in.Website); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(in.Website) > maxURL {
			add("website", "must be an http(s) URL")
		}
	}
	in.Location = strings.TrimSpace(in.Location)
	checkText(add, "location", in.Location, maxShortText, false)
	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}

func (s *Service) ListOrganizations(ctx context.Context, uid uuid.UUID) ([]Organization, error) {
	var out []Organization
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		rows, err := q.ListOrganizations(ctx, uid)
		for _, r := range rows {
			out = append(out, orgFromRow(r))
		}
		return err
	})
	return out, err
}

func (s *Service) GetOrganization(ctx context.Context, uid, id uuid.UUID) (Organization, error) {
	var out Organization
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		r, err := q.GetOrganization(ctx, db.GetOrganizationParams{UserID: uid, ID: id})
		out = orgFromRow(r)
		return notFound(err)
	})
	return out, err
}

func (s *Service) CreateOrganization(ctx context.Context, uid uuid.UUID, in OrganizationInput) (Organization, error) {
	if err := cleanOrganization(&in); err != nil {
		return Organization{}, err
	}
	var out Organization
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		r, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{
			ID: newID(), UserID: uid, Name: in.Name, NormalizedName: NormalizeName(in.Name),
			Type: db.OrganizationType(in.Type), Website: in.Website, Location: in.Location,
		})
		if err != nil {
			return orgConflict(err, in.Name)
		}
		out = orgFromRow(r)
		return emit(ctx, q, uid, "organization.created", r.ID, 1)
	})
	return out, err
}

func (s *Service) UpdateOrganization(ctx context.Context, uid, id uuid.UUID, in OrganizationInput) (Organization, error) {
	if err := cleanOrganization(&in); err != nil {
		return Organization{}, err
	}
	var out Organization
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		before, err := q.GetOrganization(ctx, db.GetOrganizationParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		r, err := q.UpdateOrganization(ctx, db.UpdateOrganizationParams{
			UserID: uid, ID: id, Name: in.Name, NormalizedName: NormalizeName(in.Name),
			Type: db.OrganizationType(in.Type), Website: in.Website, Location: in.Location,
		})
		if err != nil {
			return orgConflict(err, in.Name)
		}
		out = orgFromRow(r)
		if before.Name != r.Name {
			ids, err := q.EvidenceIDsForOrganization(ctx, db.EvidenceIDsForOrganizationParams{UserID: uid, OrganizationID: &id})
			if err != nil {
				return err
			}
			// The representation shows only organization_id, so no version bump.
			if err := rehash(ctx, q, uid, ids, false); err != nil {
				return err
			}
		}
		return emit(ctx, q, uid, "organization.updated", id, 0)
	})
	return out, err
}

func (s *Service) DeleteOrganization(ctx context.Context, uid, id uuid.UUID) error {
	return s.tx(ctx, uid, func(q *db.Queries) error {
		inUse, err := q.OrganizationInUse(ctx, db.OrganizationInUseParams{UserID: uid, OrganizationID: &id})
		if err != nil {
			return err
		}
		if inUse {
			return &ConflictError{Message: "organization is referenced by evidence"}
		}
		n, err := q.DeleteOrganization(ctx, db.DeleteOrganizationParams{UserID: uid, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return emit(ctx, q, uid, "organization.deleted", id, 0)
	})
}

func orgConflict(err error, name string) error {
	if e := translate(err); isConflict(e) {
		return &ConflictError{Message: fmt.Sprintf("an organization named %q already exists", name)}
	}
	return err
}

func orgFromRow(r db.Organization) Organization {
	return Organization{
		ID: r.ID, Name: r.Name, Type: string(r.Type), Website: r.Website, Location: r.Location,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

type Skill struct {
	ID          uuid.UUID
	Name        string
	Type        string
	Proficiency string
}

type SkillInput struct {
	Name        string
	Type        string // default "hard"
	Proficiency string
}

var (
	skillTypes       = []string{"hard", "tool", "soft", "language"}
	languageLevels   = []string{"A1", "A2", "B1", "B2", "C1", "C2", "native"}
	proficiencyLevel = []string{"beginner", "intermediate", "advanced", "expert"}
)

func cleanSkill(in *SkillInput) error {
	var errs []FieldError
	add := func(f, m string) { errs = append(errs, FieldError{f, m}) }
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	checkText(add, "name", in.Name, maxSkillName, true)
	if in.Type == "" {
		in.Type = "hard"
	}
	if !slices.Contains(skillTypes, in.Type) {
		add("type", "must be one of "+strings.Join(skillTypes, ", "))
	}
	levels := proficiencyLevel
	if in.Type == "language" {
		levels = languageLevels
	}
	if in.Proficiency != "" && !slices.Contains(levels, in.Proficiency) {
		add("proficiency", "must be one of "+strings.Join(levels, ", "))
	}
	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}

func (s *Service) ListSkills(ctx context.Context, uid uuid.UUID) ([]Skill, error) {
	var out []Skill
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		rows, err := q.ListSkills(ctx, uid)
		for _, r := range rows {
			out = append(out, skillFromRow(r))
		}
		return err
	})
	return out, err
}

func (s *Service) CreateSkill(ctx context.Context, uid uuid.UUID, in SkillInput) (Skill, error) {
	if err := cleanSkill(&in); err != nil {
		return Skill{}, err
	}
	var out Skill
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		r, err := q.CreateSkill(ctx, db.CreateSkillParams{
			ID: newID(), UserID: uid, Name: in.Name, NormalizedName: NormalizeName(in.Name),
			Type: db.SkillType(in.Type), Proficiency: in.Proficiency,
		})
		if err != nil {
			return skillConflict(err, in.Name)
		}
		out = skillFromRow(r)
		return emit(ctx, q, uid, "skill.created", r.ID, 0)
	})
	return out, err
}

func (s *Service) UpdateSkill(ctx context.Context, uid, id uuid.UUID, in SkillInput) (Skill, error) {
	if err := cleanSkill(&in); err != nil {
		return Skill{}, err
	}
	var out Skill
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		before, err := q.GetSkill(ctx, db.GetSkillParams{UserID: uid, ID: id})
		if err != nil {
			return notFound(err)
		}
		r, err := q.UpdateSkill(ctx, db.UpdateSkillParams{
			UserID: uid, ID: id, Name: in.Name, NormalizedName: NormalizeName(in.Name),
			Type: db.SkillType(in.Type), Proficiency: in.Proficiency,
		})
		if err != nil {
			return skillConflict(err, in.Name)
		}
		out = skillFromRow(r)
		if before.Name != r.Name {
			// Linked documents list skills by name: new hashes and versions.
			if err := rehashSkill(ctx, q, uid, id); err != nil {
				return err
			}
		}
		return emit(ctx, q, uid, "skill.updated", id, 0)
	})
	return out, err
}

func (s *Service) DeleteSkill(ctx context.Context, uid, id uuid.UUID) error {
	return s.tx(ctx, uid, func(q *db.Queries) error {
		ids, err := q.EvidenceIDsForSkill(ctx, db.EvidenceIDsForSkillParams{UserID: uid, SkillID: id})
		if err != nil {
			return err
		}
		n, err := q.DeleteSkill(ctx, db.DeleteSkillParams{UserID: uid, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if err := rehash(ctx, q, uid, ids, true); err != nil {
			return err
		}
		return emit(ctx, q, uid, "skill.deleted", id, 0)
	})
}

// MergeSkill moves every link from skill `from` to `into`, then deletes `from`.
func (s *Service) MergeSkill(ctx context.Context, uid, from, into uuid.UUID) (Skill, error) {
	if from == into {
		return Skill{}, invalid("into", "must be a different skill")
	}
	var out Skill
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		if _, err := q.GetSkill(ctx, db.GetSkillParams{UserID: uid, ID: from}); err != nil {
			return notFound(err)
		}
		target, err := q.GetSkill(ctx, db.GetSkillParams{UserID: uid, ID: into})
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("into", "skill not found")
		}
		if err != nil {
			return err
		}
		ids, err := q.EvidenceIDsForSkill(ctx, db.EvidenceIDsForSkillParams{UserID: uid, SkillID: from})
		if err != nil {
			return err
		}
		if err := q.RelinkEvidenceSkills(ctx, db.RelinkEvidenceSkillsParams{UserID: uid, IntoID: into, FromID: from}); err != nil {
			return err
		}
		if err := q.RelinkHighlightSkills(ctx, db.RelinkHighlightSkillsParams{UserID: uid, IntoID: into, FromID: from}); err != nil {
			return err
		}
		if _, err := q.DeleteSkill(ctx, db.DeleteSkillParams{UserID: uid, ID: from}); err != nil {
			return err
		}
		if err := rehash(ctx, q, uid, ids, true); err != nil {
			return err
		}
		out = skillFromRow(target)
		return emit(ctx, q, uid, "skill.merged", from, 0)
	})
	return out, err
}

func rehashSkill(ctx context.Context, q *db.Queries, uid, skillID uuid.UUID) error {
	ids, err := q.EvidenceIDsForSkill(ctx, db.EvidenceIDsForSkillParams{UserID: uid, SkillID: skillID})
	if err != nil {
		return err
	}
	return rehash(ctx, q, uid, ids, true)
}

func skillConflict(err error, name string) error {
	if e := translate(err); isConflict(e) {
		return &ConflictError{Message: fmt.Sprintf("a skill named %q already exists", name)}
	}
	return err
}

func isConflict(err error) bool {
	var c *ConflictError
	return errors.As(err, &c)
}

func skillFromRow(r db.Skill) Skill {
	return Skill{ID: r.ID, Name: r.Name, Type: string(r.Type), Proficiency: r.Proficiency}
}

// ensureSkills returns the user's skills for the given names, creating (and
// announcing) any that are missing. Keys are normalized names.
func ensureSkills(ctx context.Context, q *db.Queries, uid uuid.UUID, names []string) (map[string]db.Skill, error) {
	if len(names) == 0 {
		return map[string]db.Skill{}, nil
	}
	norm := make([]string, len(names))
	for i, n := range names {
		norm[i] = NormalizeName(n)
		id := newID()
		created, err := q.CreateSkillIfMissing(ctx, db.CreateSkillIfMissingParams{
			ID: id, UserID: uid, Name: n, NormalizedName: norm[i],
		})
		if err != nil {
			return nil, err
		}
		if created == 1 {
			if err := emit(ctx, q, uid, "skill.created", id, 0); err != nil {
				return nil, err
			}
		}
	}
	rows, err := q.GetSkillsByNormalizedName(ctx, db.GetSkillsByNormalizedNameParams{UserID: uid, Names: norm})
	if err != nil {
		return nil, err
	}
	out := make(map[string]db.Skill, len(rows))
	for _, r := range rows {
		out[r.NormalizedName] = r
	}
	return out, nil
}

func itoa(i int) string { return strconv.Itoa(i) }
