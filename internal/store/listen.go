package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Notification channels raised by triggers (migration 00030).
const (
	ChannelOutbox  = "zapply_outbox"
	ChannelSummary = "zapply_summary"
)

// Listen holds one connection that LISTENs on channel and signals the
// returned channel, coalescing bursts, until ctx ends. It reconnects with
// backoff and signals after every (re)connect, since notifications sent while
// disconnected are lost. Callers must keep a slow fallback poll: behind a
// transaction-mode pooler LISTEN succeeds but nothing is ever delivered.
func Listen(ctx context.Context, url, channel string, log *slog.Logger) <-chan struct{} {
	wake := make(chan struct{}, 1)
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	go func() {
		backoff := time.Second
		for ctx.Err() == nil {
			err := listenOnce(ctx, url, channel, signal, func() { backoff = time.Second })
			if ctx.Err() != nil {
				return
			}
			log.Warn("listen failed; relying on polling until it reconnects", "channel", channel, "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
		}
	}()
	return wake
}

func listenOnce(ctx context.Context, url, channel string, signal, connected func()) error {
	cfg, err := adminConfig(url)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return err
	}
	connected()
	signal()
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		signal()
	}
}
