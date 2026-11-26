package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/model/openrouter"
	"github.com/diegohh0411/zapply/internal/render"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func convertMatch[T any](value any, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	raw, e := json.Marshal(value)
	return decodePlatform[T](raw, e)
}
func (s *server) CreateMatchRun(ctx context.Context, req gen.CreateMatchRunRequestObject) (gen.CreateMatchRunResponseObject, error) {
	version := 0
	if req.Params.Pipeline != nil {
		version = *req.Params.Pipeline
	}
	return convertMatch[gen.CreateMatchRun202JSONResponse](s.matcher.Start(ctx, uid(ctx), req.Id, req.Params.IdempotencyKey, version))
}
func (s *server) ListMatchRuns(ctx context.Context, req gen.ListMatchRunsRequestObject) (gen.ListMatchRunsResponseObject, error) {
	runs, e := s.matcher.List(ctx, uid(ctx), req.Id, req.Params.Before, pageLimit(req.Params.Limit))
	if e != nil {
		return nil, e
	}
	page := map[string]any{"items": runs}
	if len(runs) == pageLimit(req.Params.Limit) {
		page["next_cursor"] = runs[len(runs)-1].ID
	}
	return convertMatch[gen.ListMatchRuns200JSONResponse](page, nil)
}
func (s *server) GetMatchRun(ctx context.Context, req gen.GetMatchRunRequestObject) (gen.GetMatchRunResponseObject, error) {
	return convertMatch[gen.GetMatchRun200JSONResponse](s.matcher.Detail(ctx, uid(ctx), req.Id))
}
func (s *server) GetMatchEstimate(ctx context.Context, req gen.GetMatchEstimateRequestObject) (gen.GetMatchEstimateResponseObject, error) {
	return convertMatch[gen.GetMatchEstimate200JSONResponse](s.matcher.Estimate(ctx, uid(ctx), req.Id))
}
func (s *server) CancelMatchRun(ctx context.Context, req gen.CancelMatchRunRequestObject) (gen.CancelMatchRunResponseObject, error) {
	if e := s.matcher.Cancel(ctx, uid(ctx), req.Id); e != nil {
		return nil, e
	}
	return gen.CancelMatchRun204Response{}, nil
}
func (s *server) PutMatchOverride(ctx context.Context, req gen.PutMatchOverrideRequestObject) (gen.PutMatchOverrideResponseObject, error) {
	c := string(req.Body.Class)
	if e := s.matcher.Override(ctx, uid(ctx), req.Id, req.TextHash, &c); e != nil {
		return nil, e
	}
	return gen.PutMatchOverride204Response{}, nil
}
func (s *server) DeleteMatchOverride(ctx context.Context, req gen.DeleteMatchOverrideRequestObject) (gen.DeleteMatchOverrideResponseObject, error) {
	if e := s.matcher.Override(ctx, uid(ctx), req.Id, req.TextHash, nil); e != nil {
		return nil, e
	}
	return gen.DeleteMatchOverride204Response{}, nil
}
func (s *server) catalogue(ctx context.Context, refresh bool) ([]openrouter.DecisionModel, error) {
	var id uuid.UUID
	var encrypted []byte
	var version string
	e := s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id,ciphertext,encryption_version FROM platform.catalogue_key()`).Scan(&id, &encrypted, &version)
	})
	if e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return nil, &membership.Error{Status: 503, Code: "provider_key_missing", Detail: "Configure an active system OpenRouter key first."}
		}
		return nil, membership.Translate(e)
	}
	key, e := s.deps.Keys.Decrypt(id.String(), version, encrypted)
	if e != nil {
		return nil, &membership.Error{Status: 503, Code: "encryption_unavailable", Detail: "System key encryption is unavailable."}
	}
	var models []openrouter.DecisionModel
	if refreshTextCatalogue(ctx) {
		models, e = s.decisionModels.TextModels(ctx, key)
	} else {
		models, e = s.decisionModels.DecisionModels(ctx, key, refresh)
	}
	if e != nil {
		return nil, &membership.Error{Status: 503, Code: "model_catalogue_unavailable", Detail: "The model catalogue is unavailable. Saved settings are unchanged."}
	}
	return models, nil
}
func (s *server) ListDecisionModels(ctx context.Context, _ gen.ListDecisionModelsRequestObject) (gen.ListDecisionModelsResponseObject, error) {
	return convertMatch[gen.ListDecisionModels200JSONResponse](s.catalogue(ctx, false))
}
func (s *server) GetModelSettings(ctx context.Context, _ gen.GetModelSettingsRequestObject) (gen.GetModelSettingsResponseObject, error) {
	var raw json.RawMessage
	e := s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT platform.get_model_settings()`).Scan(&raw) })
	return decodePlatform[gen.GetModelSettings200JSONResponse](raw, membership.Translate(e))
}
func (s *server) PutModelSettings(ctx context.Context, req gen.PutModelSettingsRequestObject) (gen.PutModelSettingsResponseObject, error) {
	version, e := parseIfMatch(req.Params.IfMatch)
	if e != nil {
		return nil, e
	}
	if r := req.Body.SummaryRows; r != nil && (*r < render.MinSummaryRows || *r > render.MaxSummaryRows) {
		return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "summary_rows", Message: "select between 1 and 4 summary rows"}}}
	}
	effort := optionalString(req.Body.SummaryReasoningEffort)
	if req.Body.SummaryReasoningEffort != nil && !req.Body.SummaryReasoningEffort.Valid() {
		return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "summary_reasoning_effort", Message: "select a supported thinking effort"}}}
	}
	if req.Body.DecisionModel != nil {
		models, e := s.catalogue(ctx, true)
		if e != nil {
			return nil, e
		}
		found := false
		for _, m := range models {
			if m.ID == *req.Body.DecisionModel {
				found = true
				break
			}
		}
		if !found {
			return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "decision_model", Message: "select an available decision model"}}}
		}
	}
	if e := s.validExtractionModel(ctx, req.Body.ExtractionModel); e != nil {
		return nil, e
	}
	if e := s.validPostingModels(ctx, req.Body.PostingDecisionModel, req.Body.PostingExtractionModel); e != nil {
		return nil, e
	}
	if (req.Body.SummaryModel == nil) != (req.Body.SummaryEndpoint == nil) {
		return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "summary_model", Message: "model and endpoint must both be configured"}}}
	}
	if req.Body.SummaryModel != nil {
		if _, err := s.decisionModels.ChatSheet(ctx, *req.Body.SummaryModel, *req.Body.SummaryEndpoint); err != nil {
			return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "summary_model", Message: "select an available text model endpoint supporting bounded output"}}}
		}
	}
	if effort != nil {
		if req.Body.SummaryModel == nil {
			return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "summary_reasoning_effort", Message: "choose a summary model before its thinking effort"}}}
		}
		models, e := s.catalogue(context.WithValue(ctx, textCatalogueKey{}, true), true)
		if e != nil {
			return nil, e
		}
		if !supportsEffort(models, *req.Body.SummaryModel, *effort) {
			return nil, &library.ValidationError{Fields: []library.FieldError{{Field: "summary_reasoning_effort", Message: "select a thinking effort the summary model accepts"}}}
		}
	}
	var raw json.RawMessage
	e = s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT platform.save_model_settings($1,$2)`, req.Body.DecisionModel, version).Scan(&raw); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT platform.save_summary_settings($1,$2,$3,$4)`, req.Body.SummaryModel, req.Body.SummaryEndpoint, req.Body.SummaryRows, effort); err != nil {
			return err
		}
		if m := req.Body.ExtractionModel; m != nil {
			var model *string
			if *m != "" {
				model = m
			}
			if _, err := tx.Exec(ctx, `SELECT platform.save_extraction_model($1)`, model); err != nil {
				return err
			}
		}
		if req.Body.PostingDecisionModel != nil || req.Body.PostingExtractionModel != nil {
			if _, err := tx.Exec(ctx, `SELECT platform.save_posting_models(
 CASE WHEN $1::text IS NULL THEN (SELECT decision FROM platform.posting_models()) ELSE NULLIF($1,'') END,
 CASE WHEN $2::text IS NULL THEN (SELECT extraction FROM platform.posting_models()) ELSE NULLIF($2,'') END)`,
				req.Body.PostingDecisionModel, req.Body.PostingExtractionModel); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `SELECT platform.get_model_settings()`).Scan(&raw)
	})
	return decodePlatform[gen.PutModelSettings200JSONResponse](raw, membership.Translate(e))
}

