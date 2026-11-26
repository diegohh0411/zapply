package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/model/openrouter"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReadPosting scrapes a posting's page for the new job dialog and, when an
// extraction model is selected, starts reading the fields the page does not
// publish from its text. Nothing is saved.
func (s *server) ReadPosting(ctx context.Context, req gen.ReadPostingRequestObject) (gen.ReadPostingResponseObject, error) {
	text := ""
	if req.Body.Text != nil {
		text = strings.TrimSpace(*req.Body.Text)
	}
	p, err := s.postings.Read(ctx, req.Body.Url)
	pageError := ""
	if err != nil {
		// Pasted text still gets read when the page cannot be.
		var unreadable *posting.UnreadableError
		switch {
		case text != "" && errors.Is(err, posting.ErrNotConfigured):
		case text != "" && errors.As(err, &unreadable):
			pageError = unreadable.Reason
		default:
			return nil, err
		}
		p = posting.Posting{}
	}
	out := gen.ReadPosting200JSONResponse{
		Title:          enumPtr[string](p.Title),
		Company:        enumPtr[string](p.Company),
		Location:       enumPtr[string](p.Location),
		WorkMode:       enumPtr[gen.WorkMode](p.WorkMode),
		EmploymentType: enumPtr[gen.EmploymentType](p.EmploymentType),
		SalaryMin:      p.SalaryMin,
		SalaryMax:      p.SalaryMax,
		SalaryCurrency: enumPtr[string](p.SalaryCurrency),
		SalaryPeriod:   enumPtr[gen.SalaryPeriod](p.SalaryPeriod),
		Description:    p.Description,
		PageError:      enumPtr[string](pageError),
	}
	state := text
	if state == "" {
		state = p.Description
	}
	if state != "" {
		if id, err := s.startExtraction(ctx, state); err != nil {
			// The page's fields stand on their own; extraction is extra.
			logx.From(ctx).WarnContext(ctx, "posting extraction not started", "error", err)
		} else if id != uuid.Nil {
			out.ExtractionId = &id
		}
	}
	return out, nil
}

