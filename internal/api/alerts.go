package api

import (
	"context"

	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/api/gen"
)

func (s *server) ListJobWatches(ctx context.Context, _ gen.ListJobWatchesRequestObject) (gen.ListJobWatchesResponseObject, error) {
	items, err := s.alerts.Watches(ctx, uid(ctx))
	return mailValue[gen.ListJobWatches200JSONResponse](map[string]any{"items": items}, err)
}

func (s *server) CreateJobWatch(ctx context.Context, req gen.CreateJobWatchRequestObject) (gen.CreateJobWatchResponseObject, error) {
	b := req.Body
	w, err := s.alerts.Create(ctx, uid(ctx), alerts.Input{URL: b.Url, Company: b.Company, Keywords: b.Keywords, Locations: b.Locations, RemoteOnly: b.RemoteOnly})
	return mailValue[gen.CreateJobWatch201JSONResponse](w, err)
}

func (s *server) UpdateJobWatch(ctx context.Context, req gen.UpdateJobWatchRequestObject) (gen.UpdateJobWatchResponseObject, error) {
	b := req.Body
	w, err := s.alerts.Update(ctx, uid(ctx), req.Id, alerts.Input{Company: b.Company, Keywords: b.Keywords, Locations: b.Locations, RemoteOnly: b.RemoteOnly, Paused: b.Paused})
	return mailValue[gen.UpdateJobWatch200JSONResponse](w, err)
}

func (s *server) DeleteJobWatch(ctx context.Context, req gen.DeleteJobWatchRequestObject) (gen.DeleteJobWatchResponseObject, error) {
	if err := s.alerts.Delete(ctx, uid(ctx), req.Id); err != nil {
		return nil, err
	}
	return gen.DeleteJobWatch204Response{}, nil
}
