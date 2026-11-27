// Command zapply is the single binary for every server role:
//
//	zapply api       serve the HTTP API
//	zapply worker    run durable model jobs
//	zapply relay     publish committed outbox events to NATS
//	zapply all       api, worker and relay in one process, with embedded NATS
//	zapply migrate   apply database migrations (schema owner)
//	zapply devtoken  print a development bearer token
//
// One image, each role scaled on its own. `all` is for small deployments
// that fit on a single machine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/diegohh0411/zapply/internal/alerts"
	"github.com/diegohh0411/zapply/internal/ats"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/riverqueue/river"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/config"
	"github.com/diegohh0411/zapply/internal/events"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/jobsearch"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/mail"
	"github.com/diegohh0411/zapply/internal/mail/google"
	"github.com/diegohh0411/zapply/internal/match"
	"github.com/diegohh0411/zapply/internal/model/openrouter"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/diegohh0411/zapply/internal/preferences"
	"github.com/diegohh0411/zapply/internal/relay"
	"github.com/diegohh0411/zapply/internal/render"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/telemetry"
	"github.com/diegohh0411/zapply/internal/webui"
	"github.com/diegohh0411/zapply/internal/worker"
)

const usage = "usage: zapply <api|worker|relay|all|migrate|owner|secrets|devtoken> [flags]"

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
	base := logx.New(os.Stderr, cfg.LogFormat, cfg.LogLevel)
	log := base.With("role", os.Args[1])
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "api" || os.Args[1] == "worker" || os.Args[1] == "relay" || os.Args[1] == "all" {
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
	case "all":
		err = runAll(ctx, cfg, base)
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

	webCfg := webui.Config{SupabaseURL: cfg.Web.SupabaseURL, SupabaseAnonKey: cfg.Web.SupabaseAnonKey, PublicURL: cfg.Web.PublicURL, BuildCommit: os.Getenv("HEROKU_BUILD_COMMIT"), BuiltAt: webui.BuiltAt}
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

	seedScraper(ctx, cfg.Scraper, st, keys, log)

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
		TypstDir: cfg.TypstDir,
		Google:   cfg.Google,
		Postings: posting.New(posting.NewChain(posting.NewDBSource(st, keys))),
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
	runtime := &worker.Runtime{Store: st, Provider: openrouter.New(cfg.Worker.Model, cfg.Worker.Endpoint), Keys: keys, Log: log, MeasureSummary: render.NewCompiler(cfg.TypstDir).SummaryLines}
	q, err := worker.NewQueue(st)
	if err != nil {
		return err
	}
	matcher := match.New(st, q)
	var provider mail.Provider
	if cfg.Google.Enabled() {
		provider = google.New(google.Config{ClientID: cfg.Google.ClientID, ClientSecret: cfg.Google.ClientSecret, RedirectURL: cfg.Google.RedirectURL(), Topic: cfg.Google.PubSubTopic})
	}
	mailer := mail.New(st, q, keys, provider, cfg.Google.PubSubTopic != "", cfg.Google.ReconcileInterval, log)
	postings := catalog.New(st, q, log)
	prefs := preferences.New(st, q, log)
	reads := posting.NewReads(q)
	runtime.OnTerminal = func(ctx context.Context, tx pgx.Tx, op worker.Operation) error {
		if handled, err := postings.OnTerminal(ctx, tx, op); handled || err != nil {
			return err
		}
		if handled, err := reads.OnTerminal(ctx, tx, op); handled || err != nil {
			return err
		}
		if handled, err := prefs.OnTerminal(ctx, tx, op); handled || err != nil {
			return err
		}
		if handled, err := mailer.OnTerminal(ctx, tx, op); handled || err != nil {
			return err
		}
		return matcher.OnTerminal(ctx, tx, op)
	}
	watcher := alerts.New(st, q, postings, log).WithSearch(&jobsearch.TheirStack{}, keys).WithBoards(&ats.Client{})
	postings.Labeled = watcher.Wake
	runtime.AddWorkers = func(w *river.Workers) {
		matcher.AddWorkers(w)
		mailer.AddWorkers(w)
		watcher.AddWorkers(w)
		postings.AddWorkers(w)
	}
	runtime.RecoverFeatures = func(ctx context.Context, c *river.Client[pgx.Tx]) error {
		return errors.Join(matcher.Recover(ctx, c), mailer.Recover(ctx, c), watcher.Recover(ctx, c), postings.Recover(ctx, c), prefs.Recover(ctx, c))
	}
	summaries := render.NewService(st, nil)
	// Woken by dirty cv_summary rows, with a slow poll as fallback. The
	// pause matches platform.dirty_summaries' debounce on changed_at.
	summaryWake := store.Listen(ctx, cfg.Worker.DatabaseURL, store.ChannelSummary, log)
	go func() {
		const fallback, debounce = 30 * time.Second, 750 * time.Millisecond
		timer := time.NewTimer(fallback)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-summaryWake:
				select {
				case <-ctx.Done():
					return
				case <-time.After(debounce):
				}
			case <-timer.C:
			}
			check, done := context.WithTimeout(ctx, 10*time.Second)
			err := summaries.RefreshSummaries(check)
			done()
			if err != nil && ctx.Err() == nil {
				log.Error("summary refresh failed", "err", err)
			}
			timer.Reset(fallback)
		}
	}()
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
		// Heroku kills the process 30s after SIGTERM, so the soft and hard
		// stops together leave a few seconds to spare.
		drain, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		err := client.Stop(drain)
		if err != nil {
			hard, end := context.WithTimeout(context.Background(), 5*time.Second)
			defer end()
			err = errors.Join(err, client.StopAndCancel(hard))
		}
		stopped <- err
	}()
	if cfg.Worker.AdminAddr == "" {
		<-ctx.Done()
		cancel()
		return <-stopped
	}
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
// reports ready while both Postgres and NATS are reachable. An empty
// AdminAddr skips the listener.
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
		Wake:      store.Listen(ctx, cfg.Relay.DatabaseURL, store.ChannelOutbox, log),
		Batch:     cfg.Relay.Batch,
		Retention: cfg.Relay.Retention,
	}, log)
	if cfg.Relay.AdminAddr == "" {
		return r.Run(ctx)
	}

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

