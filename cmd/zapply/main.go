// Command zapply is the single binary for every server role:
//
//	zapply api       serve the HTTP API
//	zapply relay     publish committed outbox events to NATS
//	zapply migrate   apply database migrations (schema owner)
//	zapply devtoken  print a development bearer token
//
// Later parts add worker. One image, each role scaled on its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/config"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/relay"
	"github.com/diegohh0411/zapply/internal/store"
)

const usage = "usage: zapply <api|relay|migrate|devtoken> [flags]"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	log := logx.New(os.Stderr, cfg.LogFormat, cfg.LogLevel).With("role", os.Args[1])
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "api":
		err = runAPI(ctx, cfg, log)
	case "relay":
		err = runRelay(ctx, cfg, log)
	case "migrate":
		err = runMigrate(ctx, cfg, log)
	case "devtoken":
		err = runDevToken(cfg, os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func runAPI(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := cfg.ValidateAPI(); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	var verifiers auth.MultiVerifier
	if cfg.Auth.JWKSURL != "" {
		v, err := auth.NewJWKS(ctx, cfg.Auth.JWKSURL, cfg.Auth.Issuer, cfg.Auth.Audience)
		if err != nil {
			return err
		}
		verifiers = append(verifiers, v)
	}
	if cfg.Auth.DevSecret != "" {
		log.Warn("development tokens enabled")
		verifiers = append(verifiers, auth.NewDev(cfg.Auth.DevSecret, cfg.Auth.Audience))
	}

	srv := &httpx.Server{Addr: cfg.HTTPAddr, DrainDelay: cfg.DrainDelay, Logger: log}
	srv.Handler = api.Handler(api.Deps{
		Store:    st,
		Verifier: verifiers,
		Logger:   log,
		Draining: srv.Draining,
	})
	return srv.Run(ctx, func(addr net.Addr) { log.Info("listening", "addr", addr.String()) })
}

// runRelay publishes outbox rows until ctx is cancelled. Its admin listener
// reports ready while both Postgres and NATS are reachable.
func runRelay(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := cfg.ValidateRelay(); err != nil {
		return err
	}
	pool, err := store.Connect(ctx, cfg.Relay.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// No reconnect buffer: while disconnected, Publish fails and the batch
	// rolls back, instead of being buffered and later sent a second time.
	nc, err := nats.Connect(cfg.NATSURL,
		nats.Name("zapply-relay"),
		nats.MaxReconnects(-1),
		nats.ReconnectBufSize(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil { // nil on our own Close
				log.Warn("nats disconnected", "err", err)
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) { log.Info("nats reconnected") }),
	)
	if err != nil {
		return fmt.Errorf("connect nats: %w", err)
	}
	defer nc.Close()

	r := relay.New(pool, nc, relay.Config{
		Interval:  cfg.Relay.Interval,
		Batch:     cfg.Relay.Batch,
		Retention: cfg.Relay.Retention,
	}, log)

	srv := &httpx.Server{Addr: cfg.Relay.AdminAddr, DrainDelay: cfg.DrainDelay, Logger: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		pctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if srv.Draining() || !nc.IsConnected() || pool.Ping(pctx) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv.Handler = mux

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	err = srv.Run(ctx, func(addr net.Addr) { log.Info("relay admin listening", "addr", addr.String()) })
	return errors.Join(err, <-done)
}

func runMigrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.MigrateDatabaseURL == "" {
		return errors.New("MIGRATE_DATABASE_URL is required")
	}
	return store.Migrate(ctx, cfg.MigrateDatabaseURL, log)
}

func runDevToken(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("devtoken", flag.ExitOnError)
	sub := fs.String("sub", "dev-user", "subject (stable per user)")
	ttl := fs.Duration("ttl", 24*time.Hour, "token lifetime")
	_ = fs.Parse(args)

	if cfg.Env == config.Production {
		return errors.New("devtoken is disabled in production")
	}
	if cfg.Auth.DevSecret == "" {
		return errors.New("AUTH_DEV_SECRET is required")
	}
	tok, err := auth.MintDev(cfg.Auth.DevSecret, cfg.Auth.Audience, *sub, *ttl)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}
