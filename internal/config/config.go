// Package config loads process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Env string

const (
	Development Env = "development"
	Production  Env = "production"
)

type Config struct {
	Env        Env
	LogLevel   slog.Level
	LogFormat  string // "json" or "text"
	HTTPAddr   string
	DrainDelay time.Duration

	// DatabaseURL connects as the restricted runtime role (zapply_app).
	DatabaseURL string
	// MigrateDatabaseURL connects as the schema owner; only `zapply migrate` uses it.
	MigrateDatabaseURL string

	Auth Auth
}

type Auth struct {
	// JWKS verification (production): tokens from an OIDC provider such as Supabase.
	JWKSURL  string
	Issuer   string
	Audience string
	// DevSecret enables HS256 tokens minted by `zapply devtoken`. Rejected in production.
	DevSecret string
}

const DevIssuer = "zapply-dev"

func Load() (Config, error) {
	c := Config{
		Env:                Env(get("ZAPPLY_ENV", string(Development))),
		LogFormat:          get("LOG_FORMAT", ""),
		HTTPAddr:           get("HTTP_ADDR", ":"+get("PORT", "8080")), // PORT: set by Heroku
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		MigrateDatabaseURL: os.Getenv("MIGRATE_DATABASE_URL"),
		Auth: Auth{
			JWKSURL:   os.Getenv("AUTH_JWKS_URL"),
			Issuer:    os.Getenv("AUTH_ISSUER"),
			Audience:  get("AUTH_AUDIENCE", "authenticated"),
			DevSecret: os.Getenv("AUTH_DEV_SECRET"),
		},
	}

	var errs []error
	if c.Env != Development && c.Env != Production {
		errs = append(errs, fmt.Errorf("ZAPPLY_ENV must be %q or %q, got %q", Development, Production, c.Env))
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if c.LogFormat == "" {
		c.LogFormat = "json"
		if c.Env == Development {
			c.LogFormat = "text"
		}
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be json or text, got %q", c.LogFormat))
	}
	defaultDrain := "5s"
	if c.Env == Development {
		defaultDrain = "0s"
	}
	d, err := time.ParseDuration(get("DRAIN_DELAY", defaultDrain))
	if err != nil {
		errs = append(errs, fmt.Errorf("DRAIN_DELAY: %w", err))
	}
	c.DrainDelay = d

	return c, errors.Join(errs...)
}

// ValidateAPI checks settings the API role needs.
func (c Config) ValidateAPI() error {
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	a := c.Auth
	switch {
	case a.JWKSURL == "" && a.DevSecret == "":
		errs = append(errs, errors.New("set AUTH_JWKS_URL (with AUTH_ISSUER) or, in development, AUTH_DEV_SECRET"))
	case a.JWKSURL != "" && a.Issuer == "":
		errs = append(errs, errors.New("AUTH_ISSUER is required with AUTH_JWKS_URL"))
	}
	if a.DevSecret != "" {
		if c.Env == Production {
			errs = append(errs, errors.New("AUTH_DEV_SECRET is not allowed in production"))
		} else if len(a.DevSecret) < 32 {
			errs = append(errs, errors.New("AUTH_DEV_SECRET must be at least 32 bytes"))
		}
	}
	return errors.Join(errs...)
}

func get(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
