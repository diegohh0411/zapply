// Package config loads process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
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

	// NATSURL is the NATS server the relay publishes to and the API subscribes to.
	NATSURL string
	// SSEBuffer is how many events a stream buffers before it is closed.
	SSEBuffer int
	// SSEMaxStreamsPerUser caps one user's open streams on each replica.
	SSEMaxStreamsPerUser int

	Auth                   Auth
	Relay                  Relay
	Web                    Web
	Worker                 Worker
	Google                 Google
	Firecrawl              Firecrawl
	SystemKeyRing          string
	SystemKeyActiveVersion string
	// TypstDir holds a Typst CLI per version, TypstDir/<version>/typst, for
	// server-side CV compiles. Empty uses typst on PATH.
	TypstDir string
}

type Worker struct {
	DatabaseURL string
	AdminAddr   string
	Concurrency int
	Model       string
	Endpoint    string
}

// Google configures the Gmail connection (part 10). All of it is optional: with
// no client ID the mail endpoints answer that mail is not configured.
type Google struct {
	ClientID     string
	ClientSecret string
	// PublicURL is this deployment's origin, e.g. https://zapply.example.com. The
	// OAuth redirect and the Pub/Sub push endpoint are built from it.
	PublicURL string
	// PubSubTopic is the full topic name Gmail publishes to
	// (projects/<project>/topics/<topic>). Empty disables push; the reconcile
	// scan then carries all syncing.
	PubSubTopic string
	// PubSubAudience and PubSubServiceAccount check the OIDC token on the push
	// subscription: its audience and the service account that signs it.
	PubSubAudience       string
	PubSubServiceAccount string
	// ReconcileInterval is how long an account may go without a sync before the
	// worker syncs it anyway, in case a notification was dropped.
	ReconcileInterval time.Duration
}

func (g Google) Enabled() bool { return g.ClientID != "" && g.ClientSecret != "" && g.PublicURL != "" }

// RedirectURL is the OAuth redirect URI to register on the client.
func (g Google) RedirectURL() string {
	return strings.TrimRight(g.PublicURL, "/") + "/oauth/google/callback"
}

// PushURL is the endpoint the Pub/Sub push subscription targets.
func (g Google) PushURL() string { return strings.TrimRight(g.PublicURL, "/") + "/hooks/google/pubsub" }

// Firecrawl is the scrape API that reads posting links in the new job dialog:
// Firecrawl Local (deploy/compose.yaml) or any Firecrawl v2 server. Optional:
// without a URL the dialog reads only what the link itself says.
type Firecrawl struct {
	URL   string
	Token string
}

// Web is public configuration for the browser app (served as /config.json).
type Web struct {
	SupabaseURL     string
	SupabaseAnonKey string
	// PublicURL is the deployment origin without a trailing slash, used for
	// canonical URLs. It is the same PUBLIC_URL as Google's.
	PublicURL string
}

