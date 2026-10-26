// Command zapply is the single binary for every server role:
//
//	zapply api       serve the HTTP API
//	zapply migrate   apply database migrations (schema owner)
//	zapply devtoken  print a development bearer token
//
// Later parts add worker and relay. One image, each role scaled on its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/config"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/store"
)

const usage = "usage: zapply <api|migrate|devtoken> [flags]"

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
