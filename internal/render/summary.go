package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/diegohh0411/zapply/internal/summary"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Service) summary(ctx context.Context, tx pgx.Tx, u uuid.UUID, d Draft, p *Preview) error {
	if _, err := tx.Exec(ctx, `INSERT INTO cv_summary(user_id,job_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, u, d.JobID); err != nil {
		return err
	}
	if d.Summary != nil {
		p.Content.Profile.Summary = *d.Summary
	}
	var lastSummary string
	var oldHash string
	var operation *uuid.UUID
	var dirty bool
	if err := tx.QueryRow(ctx, `SELECT input_hash,operation_id,dirty,last_summary FROM cv_summary WHERE user_id=$1 AND job_id=$2 FOR UPDATE`, u, d.JobID).Scan(&oldHash, &operation, &dirty, &lastSummary); err != nil {
		return err
	}
	if d.Summary == nil && lastSummary != "" {
		p.Content.Profile.Summary = lastSummary
	}
	var settings struct{ Model, Endpoint string }
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT platform.summary_settings()`).Scan(&raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return err
	}
	var description string
	if err := tx.QueryRow(ctx, `SELECT description FROM job WHERE user_id=$1 AND id=$2`, u, d.JobID).Scan(&description); err != nil {
		return err
	}
	// Only selected content enters the agent; contact details and the previous summary are excluded.
	input, _ := json.Marshal(map[string]any{"locale": p.Content.Locale, "job_description": description, "sections": p.Content.Sections, "skills": p.Content.Skills})
	hash := sha256.Sum256(append(input, []byte(settings.Model+"|"+settings.Endpoint)...))
	key := hex.EncodeToString(hash[:])
	if oldHash != key || operation == nil || dirty {
		if operation != nil {
			if _, err := tx.Exec(ctx, `SELECT platform.cancel_operation($1)`, *operation); err != nil {
				return err
			}
		}
		if settings.Model == "" || settings.Endpoint == "" {
			p.SummaryStatus = "not_configured"
			_, err := tx.Exec(ctx, `UPDATE cv_summary SET dirty=false,operation_id=NULL WHERE user_id=$1 AND job_id=$2`, u, d.JobID)
			return err
		}
		if strings.TrimSpace(description) == "" || len(p.Content.Sections) == 0 {
			p.SummaryStatus = "waiting_for_input"
			_, err := tx.Exec(ctx, `UPDATE cv_summary SET dirty=false,operation_id=NULL WHERE user_id=$1 AND job_id=$2`, u, d.JobID)
			return err
		}
		q, err := worker.NewQueue(s.store)
		if err != nil {
			return err
		}
		// Reserve extra framing bytes for the SDK's generated system instructions.
		request := worker.SummaryRequest{Model: settings.Model, Endpoint: settings.Endpoint, JobID: d.JobID, Input: string(input), Messages: []summary.Message{{Role: "system", Content: summary.Instruction + strings.Repeat(" ", 8192)}, {Role: "user", Content: string(input)}}}
		id, err := q.EnqueueSummary(ctx, tx, u, key, request)
		if err != nil {
			return err
		}
		operation = &id
		if _, err = tx.Exec(ctx, `UPDATE cv_summary SET input_hash=$3,operation_id=$4,dirty=false WHERE user_id=$1 AND job_id=$2`, u, d.JobID, key, id); err != nil {
			return err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT status,result,blocked_reason FROM model_operation WHERE id=$1`, *operation).Scan(&p.SummaryStatus, &p.SummarySuggestion, &p.SummaryReason); err != nil {
		return err
	}
	if p.SummaryStatus == "completed" && p.SummarySuggestion != "" {
		if _, err := tx.Exec(ctx, `UPDATE cv_summary SET last_summary=$3 WHERE user_id=$1 AND job_id=$2`, u, d.JobID, p.SummarySuggestion); err != nil {
			return err
		}
	}
	if p.SummaryStatus != "completed" {
		p.SummarySuggestion = ""
	}
	if d.Summary != nil {
		p.Content.Profile.Summary = *d.Summary
	} else if p.SummarySuggestion != "" {
		p.Content.Profile.Summary = p.SummarySuggestion
	}
	return nil
}

// RefreshSummaries handles invalidated drafts even when the browser is closed.
func (s *Service) RefreshSummaries(ctx context.Context) error {
	var raw []byte
	if err := s.store.InTenant(ctx, uuid.Nil, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT platform.dirty_summaries()`).Scan(&raw) }); err != nil {
		return err
	}
	var drafts []struct {
		UserID uuid.UUID `json:"user_id"`
		JobID  uuid.UUID `json:"job_id"`
	}
	if err := json.Unmarshal(raw, &drafts); err != nil {
		return err
	}
	for _, d := range drafts {
		if _, err := s.Preview(ctx, d.UserID, d.JobID); err != nil {
			return err
		}
	}
	return nil
}
