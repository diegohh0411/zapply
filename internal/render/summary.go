package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/summary"
	"github.com/diegohh0411/zapply/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Fit is the page fit the browser reported for the content identified by the
// preview's FitKey. Its dropped IDs are the optional items left out to fit.
type Fit struct {
	Dropped []string `json:"dropped"`
}

// FitInput is the browser's report: the FitKey of the preview it fitted and
// the optional IDs its fit dropped.
type FitInput struct {
	FitKey  string   `json:"fit_key"`
	Dropped []string `json:"dropped"`
}

// fitKey identifies the content a browser fit is valid for. The summary is
// left out because fitting reserves its maximum space, so its text can't
// change the fit.
func fitKey(p Preview) string {
	c := p.Content
	c.Profile.Summary = ""
	b, _ := json.Marshal(struct {
		Content         Content  `json:"content"`
		Trim            []string `json:"trim"`
		Template        string   `json:"template"`
		TemplateVersion int      `json:"template_version"`
		PageBudget      int      `json:"page_budget"`
	}{c, p.Trim, p.Template.Name, p.Template.Version, p.Draft.PageBudget})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// summaryEntry is what the summary agent sees of an entry: its heading and
// the highlights on the page. Skills, entry summaries and details stay out.
type summaryEntry struct {
	Title        string         `json:"title"`
	Organization string         `json:"organization,omitempty"`
	Start        *Date          `json:"start,omitempty"`
	End          *Date          `json:"end,omitempty"`
	Current      bool           `json:"current,omitempty"`
	Highlights   []string       `json:"highlights,omitempty"`
	Children     []summaryEntry `json:"children,omitempty"`
}

type summarySection struct {
	Key     string         `json:"key"`
	Entries []summaryEntry `json:"entries"`
}

// summarySections lists what the page shows: entries kept by the fit, with
// the highlights that remain.
func summarySections(sections []Section) []summarySection {
	var entries func([]Entry) []summaryEntry
	entries = func(in []Entry) []summaryEntry {
		var out []summaryEntry
		for _, e := range in {
			se := summaryEntry{Title: e.Title, Organization: e.Organization, Start: e.Start, End: e.End, Current: e.Current, Children: entries(e.Children)}
			for _, h := range e.Highlights {
				se.Highlights = append(se.Highlights, h.Text)
			}
			out = append(out, se)
		}
		return out
	}
	out := []summarySection{}
	for _, s := range sections {
		out = append(out, summarySection{Key: s.Key, Entries: entries(s.Entries)})
	}
	return out
}

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
	var storedFitKey string
	var storedDropped []string
	if err := tx.QueryRow(ctx, `SELECT input_hash,operation_id,dirty,last_summary,fit_key,fit_dropped FROM cv_summary WHERE user_id=$1 AND job_id=$2 FOR UPDATE`, u, d.JobID).Scan(&oldHash, &operation, &dirty, &lastSummary, &storedFitKey, &storedDropped); err != nil {
		return err
	}
	if d.Summary == nil && lastSummary != "" {
		p.Content.Profile.Summary = lastSummary
	}
	if storedFitKey != "" && storedFitKey == p.FitKey {
		p.Fit = &Fit{Dropped: storedDropped}
	}
	var settings struct {
		Model, Endpoint string
		MaxOutputTokens int     `json:"max_output_tokens"`
		MaxCharacters   int     `json:"max_characters"`
		ReasoningEffort *string `json:"reasoning_effort"`
	}
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
	// stop ends any running request and records why no summary is written yet.
	stop := func(status string) error {
		if operation != nil {
			if _, err := tx.Exec(ctx, `SELECT platform.cancel_operation($1)`, *operation); err != nil {
				return err
			}
		}
		p.SummaryStatus = status
		_, err := tx.Exec(ctx, `UPDATE cv_summary SET dirty=false,operation_id=NULL WHERE user_id=$1 AND job_id=$2`, u, d.JobID)
		return err
	}
	if settings.Model == "" || settings.Endpoint == "" {
		return stop("not_configured")
	}
	if strings.TrimSpace(description) == "" || len(p.Content.Sections) == 0 {
		return stop("waiting_for_input")
	}
	// Only the content the browser's fit keeps may reach the agent. Until the
	// browser reports a fit for the current content, no summary is written.
	if p.Fit == nil {
		return stop("waiting_for_fit")
	}
	shown := summarySections(Apply(p.Content, p.Fit.Dropped).Sections)
	if len(shown) == 0 {
		return stop("waiting_for_input")
	}
	// as_of lets the agent tell past roles from current ones. Month precision
	// regenerates a summary at most once a month as roles age into the past.
	input, _ := json.Marshal(map[string]any{"as_of": time.Now().UTC().Format("2006-01"), "locale": p.Content.Locale, "job_description": description, "sections": shown})
	// Automatic thinking keeps the original hash, so only an explicit effort regenerates summaries.
	settingsKey := settings.Model + "|" + settings.Endpoint + "|" + strconv.Itoa(settings.MaxOutputTokens) + "|" + strconv.Itoa(settings.MaxCharacters)
	effort := ""
	if settings.ReasoningEffort != nil {
		effort = *settings.ReasoningEffort
		settingsKey += "|" + effort
	}
	hash := sha256.Sum256(append(input, []byte(settingsKey)...))
	key := hex.EncodeToString(hash[:])
	if oldHash != key || operation == nil || dirty {
		if operation != nil {
			if _, err := tx.Exec(ctx, `SELECT platform.cancel_operation($1)`, *operation); err != nil {
				return err
			}
		}
		q, err := worker.NewQueue(s.store)
		if err != nil {
			return err
		}
		// Reserve extra framing bytes for the SDK's generated system instructions.
		request := worker.SummaryRequest{MaxCharacters: settings.MaxCharacters, MaxOutputTokens: settings.MaxOutputTokens, ReasoningEffort: effort, Model: settings.Model, Endpoint: settings.Endpoint, JobID: d.JobID, Input: string(input), Messages: []summary.Message{{Role: "system", Content: summary.InstructionFor(settings.MaxCharacters) + strings.Repeat(" ", 8192)}, {Role: "user", Content: string(input)}}}
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

// SaveFit stores the browser's fit for the current content and rebuilds the
// preview, which queues the summary from the content that fit keeps. A fit
// for content that has since changed is a conflict.
func (s *Service) SaveFit(ctx context.Context, u, job uuid.UUID, in FitInput) (Preview, error) {
	if in.Dropped == nil {
		in.Dropped = []string{}
	}
	if e := validPicks("dropped", in.Dropped); e != nil {
		return Preview{}, e
	}
	var p Preview
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		d, e := draft(ctx, tx, u, job, true)
		if e != nil {
			return e
		}
		p, e = s.plan(ctx, tx, u, d)
		if e != nil {
			return e
		}
		if p.FitKey != in.FitKey {
			return &library.ConflictError{Message: "the CV changed since it was fitted; reload it and fit again"}
		}
		seen := map[string]bool{}
		for _, id := range in.Dropped {
			if seen[id] || !slices.Contains(p.Trim, id) {
				return invalid("dropped", "only optional items can be dropped, each once")
			}
			seen[id] = true
		}
		if _, e = tx.Exec(ctx, `UPDATE cv_summary SET fit_key=$3,fit_dropped=$4 WHERE user_id=$1 AND job_id=$2`, u, job, in.FitKey, in.Dropped); e != nil {
			return e
		}
		p, e = s.build(ctx, tx, u, d)
		return e
	})
	return p, translate(e)
}

// RefreshSummaries handles invalidated drafts even when the browser is closed.
// A draft whose fit is out of date waits for the browser instead of failing.
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
