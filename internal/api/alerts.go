package api

import (
	"context"

	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/api/gen"
)

func (s *server) ListJobWatches(ctx context.Context, _ gen.ListJobWatchesRequestObject) (gen.ListJobWatchesResponseObject, error) {
	items, search, err := s.alerts.Watches(ctx, uid(ctx))
	return mailValue[gen.ListJobWatches200JSONResponse](map[string]any{"items": items, "search": search}, err)
}

func (s *server) CreateJobWatch(ctx context.Context, req gen.CreateJobWatchRequestObject) (gen.CreateJobWatchResponseObject, error) {
	b := req.Body
	in := alerts.Input{URL: deref(b.Url), Company: b.Company, Keywords: b.Keywords, Locations: b.Locations, Country: b.Country, RemoteOnly: b.RemoteOnly}
	if b.Kind != nil {
		in.Kind = string(*b.Kind)
	}
	if b.EveryHours != nil {
		n := int(*b.EveryHours)
		in.EveryHours = &n
	}
	w, err := s.alerts.Create(ctx, uid(ctx), in)
	return mailValue[gen.CreateJobWatch201JSONResponse](w, err)
}

func (s *server) UpdateJobWatch(ctx context.Context, req gen.UpdateJobWatchRequestObject) (gen.UpdateJobWatchResponseObject, error) {
	b := req.Body
	in := alerts.Input{Company: b.Company, Keywords: b.Keywords, Locations: b.Locations, Country: b.Country, RemoteOnly: b.RemoteOnly, Paused: b.Paused}
	if b.EveryHours != nil {
		n := int(*b.EveryHours)
		in.EveryHours = &n
	}
	w, err := s.alerts.Update(ctx, uid(ctx), req.Id, in)
	return mailValue[gen.UpdateJobWatch200JSONResponse](w, err)
}

func (s *server) DeleteJobWatch(ctx context.Context, req gen.DeleteJobWatchRequestObject) (gen.DeleteJobWatchResponseObject, error) {
	if err := s.alerts.Delete(ctx, uid(ctx), req.Id); err != nil {
		return nil, err
	}
	return gen.DeleteJobWatch204Response{}, nil
}
