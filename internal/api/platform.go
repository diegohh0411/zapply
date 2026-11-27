package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/jobsearch"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/google/uuid"
)

// Database functions return explicitly shaped JSON; decoding into generated
// types keeps the OpenAPI contract authoritative for these small resources.
func decodePlatform[T any](raw json.RawMessage, err error) (T, error) {
	var value T
	if err != nil {
		return value, err
	}
	err = json.Unmarshal(raw, &value)
	return value, err
}
func pageLimit(n *int) int {
	if n == nil {
		return 50
	}
	return min(max(*n, 1), 100)
}
func (s *server) ownerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/admin/") {
			if err := s.members.AssertOwner(r.Context(), uid(r.Context())); err != nil {
				writeError(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (s *server) GetUsage(ctx context.Context, _ gen.GetUsageRequestObject) (gen.GetUsageResponseObject, error) {
	v, err := decodePlatform[gen.GetUsage200JSONResponse](s.members.Usage(ctx, uid(ctx)))
	return v, err
}
func (s *server) ListUsageEntries(ctx context.Context, req gen.ListUsageEntriesRequestObject) (gen.ListUsageEntriesResponseObject, error) {
	var before *int64
	if req.Params.Before != nil {
		n, err := strconv.ParseInt(*req.Params.Before, 10, 64)
		if err != nil || n < 1 {
			return nil, badRequest("before must be a positive entry ID")
		}
		before = &n
	}
	v, err := decodePlatform[gen.ListUsageEntries200JSONResponse](s.members.Entries(ctx, uid(ctx), before, pageLimit(req.Params.Limit)))
	return v, err
}
func (s *server) ListPlans(ctx context.Context, _ gen.ListPlansRequestObject) (gen.ListPlansResponseObject, error) {
	v, err := decodePlatform[gen.ListPlans200JSONResponse](s.members.Plans(ctx, uid(ctx)))
	return v, err
}
func (s *server) CreatePlan(ctx context.Context, req gen.CreatePlanRequestObject) (gen.CreatePlanResponseObject, error) {
	b := req.Body
	v, err := decodePlatform[gen.Plan](s.members.SavePlan(ctx, uid(ctx), uuid.Must(uuid.NewV7()), b.Name, b.Cap7dUsd, b.Cap30dUsd, 0))
	if err != nil {
		return nil, err
	}
	return gen.CreatePlan201JSONResponse{Body: v, Headers: gen.CreatePlan201ResponseHeaders{ETag: etag(v.Version)}}, nil
}
func (s *server) UpdatePlan(ctx context.Context, req gen.UpdatePlanRequestObject) (gen.UpdatePlanResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	if ver == 0 {
		return nil, badRequest("existing plan version is required")
	}
	b := req.Body
	v, err := decodePlatform[gen.Plan](s.members.SavePlan(ctx, uid(ctx), req.Id, b.Name, b.Cap7dUsd, b.Cap30dUsd, ver))
	if err != nil {
		return nil, err
	}
	return gen.UpdatePlan200JSONResponse{Body: v, Headers: gen.UpdatePlan200ResponseHeaders{ETag: etag(v.Version)}}, nil
}
func (s *server) ListMembers(ctx context.Context, req gen.ListMembersRequestObject) (gen.ListMembersResponseObject, error) {
	v, err := decodePlatform[gen.ListMembers200JSONResponse](s.members.Members(ctx, uid(ctx), req.Params.After, pageLimit(req.Params.Limit)))
	return v, err
}
func (s *server) PutMembership(ctx context.Context, req gen.PutMembershipRequestObject) (gen.PutMembershipResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	b := req.Body
	v, err := decodePlatform[gen.Membership](s.members.SaveMembership(ctx, uid(ctx), req.Id, b.PlanId, string(b.Status), ver))
	if err != nil {
		return nil, err
	}
	return gen.PutMembership200JSONResponse{Body: v, Headers: gen.PutMembership200ResponseHeaders{ETag: etag(v.Version)}}, nil
}
func (s *server) ListSystemKeys(ctx context.Context, _ gen.ListSystemKeysRequestObject) (gen.ListSystemKeysResponseObject, error) {
	v, err := decodePlatform[gen.ListSystemKeys200JSONResponse](s.members.Keys(ctx, uid(ctx)))
	return v, err
}
func (s *server) CreateSystemKey(ctx context.Context, req gen.CreateSystemKeyRequestObject) (gen.CreateSystemKeyResponseObject, error) {
	kind := gen.SystemKeyKindOpenrouter
	if req.Body.Kind != nil {
		kind = *req.Body.Kind
	}
	id, err := s.members.AddKeyOfKind(ctx, uid(ctx), req.Body.Label, deref(req.Body.Key), string(kind))
	if err != nil {
		return nil, err
	}
	keys, err := decodePlatform[[]gen.SystemKey](s.members.Keys(ctx, uid(ctx)))
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		if key.Id == id {
			return gen.CreateSystemKey201JSONResponse{Body: key, Headers: gen.CreateSystemKey201ResponseHeaders{ETag: etag(key.Version)}}, nil
		}
	}
	return nil, &membership.Error{Status: 503, Code: "key_unavailable", Detail: "key metadata is unavailable"}
}
func (s *server) ActivateSystemKey(ctx context.Context, req gen.ActivateSystemKeyRequestObject) (gen.ActivateSystemKeyResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	if err := s.members.ActivateKey(ctx, uid(ctx), req.Id, req.Body.Active, ver); err != nil {
		return nil, err
	}
	return gen.ActivateSystemKey204Response{}, nil
}
func (s *server) ListModelAttempts(ctx context.Context, req gen.ListModelAttemptsRequestObject) (gen.ListModelAttemptsResponseObject, error) {
	v, err := decodePlatform[gen.ListModelAttempts200JSONResponse](s.members.Attempts(ctx, uid(ctx), req.Params.Before, pageLimit(req.Params.Limit)))
	return v, err
}
func (s *server) ResolveModelAttempt(ctx context.Context, req gen.ResolveModelAttemptRequestObject) (gen.ResolveModelAttemptResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	b := req.Body
	if err := s.members.Resolve(ctx, uid(ctx), req.Id, b.AmountUsd, b.GenerationId, b.Reason, ver); err != nil {
		return nil, err
	}
	return gen.ResolveModelAttempt204Response{}, nil
}
func (s *server) CreateGatewayTest(ctx context.Context, req gen.CreateGatewayTestRequestObject) (gen.CreateGatewayTestResponseObject, error) {
	if s.queue == nil {
		return nil, &membership.Error{Status: 503, Code: "queue_unavailable", Detail: "worker queue is unavailable"}
	}
	op, err := s.queue.Enqueue(ctx, uid(ctx), req.Params.IdempotencyKey, httpx.RequestID(ctx))
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(op)
	v, err := decodePlatform[gen.CreateGatewayTest202JSONResponse](raw, nil)
	return v, err
}
func (s *server) GetGatewayTest(ctx context.Context, req gen.GetGatewayTestRequestObject) (gen.GetGatewayTestResponseObject, error) {
	if s.queue == nil {
		return nil, &membership.Error{Status: 503, Code: "queue_unavailable", Detail: "worker queue is unavailable"}
	}
	op, err := s.queue.Get(ctx, uid(ctx), req.Id)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(op)
	v, err := decodePlatform[gen.GetGatewayTest200JSONResponse](raw, nil)
	return v, err
}

// GetSearchSettings returns the job search provider behind search watchers (owner only).
func (s *server) GetSearchSettings(ctx context.Context, _ gen.GetSearchSettingsRequestObject) (gen.GetSearchSettingsResponseObject, error) {
	v, err := decodePlatform[gen.SearchSettings](s.members.SearchSettings(ctx, uid(ctx)))
	if err != nil {
		return nil, err
	}
	return gen.GetSearchSettings200JSONResponse(v), nil
}

// PutSearchSettings sets the TheirStack key and credit price (owner only).
func (s *server) PutSearchSettings(ctx context.Context, req gen.PutSearchSettingsRequestObject) (gen.PutSearchSettingsResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	b := req.Body
	v, err := decodePlatform[gen.SearchSettings](s.members.SaveSearchSettings(ctx, uid(ctx), b.KeyId, b.MaxResults, ver))
	if err != nil {
		return nil, err
	}
	return gen.PutSearchSettings200JSONResponse(v), nil
}

// GetSearchBalance reads the team's TheirStack credit balance (owner only). A
// provider failure is reported in the body, so the settings page still loads.
func (s *server) GetSearchBalance(ctx context.Context, _ gen.GetSearchBalanceRequestObject) (gen.GetSearchBalanceResponseObject, error) {
	b, err := s.members.SearchBalance(ctx, uid(ctx), &jobsearch.TheirStack{})
	var pe *jobsearch.Error
	switch {
	case errors.As(err, &pe):
		return gen.GetSearchBalance200JSONResponse{Configured: true, Error: pe.Reason}, nil
	case err != nil:
		return nil, err
	case b == nil:
		return gen.GetSearchBalance200JSONResponse{}, nil
	}
	return gen.GetSearchBalance200JSONResponse{Configured: true, Credits: &b.Credits, Used: &b.Used, ExpiresAt: b.Expires}, nil
}