type Relay struct {
	// DatabaseURL connects as the relay role (zapply_relay).
	DatabaseURL string
	// AdminAddr serves the relay's /healthz and /readyz.
	AdminAddr string
	Interval  time.Duration
	Batch     int
	// Retention is how long published events stay replayable.
	Retention time.Duration
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
		Env:                    Env(get("ZAPPLY_ENV", string(Development))),
		LogFormat:              get("LOG_FORMAT", ""),
		HTTPAddr:               get("HTTP_ADDR", ":"+get("PORT", "8080")), // PORT: set by Heroku
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		MigrateDatabaseURL:     os.Getenv("MIGRATE_DATABASE_URL"),
		NATSURL:                get("NATS_URL", "nats://127.0.0.1:4222"),
		SystemKeyRing:          os.Getenv("SYSTEM_KEY_RING"),
		SystemKeyActiveVersion: os.Getenv("SYSTEM_KEY_ACTIVE_VERSION"),
		TypstDir:               os.Getenv("TYPST_DIR"),
		Worker:                 Worker{DatabaseURL: os.Getenv("WORKER_DATABASE_URL"), AdminAddr: get("WORKER_ADMIN_ADDR", ":8082"), Model: os.Getenv("LLM_MODEL"), Endpoint: os.Getenv("LLM_ENDPOINT")},
		Google: Google{
			ClientID:             os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
			ClientSecret:         os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
			PublicURL:            os.Getenv("PUBLIC_URL"),
			PubSubTopic:          os.Getenv("GOOGLE_PUBSUB_TOPIC"),
			PubSubAudience:       os.Getenv("GOOGLE_PUBSUB_AUDIENCE"),
			PubSubServiceAccount: os.Getenv("GOOGLE_PUBSUB_SERVICE_ACCOUNT"),
		},
		Firecrawl: Firecrawl{URL: os.Getenv("FIRECRAWL_URL"), Token: os.Getenv("FIRECRAWL_TOKEN")},
		Relay: Relay{
			DatabaseURL: os.Getenv("RELAY_DATABASE_URL"),
			AdminAddr:   get("RELAY_ADMIN_ADDR", ":8081"),
		},
		Web: Web{
			SupabaseURL:     os.Getenv("SUPABASE_URL"),
			SupabaseAnonKey: os.Getenv("SUPABASE_ANON_KEY"),
			PublicURL:       strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"),
		},
		Auth: Auth{
			JWKSURL:   os.Getenv("AUTH_JWKS_URL"),
			Issuer:    os.Getenv("AUTH_ISSUER"),
			Audience:  get("AUTH_AUDIENCE", "authenticated"),
			DevSecret: os.Getenv("AUTH_DEV_SECRET"),
		},
	}

	var errs []error
	if u, err := url.Parse(c.Web.PublicURL); c.Web.PublicURL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		errs = append(errs, fmt.Errorf("PUBLIC_URL must be an absolute http(s) URL, got %q", c.Web.PublicURL))
	}
	var workerErr error
	if c.Worker.Concurrency, workerErr = strconv.Atoi(get("WORKER_CONCURRENCY", "10")); workerErr != nil || c.Worker.Concurrency < 1 || c.Worker.Concurrency > 64 {
		errs = append(errs, errors.New("WORKER_CONCURRENCY must be between 1 and 64"))
	}
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

	if c.Google.ReconcileInterval, err = time.ParseDuration(get("MAIL_RECONCILE_INTERVAL", "6h")); err != nil || c.Google.ReconcileInterval < time.Second {
		errs = append(errs, fmt.Errorf("MAIL_RECONCILE_INTERVAL must be a duration of at least 1s, got %q", get("MAIL_RECONCILE_INTERVAL", "")))
	}
	if g := c.Google; g.ClientID != "" || g.ClientSecret != "" {
		if g.ClientID == "" || g.ClientSecret == "" || g.PublicURL == "" {
			errs = append(errs, errors.New("GOOGLE_OAUTH_CLIENT_ID, GOOGLE_OAUTH_CLIENT_SECRET and PUBLIC_URL are set together"))
		}
		if g.PubSubTopic != "" && g.PubSubServiceAccount == "" {
			errs = append(errs, errors.New("GOOGLE_PUBSUB_SERVICE_ACCOUNT is required with GOOGLE_PUBSUB_TOPIC"))
		}
	}
	if c.Google.PubSubAudience == "" && c.Google.PublicURL != "" {
		c.Google.PubSubAudience = c.Google.PushURL()
	}
	if c.Relay.Interval, err = time.ParseDuration(get("RELAY_INTERVAL", "15s")); err != nil || c.Relay.Interval <= 0 {
		errs = append(errs, fmt.Errorf("RELAY_INTERVAL must be a positive duration, got %q", get("RELAY_INTERVAL", "")))
	}
	if c.Relay.Retention, err = time.ParseDuration(get("EVENT_RETENTION", "24h")); err != nil || c.Relay.Retention <= 0 {
		errs = append(errs, fmt.Errorf("EVENT_RETENTION must be a positive duration, got %q", get("EVENT_RETENTION", "")))
	}
	if c.SSEBuffer, err = strconv.Atoi(get("SSE_BUFFER", "256")); err != nil || c.SSEBuffer < 1 {
		errs = append(errs, fmt.Errorf("SSE_BUFFER must be a positive integer, got %q", get("SSE_BUFFER", "")))
	}
	if c.SSEMaxStreamsPerUser, err = strconv.Atoi(get("SSE_MAX_STREAMS_PER_USER", "8")); err != nil || c.SSEMaxStreamsPerUser < 1 {
		errs = append(errs, fmt.Errorf("SSE_MAX_STREAMS_PER_USER must be a positive integer, got %q", get("SSE_MAX_STREAMS_PER_USER", "")))
	}
	if c.Relay.Batch, err = strconv.Atoi(get("RELAY_BATCH", "500")); err != nil || c.Relay.Batch < 1 || c.Relay.Batch > 10_000 {
		errs = append(errs, fmt.Errorf("RELAY_BATCH must be between 1 and 10000, got %q", get("RELAY_BATCH", "")))
	}

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

// ValidateRelay checks settings the relay role needs.
func (c Config) ValidateRelay() error {
	if c.Relay.DatabaseURL == "" {
		return errors.New("RELAY_DATABASE_URL is required")
	}
	return nil
}

func (c Config) ValidateWorker() error {
	if c.Worker.DatabaseURL == "" {
		return errors.New("WORKER_DATABASE_URL is required (direct or session-pooled Postgres)")
	}
	return nil
}

func get(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
