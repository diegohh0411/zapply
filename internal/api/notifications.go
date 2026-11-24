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
	if req.Params.View != nil {
		f.View = string(*req.Params.View)
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
	sum, err := s.notify.MarkRead(ctx, uid(ctx), ids, flag(req.Body.All), flag(req.Body.Unread))
	return mailValue[gen.MarkNotificationsRead200JSONResponse](sum, err)
}

func (s *server) ArchiveNotifications(ctx context.Context, req gen.ArchiveNotificationsRequestObject) (gen.ArchiveNotificationsResponseObject, error) {
	var ids []uuid.UUID
	if req.Body.Ids != nil {
		ids = *req.Body.Ids
	}
	sum, err := s.notify.Archive(ctx, uid(ctx), ids, flag(req.Body.Read), flag(req.Body.Restore))
	return mailValue[gen.ArchiveNotifications200JSONResponse](sum, err)
}

func flag(b *bool) bool { return b != nil && *b }
