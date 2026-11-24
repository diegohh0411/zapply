package api

import (
	"context"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/notify"
	"github.com/google/uuid"
)

func (s *server) ListNotifications(ctx context.Context, req gen.ListNotificationsRequestObject) (gen.ListNotificationsResponseObject, error) {
	f := notify.Filter{Before: req.Params.Before, Limit: pageLimit(req.Params.Limit)}
	if req.Params.Kind != nil {
		f.Kind = string(*req.Params.Kind)
	}
	if req.Params.Category != nil {
		f.Category = string(*req.Params.Category)
	}
	if req.Params.Unread != nil {
		f.Unread = *req.Params.Unread
	}
	items, err := s.notify.List(ctx, uid(ctx), f)
	if err != nil {
		return nil, err
	}
	page := map[string]any{"items": items}
	if len(items) == f.Limit {
		page["next_cursor"] = items[len(items)-1].ID
	}
	return mailValue[gen.ListNotifications200JSONResponse](page, nil)
}

func (s *server) GetNotificationSummary(ctx context.Context, _ gen.GetNotificationSummaryRequestObject) (gen.GetNotificationSummaryResponseObject, error) {
	sum, err := s.notify.Summary(ctx, uid(ctx))
	return mailValue[gen.GetNotificationSummary200JSONResponse](sum, err)
}

func (s *server) MarkNotificationsRead(ctx context.Context, req gen.MarkNotificationsReadRequestObject) (gen.MarkNotificationsReadResponseObject, error) {
	var ids []uuid.UUID
	if req.Body.Ids != nil {
		ids = *req.Body.Ids
	}
	all := req.Body.All != nil && *req.Body.All
	sum, err := s.notify.MarkRead(ctx, uid(ctx), ids, all)
	return mailValue[gen.MarkNotificationsRead200JSONResponse](sum, err)
}