// optionalString converts an optional enum from the generated API types.
func optionalString[T ~string](value *T) *string {
	if value == nil {
		return nil
	}
	s := string(*value)
	return &s
}

// supportsEffort reports whether the catalogue lists the effort for the model.
func supportsEffort(models []openrouter.DecisionModel, model, effort string) bool {
	for _, m := range models {
		if m.ID == model {
			return m.Reasoning.Supports(effort)
		}
	}
	return false
}

type textCatalogueKey struct{}

func refreshTextCatalogue(ctx context.Context) bool {
	v, _ := ctx.Value(textCatalogueKey{}).(bool)
	return v
}
func (s *server) ListTextModels(ctx context.Context, _ gen.ListTextModelsRequestObject) (gen.ListTextModelsResponseObject, error) {
	return convertMatch[gen.ListTextModels200JSONResponse](s.catalogue(context.WithValue(ctx, textCatalogueKey{}, true), false))
}

func (s *server) ListTextModelEndpoints(ctx context.Context, req gen.ListTextModelEndpointsRequestObject) (gen.ListTextModelEndpointsResponseObject, error) {
	err := s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `SELECT platform.assert_owner()`); return err })
	if err != nil {
		return nil, membership.Translate(err)
	}
	endpoints, err := s.decisionModels.TextEndpoints(ctx, req.Params.Model)
	if err != nil {
		return nil, &membership.Error{Status: 503, Code: "endpoint_catalogue_unavailable", Detail: "Provider endpoints could not be loaded. Try again."}
	}
	return convertMatch[gen.ListTextModelEndpoints200JSONResponse](endpoints, nil)
}
