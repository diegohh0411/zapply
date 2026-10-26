package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/events"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/store/db"
)

const (
	defaultHeartbeat = 20 * time.Second
	writeTimeout     = 10 * time.Second
	// replayLimit bounds a reconnect's catch-up. Further behind than this,
	// the client gets reset and refetches instead.
	replayLimit = 1000
)

// events serves GET /v1/events, the user's live change feed as Server-Sent
// Events. Design: docs/parts/03-events.md.
//
// The stream subscribes to NATS first, then replays from the outbox
// (seq > Last-Event-ID), then streams live events, dropping any it already
// sent. It ends at token expiry, on drain, on overflow and on NATS
// disconnect; the client reconnects with Last-Event-ID.
//
// Client contract: after `ready`, the stream is live. A client with no
// Last-Event-ID, or one that receives `reset`, fetches its data after
// `ready`, so nothing falls between the fetch and the stream.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	last, hasLast, err := lastEventID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	hub := s.deps.Events
	if hub == nil {
		unavailable(w, r)
		return
	}
	uid := auth.UserID(ctx)

	// One context ends the stream at token expiry or on drain, including
	// while replay waits on the relay lock: an expired token must not
	// receive replayed data.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if exp := auth.ExpiresAt(ctx); !exp.IsZero() {
		var cancelExp context.CancelFunc
		ctx, cancelExp = context.WithDeadlineCause(ctx, exp, errTokenExpired)
		defer cancelExp()
	}
	if s.deps.Drain != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-s.deps.Drain:
				cancel(errDraining)
			case <-stop:
			}
		}()
	}

	stream, err := hub.Subscribe(uid)
	switch {
	case errors.Is(err, events.ErrTooManyStreams):
		httpx.WriteProblem(w, r, http.StatusTooManyRequests, "too many open event streams; close one first")
		return
	case err != nil:
		logx.From(ctx).Warn("event stream unavailable", "err", err)
		unavailable(w, r)
		return
	}
	defer stream.Close()

	rep, err := s.replay(ctx, uid, last, hasLast)
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errTokenExpired):
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "token expired")
		return
	case errors.Is(cause, errDraining):
		unavailable(w, r)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	w.WriteHeader(http.StatusOK)
	sse := sseWriter{ctx: ctx, w: w, rc: http.NewResponseController(w), timeout: s.deps.WriteTimeout}

	if rep.reset {
		if sse.event(0, "reset", []byte("{}")) != nil {
			return
		}
	}
	pos := rep.position
	for _, e := range rep.events {
		if sse.event(e.Seq, e.Topic, e.Data) != nil {
			return
		}
		pos = e.Seq
	}
	if sse.event(pos, "ready", []byte("{}")) != nil {
		return
	}

	beat := s.deps.Heartbeat
	if beat <= 0 {
		beat = defaultHeartbeat
	}
	heartbeat := time.NewTicker(beat)
	defer heartbeat.Stop()

	for {
		select {
		case e, ok := <-stream.C:
			if !ok {
				return // overflow or NATS disconnect: reconnect and replay
			}
			if e.Seq <= pos {
				continue // already sent during replay
			}
			if sse.event(e.Seq, e.Topic, e.Data) != nil {
				return
			}
			pos = e.Seq
		case <-heartbeat.C:
			if sse.raw(": ping\n\n") != nil {
				return
			}
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), errDraining) {
				// Reconnect soon, to a replica that isn't shutting down.
				sse.ctx = context.WithoutCancel(ctx)
				_ = sse.raw("retry: 1000\n\n")
			}
			return // token expired, draining, or client gone
		}
	}
}

var (
	errTokenExpired = errors.New("token expired")
	errDraining     = errors.New("server draining")
)

func unavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "5")
	httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "live events are temporarily unavailable")
}

func lastEventID(r *http.Request) (int64, bool, error) {
	v := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("Last-Event-ID must be an event id from this stream, got %q", v)
	}
	return n, true, nil
}

type replayResult struct {
	events   []events.Event
	reset    bool  // the cursor is gone: refetch
	position int64 // id for `ready` when nothing is replayed
}

// replay reads what the client missed. It first takes the relay lock shared,
// which waits for an in-flight relay batch to commit (see
// events.RelayLockKey).
//
// A cursor is valid while its own event is still in the outbox. Pruning goes
// by publish time, which follows seq, so if that row survives, everything
// after it does too.
func (s *server) replay(ctx context.Context, uid uuid.UUID, last int64, hasLast bool) (replayResult, error) {
	var rep replayResult
	err := s.deps.Store.InTenant(ctx, uid, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, events.RelayLockKey); err != nil {
			return err
		}
		q := db.New(tx)
		latest := func() error {
			var err error
			rep.position, err = q.LatestOutboxSeq(ctx, uid)
			return err
		}
		if !hasLast {
			return latest()
		}
		ok, err := q.OutboxSeqExists(ctx, db.OutboxSeqExistsParams{UserID: uid, Seq: last})
		if err != nil {
			return err
		}
		if !ok {
			rep.reset = true
			return latest()
		}
		rows, err := q.ListOutboxAfter(ctx, db.ListOutboxAfterParams{UserID: uid, After: last, N: replayLimit + 1})
		if err != nil {
			return err
		}
		if len(rows) > replayLimit {
			rep.reset = true
			return latest()
		}
		rep.position = last
		for _, row := range rows {
			rep.events = append(rep.events, events.Event{Seq: row.Seq, Topic: row.Topic, Data: row.Payload})
		}
		return nil
	})
	return rep, err
}

type sseWriter struct {
	ctx     context.Context // writes stop once it ends
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

// event writes one SSE event. id 0 is left out. data must be JSON, which is
// compacted onto one line as SSE requires.
func (s sseWriter) event(id int64, name string, data []byte) error {
	var buf bytes.Buffer
	if id > 0 {
		fmt.Fprintf(&buf, "id: %d\n", id)
	}
	fmt.Fprintf(&buf, "event: %s\ndata: ", name)
	if err := json.Compact(&buf, data); err != nil {
		return err
	}
	buf.WriteString("\n\n")
	return s.raw(buf.String())
}

// raw writes and flushes, with a deadline so a stalled client can't pin the
// handler. The deadline is cleared afterwards: left armed, it would end an
// idle stream (HTTP/2 resets it) before the next heartbeat.
func (s sseWriter) raw(text string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = writeTimeout
	}
	if err := s.deadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte(text)); err != nil {
		return err
	}
	if err := s.flush(); err != nil {
		return err
	}
	return s.deadline(time.Time{})
}

func (s sseWriter) deadline(t time.Time) error {
	if err := s.rc.SetWriteDeadline(t); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (s sseWriter) flush() error { return s.rc.Flush() }
