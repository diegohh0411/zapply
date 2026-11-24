package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/posting"
)

// GetScraperSettings returns the scrapers and per-site rules (owner only).
func (s *server) GetScraperSettings(ctx context.Context, _ gen.GetScraperSettingsRequestObject) (gen.GetScraperSettingsResponseObject, error) {
	var raw json.RawMessage
	e := s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT platform.assert_owner()`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT platform.scraper_settings()`).Scan(&raw)
	})
	return decodePlatform[gen.GetScraperSettings200JSONResponse](raw, membership.Translate(e))
}

// PutScraperSettings replaces the scrapers and per-site rules (owner only).
func (s *server) PutScraperSettings(ctx context.Context, req gen.PutScraperSettingsRequestObject) (gen.PutScraperSettingsResponseObject, error) {
	version, e := parseIfMatch(req.Params.IfMatch)
	if e != nil {
		return nil, e
	}
	settings := posting.Settings{Providers: []posting.ProviderConfig{}, Rules: []posting.Rule{}}
	for _, p := range req.Body.Providers {
		c := posting.ProviderConfig{Kind: string(p.Kind), BaseURL: deref(p.BaseUrl), CreditUSD: deref(p.CreditUsd)}
		if p.KeyId != nil {
			c.KeyID = p.KeyId.String()
		}
		settings.Providers = append(settings.Providers, c)
	}
	for _, r := range req.Body.Rules {
		settings.Rules = append(settings.Rules, posting.Rule{Host: r.Host, Providers: r.Providers})
	}
	if e := settings.Validate(); e != nil {
		var invalid *posting.SettingsError
		if errors.As(e, &invalid) {
			return nil, &library.ValidationError{Fields: []library.FieldError{{Field: invalid.Field, Message: invalid.Message}}}
		}
		return nil, e
	}
	providers, e := json.Marshal(settings.Providers)
	if e != nil {
		return nil, e
	}
	rules, e := json.Marshal(settings.Rules)
	if e != nil {
		return nil, e
	}
	var raw json.RawMessage
	e = s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.save_scraper_settings($1::jsonb,$2::jsonb,$3)`, string(providers), string(rules), version).Scan(&raw)
	})
	return decodePlatform[gen.PutScraperSettings200JSONResponse](raw, membership.Translate(e))
}
