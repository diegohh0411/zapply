// Package httpx holds HTTP plumbing shared by every handler: middleware,
// JSON and problem+json responses, and a server with graceful drain.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/diegohh0411/zapply/internal/logx"
)

// Middleware wraps a handler. Chain applies them outermost first.
type Middleware func(http.Handler) http.Handler

func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// WriteJSON writes v as a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Problem is an RFC 9457 problem details body.
type Problem struct {
	Type      string `json:"type,omitempty"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func WriteProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	p := Problem{
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    detail,
		RequestID: RequestID(r.Context()),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

type requestIDKey struct{}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestLog assigns a request ID (reusing a well-formed X-Request-ID),
// attaches a scoped logger to the context and logs one line per request.
func RequestLog(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if !validRequestID.MatchString(id) {
				id = newID()
			}
			w.Header().Set("X-Request-ID", id)
			l := base.With("request_id", id)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			ctx = logx.With(ctx, l)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r.WithContext(ctx))

			l.LogAttrs(ctx, slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// Recover turns a handler panic into a 500 and logs it.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				logx.From(r.Context()).Error("panic", "value", v)
				WriteProblem(w, r, http.StatusInternalServerError, "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush and deadlines (needed for SSE).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Server is an http.Server that drains before shutting down: on cancellation it
// reports not-ready, waits DrainDelay so load balancers stop routing to it,
// then lets in-flight requests finish within ShutdownTimeout.
type Server struct {
	Addr            string
	Handler         http.Handler
	DrainDelay      time.Duration
	ShutdownTimeout time.Duration
	Logger          *slog.Logger

	draining atomic.Bool
}

// Draining reports whether shutdown has begun; readiness checks should fail.
func (s *Server) Draining() bool { return s.draining.Load() }

// Run serves until ctx is cancelled, then drains. ready is called once listening.
func (s *Server) Run(ctx context.Context, ready func(addr net.Addr)) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE streams are long-lived. Handlers set their own
		// deadlines through http.ResponseController where needed.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	if ready != nil {
		ready(ln.Addr())
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	s.draining.Store(true)
	s.Logger.Info("draining", "delay", s.DrainDelay)
	time.Sleep(s.DrainDelay)

	timeout := s.ShutdownTimeout
	if timeout == 0 {
		timeout = 25 * time.Second
	}
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
