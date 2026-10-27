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

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/config"
	"github.com/diegohh0411/zapply/internal/events"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/match"
	"github.com/diegohh0411/zapply/internal/model/openrouter"
	"github.com/diegohh0411/zapply/internal/relay"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/telemetry"
	"github.com/diegohh0411/zapply/internal/webui"
	"github.com/diegohh0411/zapply/internal/worker"
)

const usage = "usage: zapply <api|worker|relay|migrate|owner|secrets|devtoken> [flags]"

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
	if os.Args[1] == "api" || os.Args[1] == "worker" || os.Args[1] == "relay" {
		shutdown, err := telemetry.Init(ctx, os.Args[1])
		if err != nil {
			log.Error("tracing initialization failed")
			os.Exit(1)
		}
		defer func() {
			flush, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = shutdown(flush)
		}()
	}

	switch os.Args[1] {
	case "api":
		err = runAPI(ctx, cfg, log)
	case "relay":
		err = runRelay(ctx, cfg, log)
	case "worker":
		err = runWorker(ctx, cfg, log)
	case "owner":
		err = runOwner(ctx, cfg, os.Args[2:])
	case "secrets":
		if len(os.Args) != 3 || os.Args[2] != "reencrypt" || cfg.MigrateDatabaseURL == "" {
			err = errors.New("usage: zapply secrets reencrypt (requires MIGRATE_DATABASE_URL)")
		} else {
			var ring *secrets.Ring
			ring, err = secrets.New(cfg.SystemKeyRing, cfg.SystemKeyActiveVersion)
			if err == nil {
				err = store.ReencryptCredentials(ctx, cfg.MigrateDatabaseURL, ring)
			}
		}
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
	keys, err := secrets.New(cfg.SystemKeyRing, cfg.SystemKeyActiveVersion)
	if err != nil {
		return err
	}

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

	// NATS only feeds /v1/events, so the API starts without it and that
	// endpoint answers 503 until it connects.
	hub, err := events.Connect(cfg.NATSURL, events.HubConfig{
		Buffer:            cfg.SSEBuffer,
		MaxStreamsPerUser: cfg.SSEMaxStreamsPerUser,
	}, log)
	if err != nil {
		return err
	}
	defer hub.Close()

	webCfg := webui.Config{SupabaseURL: cfg.Web.SupabaseURL, SupabaseAnonKey: cfg.Web.SupabaseAnonKey}
	var devToken func(string) (string, error)
	if cfg.Env == config.Development && cfg.Auth.DevSecret != "" {
		webCfg.DevLogin = true
		devToken = func(sub string) (string, error) {
			return auth.MintDev(cfg.Auth.DevSecret, cfg.Auth.Audience, sub, 12*time.Hour)
		}
	}
	if !webui.Built() {
		log.Warn("web app not built into this binary; run make web")
	}

	srv := &httpx.Server{Addr: cfg.HTTPAddr, DrainDelay: cfg.DrainDelay, Logger: log}
	srv.Handler = api.Handler(api.Deps{
		Keys:     keys,
		Store:    st,
		Verifier: verifiers,
		Logger:   log,
		Draining: srv.Draining,
		Events:   hub,
		Drain:    srv.DrainC(),
		Web:      webui.Handler(webCfg),
		DevToken: devToken,
	})
	return srv.Run(ctx, func(addr net.Addr) { log.Info("listening", "addr", addr.String()) })
}

func runOwner(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 || (args[0] != "grant" && args[0] != "revoke") {
		return errors.New("usage: zapply owner <grant|revoke> --user-id <existing UUID>")
	}
	fs := flag.NewFlagSet("owner", flag.ContinueOnError)
	user := fs.String("user-id", "", "existing app user ID")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	id, err := uuid.Parse(*user)
	if err != nil {
		return errors.New("a valid --user-id is required")
	}
	if cfg.MigrateDatabaseURL == "" {
		return errors.New("MIGRATE_DATABASE_URL is required for owner provisioning")
	}
	if err := store.SetOwner(ctx, cfg.MigrateDatabaseURL, id, args[0] == "grant"); err != nil {
		return err
	}
	slog.Info("platform owner permission changed", "user_id", id, "action", args[0])
	return nil
}

func runWorker(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := cfg.ValidateWorker(); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.Worker.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	keys, err := secrets.New(cfg.SystemKeyRing, cfg.SystemKeyActiveVersion)
	if err != nil {
		return err
	}
	runtime := &worker.Runtime{Store: st, Provider: openrouter.New(cfg.Worker.Model, cfg.Worker.Endpoint), Keys: keys, Log: log}
	q, err := worker.NewQueue(st)
	if err != nil {
		return err
	}
	matcher := match.New(st, q)
	runtime.OnTerminal = matcher.OnTerminal
	runtime.AddWorkers = matcher.AddWorkers
	runtime.RecoverFeatures = matcher.Recover
	client, err := runtime.Client(cfg.Worker.Concurrency)
	if err != nil {
		return err
	}
	if err := client.Start(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	stopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			check, done := context.WithTimeout(stopCtx, 20*time.Second)
			err := runtime.Recover(check, client)
			done()
			if err != nil && stopCtx.Err() == nil {
				log.Error("operation recovery failed", "err", err)
			}
			select {
			case <-stopCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	stopped := make(chan error, 1)
	go func() {
		<-stopCtx.Done()
		drain, done := context.WithTimeout(context.Background(), 110*time.Second)
		defer done()
		err := client.Stop(drain)
		if err != nil {
			hard, end := context.WithTimeout(context.Background(), 20*time.Second)
			defer end()
			err = errors.Join(err, client.StopAndCancel(hard))
		}
		stopped <- err
	}()
	srv := &httpx.Server{Addr: cfg.Worker.AdminAddr, DrainDelay: cfg.DrainDelay, Logger: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		check, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if srv.Draining() || st.Ping(check) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv.Handler = mux
	err = srv.Run(ctx, func(addr net.Addr) { log.Info("worker admin listening", "addr", addr.String()) })
	cancel()
	return errors.Join(err, <-stopped)
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

	// If the admin listener fails (e.g. its port is taken), stop relaying
	// too rather than run without health checks.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()
	err = srv.Run(ctx, func(addr net.Addr) { log.Info("relay admin listening", "addr", addr.String()) })
	cancel()
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
