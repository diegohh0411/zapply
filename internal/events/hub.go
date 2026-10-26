package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

var (
	// ErrUnavailable means NATS is not connected; the client should retry.
	ErrUnavailable = errors.New("events unavailable")
	// ErrTooManyStreams means the user already has the maximum number of
	// streams open on this replica.
	ErrTooManyStreams = errors.New("too many event streams")
)

type HubConfig struct {
	Buffer            int // events buffered per stream before it is closed
	MaxStreamsPerUser int // per replica
}

// Hub owns an API replica's NATS connection and fans each user's events out
// to that user's open streams. A user has one NATS subscription per replica,
// shared by all of their streams and dropped with the last one.
//
// A stream is closed, never silently starved, when it can't keep up or NATS
// disconnects. Its client reconnects with Last-Event-ID and replays what it
// missed from the outbox.
type Hub struct {
	nc  *nats.Conn
	cfg HubConfig
	log *slog.Logger

	mu    sync.Mutex
	users map[uuid.UUID]*userStreams
}

type userStreams struct {
	sub     *nats.Subscription
	streams map[*Stream]struct{}
}

// Stream receives one user's live events. C is closed when the stream must
// end: on overflow, on NATS disconnect, or after Close.
type Stream struct {
	C <-chan Event

	c      chan Event
	hub    *Hub
	userID uuid.UUID
	closed bool // guarded by hub.mu
}

// Connect dials NATS and returns a hub. It starts even if NATS is down:
// streams fail with ErrUnavailable until the connection is up, and the rest
// of the API keeps working.
func Connect(url string, cfg HubConfig, log *slog.Logger) (*Hub, error) {
	if cfg.Buffer <= 0 {
		cfg.Buffer = 256
	}
	if cfg.MaxStreamsPerUser <= 0 {
		cfg.MaxStreamsPerUser = 8
	}
	h := &Hub{cfg: cfg, log: log, users: map[uuid.UUID]*userStreams{}}
	nc, err := nats.Connect(url,
		nats.Name("zapply-api"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil { // nil on our own Close
				log.Warn("nats disconnected; closing event streams", "err", err)
			}
			// Events published while disconnected never reach this
			// replica, so make every client reconnect and replay.
			h.closeAll()
		}),
		nats.ReconnectHandler(func(*nats.Conn) { log.Info("nats reconnected") }),
	)
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}
	h.nc = nc
	return h, nil
}

func (h *Hub) Close() { h.nc.Close() }

// Connected reports whether streams can be opened.
func (h *Hub) Connected() bool { return h.nc.IsConnected() }

// Subscribe opens a stream for userID. When it returns, the subscription is
// active on the NATS server, so every event published from then on reaches
// the stream: replay from the outbox can start.
func (h *Hub) Subscribe(userID uuid.UUID) (*Stream, error) {
	if !h.nc.IsConnected() {
		return nil, ErrUnavailable
	}
	c := make(chan Event, h.cfg.Buffer)
	s := &Stream{C: c, c: c, hub: h, userID: userID}

	h.mu.Lock()
	u := h.users[userID]
	if u == nil {
		sub, err := h.nc.Subscribe(UserSubjects(userID), func(m *nats.Msg) { h.deliver(userID, m) })
		if err != nil {
			h.mu.Unlock()
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		u = &userStreams{sub: sub, streams: map[*Stream]struct{}{}}
		h.users[userID] = u
	} else if len(u.streams) >= h.cfg.MaxStreamsPerUser {
		h.mu.Unlock()
		return nil, ErrTooManyStreams
	}
	u.streams[s] = struct{}{}
	h.mu.Unlock()

	// A round trip after SUB means the server has registered it.
	if err := h.nc.FlushTimeout(2 * time.Second); err != nil {
		s.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return s, nil
}

// Close ends the stream. It is safe to call more than once.
func (s *Stream) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.closeLocked(s)
}

// deliver runs on the user's subscription goroutine, so events reach each
// stream in publish order.
func (h *Hub) deliver(userID uuid.UUID, m *nats.Msg) {
	var e Event
	if err := json.Unmarshal(m.Data, &e); err != nil {
		h.log.Error("bad event message", "subject", m.Subject, "err", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	u := h.users[userID]
	if u == nil {
		return
	}
	for s := range u.streams {
		select {
		case s.c <- e:
		default:
			h.log.Warn("event stream overflowed; closing", "user_id", userID)
			h.closeLocked(s)
		}
	}
}

func (h *Hub) closeLocked(s *Stream) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.c)
	u := h.users[s.userID]
	if u == nil {
		return
	}
	delete(u.streams, s)
	if len(u.streams) == 0 {
		_ = u.sub.Unsubscribe()
		delete(h.users, s.userID)
	}
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, u := range h.users {
		for s := range u.streams {
			h.closeLocked(s)
		}
	}
}