// startExtraction queues an extraction of state's job fields, or returns
// uuid.Nil when no extraction model is selected.
func (s *server) startExtraction(ctx context.Context, state string) (uuid.UUID, error) {
	if s.queue == nil || s.deps.Store == nil {
		return uuid.Nil, nil
	}
	u := uid(ctx)
	var id uuid.UUID
	err := s.deps.Store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var model *string
		if err := tx.QueryRow(ctx, `SELECT platform.selected_extraction_model()`).Scan(&model); err != nil || model == nil {
			return err
		}
		var err error
		id, err = s.queue.EnqueueExtraction(ctx, tx, u, "posting", posting.ExtractionRequest(*model, state))
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// GetPostingExtraction returns what the extraction model read from a posting.
func (s *server) GetPostingExtraction(ctx context.Context, req gen.GetPostingExtractionRequestObject) (gen.GetPostingExtractionResponseObject, error) {
	u := uid(ctx)
	var status, reason, result string
	var body []byte
	err := s.deps.Store.InTenant(ctx, u, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT o.status,COALESCE(o.blocked_reason,''),COALESCE(o.result,''),r.body FROM model_operation o
 JOIN model_request r ON r.operation_id=o.id AND r.user_id=o.user_id WHERE o.id=$1 AND o.user_id=$2 AND o.kind='extraction'`, req.Id, u).Scan(&status, &reason, &result, &body)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, library.ErrNotFound
	}
	if err != nil {
		return nil, membership.Translate(err)
	}
	out := gen.GetPostingExtraction200JSONResponse{Status: gen.PostingExtractionStatusPending, Reason: enumPtr[string](reason)}
	switch status {
	case "failed", "cancelled":
		out.Status = gen.PostingExtractionStatusFailed
	case "completed":
		var r extraction.Request
		if json.Unmarshal(body, &r) != nil {
			out.Status = gen.PostingExtractionStatusFailed
			break
		}
		p := posting.FromExtraction(r, result)
		out.Status = gen.PostingExtractionStatusCompleted
		out.Title = enumPtr[string](p.Title)
		out.Company = enumPtr[string](p.Company)
		out.Location = enumPtr[string](p.Location)
		out.WorkMode = enumPtr[gen.WorkMode](p.WorkMode)
		out.EmploymentType = enumPtr[gen.EmploymentType](p.EmploymentType)
		out.SalaryMin, out.SalaryMax = p.SalaryMin, p.SalaryMax
		out.SalaryCurrency = enumPtr[string](p.SalaryCurrency)
		out.SalaryPeriod = enumPtr[gen.SalaryPeriod](p.SalaryPeriod)
		out.Description = enumPtr[string](p.Description)
	}
	return out, nil
}

// ListExtractionModels lists the extraction model catalogue for the owner.
func (s *server) ListExtractionModels(ctx context.Context, _ gen.ListExtractionModelsRequestObject) (gen.ListExtractionModelsResponseObject, error) {
	return convertMatch[gen.ListExtractionModels200JSONResponse](s.extractionCatalogue(ctx, false))
}

func (s *server) extractionCatalogue(ctx context.Context, refresh bool) ([]openrouter.ExtractionModel, error) {
	if e := s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error { _, e := tx.Exec(ctx, `SELECT platform.assert_owner()`); return e }); e != nil {
		return nil, membership.Translate(e)
	}
	models, e := s.decisionModels.ExtractionModels(ctx, refresh)
	if e != nil {
		return nil, &membership.Error{Status: 503, Code: "model_catalogue_unavailable", Detail: "The model catalogue is unavailable. Saved settings are unchanged."}
	}
	return models, nil
}

// validExtractionModel checks a selected extraction model against the
// catalogue. Nil keeps the saved model; an empty string turns extraction off.
func (s *server) validExtractionModel(ctx context.Context, model *string) error {
	if model == nil || *model == "" {
		return nil
	}
	models, e := s.extractionCatalogue(ctx, true)
	if e != nil {
		return e
	}
	for _, m := range models {
		if m.ID == *model && m.Compatibility == "" {
			return nil
		}
	}
	return &library.ValidationError{Fields: []library.FieldError{{Field: "extraction_model", Message: "select an available extraction model"}}}
}

// validPostingModels checks the models that label cataloged postings: a
// decision model from the decision catalogue and an extraction model from the
// extraction catalogue. Empty turns a stage off.
func (s *server) validPostingModels(ctx context.Context, decision, extraction *string) error {
	if decision != nil && *decision != "" {
		models, e := s.catalogue(ctx, true)
		if e != nil {
			return e
		}
		found := false
		for _, m := range models {
			found = found || m.ID == *decision
		}
		if !found {
			return &library.ValidationError{Fields: []library.FieldError{{Field: "posting_decision_model", Message: "select an available decision model"}}}
		}
	}
	e := s.validExtractionModel(ctx, extraction)
	var invalid *library.ValidationError
	if errors.As(e, &invalid) {
		return &library.ValidationError{Fields: []library.FieldError{{Field: "posting_extraction_model", Message: "select an available extraction model"}}}
	}
	return e
}

// GetPostingCatalog reports the posting catalog's state for the owner: its
// counts, where each classification stage stands, the catalog member's spend
// and the newest labels to spot-check.
func (s *server) GetPostingCatalog(ctx context.Context, req gen.GetPostingCatalogRequestObject) (gen.GetPostingCatalogResponseObject, error) {
	limit := 20
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	var raw json.RawMessage
	e := s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.posting_catalog($1)`, limit).Scan(&raw)
	})
	return decodePlatform[gen.GetPostingCatalog200JSONResponse](raw, membership.Translate(e))
}
