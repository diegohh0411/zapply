package api

import (
	"context"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/preferences"
)

// Job preferences (part 12)

func (s *server) GetJobPreferences(ctx context.Context, _ gen.GetJobPreferencesRequestObject) (gen.GetJobPreferencesResponseObject, error) {
	p, err := s.prefs.Get(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	return gen.GetJobPreferences200JSONResponse{Body: toJobPreferences(p), Headers: gen.GetJobPreferences200ResponseHeaders{ETag: etag(p.Version)}}, nil
}

func (s *server) PutJobPreferences(ctx context.Context, req gen.PutJobPreferencesRequestObject) (gen.PutJobPreferencesResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	b := req.Body
	var u preferences.Update
	if b.Seniority != nil {
		u.Seniority = &[2]string{string(b.Seniority.Min), string(b.Seniority.Max)}
	}
	u.Areas = b.Areas
	if b.Location != nil {
		u.Location = &preferences.Location{Countries: b.Location.Countries, RemoteOK: b.Location.RemoteOk}
	}
	if b.ExcludedEmployment != nil {
		ex := make([]string, len(*b.ExcludedEmployment))
		for i, e := range *b.ExcludedEmployment {
			ex[i] = string(e)
		}
		u.ExcludedEmployment = &ex
	}
	p, err := s.prefs.Save(ctx, uid(ctx), ver, u)
	if err != nil {
		return nil, err
	}
	return gen.PutJobPreferences200JSONResponse{Body: toJobPreferences(p), Headers: gen.PutJobPreferences200ResponseHeaders{ETag: etag(p.Version)}}, nil
}

func (s *server) ResetJobPreferences(ctx context.Context, req gen.ResetJobPreferencesRequestObject) (gen.ResetJobPreferencesResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	fields := make([]string, len(req.Body.Fields))
	for i, f := range req.Body.Fields {
		fields[i] = string(f)
	}
	p, err := s.prefs.Reset(ctx, uid(ctx), ver, fields)
	if err != nil {
		return nil, err
	}
	return gen.ResetJobPreferences200JSONResponse{Body: toJobPreferences(p), Headers: gen.ResetJobPreferences200ResponseHeaders{ETag: etag(p.Version)}}, nil
}

func toJobPreferences(p preferences.Preferences) gen.JobPreferences {
	level := func(s *string) *gen.SeniorityLevel {
		if s == nil || *s == "" {
			return nil
		}
		l := gen.SeniorityLevel(*s)
		return &l
	}
	types := func(vs []string) []gen.PostingEmploymentType {
		out := make([]gen.PostingEmploymentType, len(vs))
		for i, v := range vs {
			out[i] = gen.PostingEmploymentType(v)
		}
		return out
	}
	str := func(v string) *string { return &v }
	sg := p.Suggested
	out := gen.JobPreferences{
		SeniorityMin: level(p.SeniorityMin), SeniorityMax: level(p.SeniorityMax),
		Areas: nonNil(p.Areas), Countries: nonNil(p.Countries), RemoteOk: p.RemoteOK, ExcludedEmployment: types(p.ExcludedEmployment),
		Suggested: gen.JobPreferenceSuggestion{
			WorkYears: float32(sg.WorkYears), Enrolled: sg.Enrolled, Manager: sg.Manager,
			SeniorityMin: level(str(sg.SeniorityMin)), SeniorityMax: level(str(sg.SeniorityMax)),
			Areas: nonNil(sg.Areas), Countries: nonNil(sg.Countries), RemoteOk: sg.RemoteOK, ExcludedEmployment: types(sg.ExcludedEmployment),
		},
		DerivedAt: p.DerivedAt, Deriving: p.Deriving, Version: p.Version, UpdatedAt: &p.UpdatedAt,
	}
	out.Sources.Seniority = gen.JobPreferenceSource(p.Sources[preferences.FieldSeniority])
	out.Sources.Areas = gen.JobPreferenceSource(p.Sources[preferences.FieldAreas])
	out.Sources.Location = gen.JobPreferenceSource(p.Sources[preferences.FieldLocation])
	out.Sources.Employment = gen.JobPreferenceSource(p.Sources[preferences.FieldEmployment])
	return out
}

func nonNil(vs []string) []string {
	if vs == nil {
		return []string{}
	}
	return vs
}