// runAll runs the api, worker and relay in one process around an embedded
// NATS server. If any of them stops, the others shut down too. Events lost
// with NATS on a restart are replayed from the outbox when clients reconnect.
func runAll(ctx context.Context, cfg config.Config, base *slog.Logger) error {
	if err := errors.Join(cfg.ValidateAPI(), cfg.ValidateWorker(), cfg.ValidateRelay()); err != nil {
		return err
	}
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		return fmt.Errorf("embedded nats: %w", err)
	}
	go ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(5 * time.Second) {
		return errors.New("embedded nats not ready")
	}
	cfg.NATSURL = ns.ClientURL()
	// The API's /healthz and /readyz cover the whole process.
	cfg.Worker.AdminAddr = ""
	cfg.Relay.AdminAddr = ""

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	roles := []struct {
		name string
		run  func(context.Context, config.Config, *slog.Logger) error
	}{{"api", runAPI}, {"worker", runWorker}, {"relay", runRelay}}
	errs := make(chan error, len(roles))
	for _, role := range roles {
		go func() {
			err := role.run(ctx, cfg, base.With("role", role.name))
			if err != nil {
				err = fmt.Errorf("%s: %w", role.name, err)
			}
			errs <- err
			cancel()
		}()
	}
	var all []error
	for range roles {
		all = append(all, <-errs)
	}
	return errors.Join(all...)
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

// seedScraper imports the environment's scraper as the saved settings when
// none were saved yet (see config.Scraper). A failure is logged: the owner can
// configure scrapers in the admin UI, and the next boot tries again.
func seedScraper(ctx context.Context, c config.Scraper, st *store.Store, keys *secrets.Ring, log *slog.Logger) {
	if c.Provider != config.ScraperFirecrawl {
		return
	}
	seeded, err := posting.SeedFromEnv(ctx, st, keys, c.Firecrawl.URL, c.Firecrawl.Key)
	switch {
	case err != nil:
		log.Warn("scraper settings not imported from the environment", "err", err)
	case seeded:
		log.Info("scraper settings imported from the environment; manage them in the admin UI from now on")
	}
}
