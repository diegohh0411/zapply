// Command zapply-edge is an edge worker: it runs on a residential machine,
// pulls scrape jobs from a zapply server over HTTPS (outbound only), reads each
// page in an isolated headless Chrome and posts it back. See "Edge workers" in
// the README.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/diegohh0411/zapply/internal/edgeworker"
)

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "zapply-edge:", err)
		os.Exit(1)
	}
}

func run(args []string, env func(string) string) error {
	fs := flag.NewFlagSet("zapply-edge", flag.ContinueOnError)
	serverURL := fs.String("url", env("ZAPPLY_URL"), "zapply server address, e.g. https://zapply.example.com (ZAPPLY_URL)")
	chrome := fs.String("chrome", env("ZAPPLY_EDGE_CHROME"), "Chrome or Chromium binary; empty finds one on PATH (ZAPPLY_EDGE_CHROME)")
	noSandbox := fs.Bool("no-sandbox", env("ZAPPLY_EDGE_NO_SANDBOX") == "1", "run Chrome without its own sandbox, for containers that forbid user namespaces (ZAPPLY_EDGE_NO_SANDBOX=1)")
	minDelay := fs.Duration("min-delay", envDuration(env, "ZAPPLY_EDGE_MIN_DELAY", 2*time.Second), "shortest pause after a job (ZAPPLY_EDGE_MIN_DELAY)")
	maxDelay := fs.Duration("max-delay", envDuration(env, "ZAPPLY_EDGE_MAX_DELAY", 6*time.Second), "longest pause after a job (ZAPPLY_EDGE_MAX_DELAY)")
	verbose := fs.Bool("v", false, "log at debug level")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The token is read from the environment only: a flag would show in the
	// process list.
	token := strings.TrimSpace(env("ZAPPLY_EDGE_TOKEN"))
	switch {
	case *serverURL == "":
		return errors.New("set ZAPPLY_URL (or -url) to your zapply server")
	case token == "":
		return errors.New("set ZAPPLY_EDGE_TOKEN to the token shown when the worker was created")
	case !strings.HasPrefix(*serverURL, "https://") && !strings.HasPrefix(*serverURL, "http://localhost") && !strings.HasPrefix(*serverURL, "http://127.0.0.1"):
		return errors.New("ZAPPLY_URL must be an https:// address: the token travels in every request")
	case *minDelay < 0 || *maxDelay < *minDelay:
		return errors.New("-max-delay must not be shorter than -min-delay")
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	guard := edgeworker.NewGuard()
	browser := edgeworker.NewChrome(*chrome, *noSandbox, guard, log)
	defer browser.Close()
	w := edgeworker.New(edgeworker.NewClient(*serverURL, token), edgeworker.Options{
		Guard: guard, Renderer: browser, Log: log, MinDelay: *minDelay, MaxDelay: *maxDelay,
	})
	log.Info("edge worker started", "server", *serverURL)
	err := w.Run(ctx)
	log.Info("edge worker stopped")
	return err
}

func envDuration(env func(string) string, key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(env(key)); err == nil {
		return d
	}
	return def
}
