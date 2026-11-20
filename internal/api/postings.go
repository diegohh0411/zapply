package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/model/extraction"
	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/diegohh0411/zapply/internal/worker"
)

// ReadPosting scrapes a posting's page for the new job dialog and, when an
// extraction model is configured, starts reading its fields from the text.
// Nothing about the job is saved.
func (s *server) ReadPosting(ctx context.Context, req gen.ReadPostingRequestObject) (gen.ReadPostingResponseObject, error) {
	p, err := s.postings.Read(ctx, req.Body.Url)
	if err != nil {
		return nil, err
	}
	out := postingRead(p)
	if p.Description != "" {
		id, err := s.startExtraction(ctx, uid(ctx), p.Description)
		if err != nil {
			// The page's own data still fills the dialog.
			logx.From(ctx).Warn("posting extraction not started", "err", err)
		}
		out.ExtractionId = id
	}
	return gen.ReadPosting200JSONResponse(out), nil
}

func postingRead(p posting.Posting) gen.PostingRead {
	return gen.PostingRead{
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
	}
}

// startExtraction queues a metered extraction of page's fields. It returns
// nil when no extraction model is configured.
func (s *server) startExtraction(ctx context.Context, u uuid.UUID, page string) (*uuid.UUID, error) {
	if s.queue == nil {
		return nil, nil
	}
	var started *uuid.UUID
	err := s.deps.Store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var model *string
		if err := tx.QueryRow(ctx, `SELECT platform.selected_extraction_model()`).Scan(&model); err != nil {
			return err
		}
		if model == nil || *model == "" {
			return nil
		}
		r := posting.ExtractionRequest(*model, page)
		if err := extraction.Validate(r); err != nil {
			return err
		}
		body, err := json.Marshal(r)
		if err != nil {
			return err
		}
		hash, err := systemone.Hash(r)
		if err != nil {
			return err
		}
		id := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO model_operation(id,user_id,kind,idempotency_key,request_hash) VALUES($1,$2,'extraction',$3,$4)`, id, u, "posting:"+id.String(), hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO model_request(user_id,operation_id,body) VALUES($1,$2,$3)`, u, id, json.RawMessage(body)); err != nil {
			return err
		}
		if err := s.queue.InsertTx(ctx, tx, worker.Args{UserID: u, OperationID: id}); err != nil {
			return err
		}
		started = &id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return started, nil
}

func (s *server) GetPostingExtraction(ctx context.Context, req gen.GetPostingExtractionRequestObject) (gen.GetPostingExtractionResponseObject, error) {
	u := uid(ctx)
	var status, reason, result string
	var body []byte
	err := s.deps.Store.InTenant(ctx, u, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT o.status,COALESCE(o.blocked_reason,''),COALESCE(o.result,''),r.body
 FROM model_operation o JOIN model_request r ON r.user_id=o.user_id AND r.operation_id=o.id
 WHERE o.id=$1 AND o.user_id=$2 AND o.kind='extraction'`, req.Id, u).Scan(&status, &reason, &result, &body)
		if err != nil {
			return err
		}
		// A blocked request would run whenever the member's limits allow,
		// long after the dialog stopped waiting for it.
		if status == "blocked" {
			var cancelled bool
			if err := tx.QueryRow(ctx, `SELECT platform.cancel_operation($1)`, req.Id).Scan(&cancelled); err != nil {
				return err
			}
			if cancelled {
				status = "cancelled"
			}
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, library.ErrNotFound
	}
	if err != nil {
		return nil, membership.Translate(err)
	}
	switch status {
	case "pending", "running", "blocked":
		return gen.GetPostingExtraction200JSONResponse{Status: gen.PostingExtractionStatusPending}, nil
	case "completed":
	default:
		if reason == "" {
			reason = status
		}
		return gen.GetPostingExtraction200JSONResponse{Status: gen.PostingExtractionStatusUnavailable, Reason: &reason}, nil
	}
	var r extraction.Request
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	answers, err := extraction.Parse(r, []byte(result))
	if err != nil {
		reason := "invalid_answers"
		return gen.GetPostingExtraction200JSONResponse{Status: gen.PostingExtractionStatusUnavailable, Reason: &reason}, nil
	}
	x := posting.FromAnswers(answers)
	fields := postingRead(x.Posting)
	return gen.GetPostingExtraction200JSONResponse{Status: gen.PostingExtractionStatusCompleted, Fields: &fields, Certainty: &x.Certainty}, nil
}
