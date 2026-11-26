package alerts

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/jobsearch"
	"github.com/diegohh0411/zapply/internal/logx"
)

// WithCatalog has polls add what they see to the posting catalog, after
// their alerts are filed. Cataloging never changes or delays an alert: a
// failure is logged, and board postings are tried again on the next poll.
func (s *Service) WithCatalog(c *catalog.Catalog) *Service {
	s.catalog = c
	return s
}

// catalogBoard adds the board's postings the catalog has not seen.
func (s *Service) catalogBoard(ctx context.Context, system, board, company string) {
	if s.catalog == nil {
		return
	}
	if err := s.catalogBoardErr(ctx, system, board, company); err != nil {
		logx.From(ctx).WarnContext(ctx, "board postings not cataloged", "ats", system, "board", board, "error", err)
	}
}

func (s *Service) catalogBoardErr(ctx context.Context, system, board, company string) error {
	var raw []byte
	if err := s.store.Pool().QueryRow(ctx, `SELECT platform.ats_postings_uncataloged($1,$2,$3)`, system, board, maxPostings).Scan(&raw); err != nil {
		return err
	}
	var page struct {
		Company  string        `json:"company"`
		Postings []ats.Posting `json:"postings"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return err
	}
	if len(page.Postings) == 0 {
		return nil
	}
	if strings.TrimSpace(company) == "" {
		company = page.Company
	}
	ss := make([]catalog.Sighting, len(page.Postings))
	keys := make([]string, len(page.Postings))
	for i, p := range page.Postings {
		ss[i] = catalog.Sighting{Source: catalog.SourceATS, ATSKey: system + ":" + board + ":" + p.Key, Company: company, Title: p.Title,
			Location: p.Location, URL: p.URL, Remote: p.Remote, PublishedAt: p.PublishedAt}
		keys[i] = p.Key
	}
	if _, err := s.catalog.Add(ctx, ss); err != nil {
		return err
	}
	_, err := s.store.Pool().Exec(ctx, `SELECT platform.ats_postings_cataloged($1,$2,$3)`, system, board, keys)
	return err
}

// catalogSearch adds a search's postings.
func (s *Service) catalogSearch(ctx context.Context, ps []jobsearch.Posting) {
	if s.catalog == nil || len(ps) == 0 {
		return
	}
	ss := make([]catalog.Sighting, len(ps))
	for i, p := range ps {
		ss[i] = catalog.Sighting{Source: catalog.SourceSearch, Company: p.Company, Title: p.Title, Location: p.Location, URL: p.URL,
			Remote: p.Remote, Country: p.CountryCode, PublishedAt: p.PostedAt, Description: p.Description, SeniorityHint: p.Seniority}
	}
	if _, err := s.catalog.Add(ctx, ss); err != nil {
		logx.From(ctx).WarnContext(ctx, "search postings not cataloged", "error", err)
	}
}
