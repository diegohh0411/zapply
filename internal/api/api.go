// Package api wires HTTP routes to the domain. /v1 is generated from
// api/openapi.yaml (internal/api/gen); handlers live in library.go, and the
// SSE feed in events.go.
// Job tracking handlers live in jobs.go.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/config"
	"github.com/diegohh0411/zapply/internal/events"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/jobs"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/mail"
	"github.com/diegohh0411/zapply/internal/mail/google"
	"github.com/diegohh0411/zapply/internal/match"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/model/openrouter"
	"github.com/diegohh0411/zapply/internal/notify"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/diegohh0411/zapply/internal/render"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/worker"
)

type Deps struct {
	Keys     *secrets.Ring
	Store    *store.Store
	Verifier auth.Verifier
	Logger   *slog.Logger
	// Draining reports shutdown in progress, so /readyz fails first.
	Draining func() bool
	// Events serves GET /v1/events; nil answers 503.
	Events *events.Hub
	// Drain is closed when shutdown begins, ending event streams.
	Drain <-chan struct{}
	// Heartbeat is the SSE keep-alive interval (default 20 s).
	Heartbeat time.Duration
	// WriteTimeout bounds each SSE write (default 10 s).
	WriteTimeout time.Duration
	// Web serves the single-page app for every other path; nil answers 404.
	Web http.Handler
	// DevToken mints a token for a subject (POST /dev/token, development
	// only); nil disables the route.
	DevToken func(subject string) (string, error)
	// TypstDir holds the Typst CLIs for server-side CV compiles
	// (render.Compiler); empty uses typst on PATH.
	TypstDir string
	// Google configures Gmail (part 10); the zero value leaves mail unconfigured.
	Google config.Google
	// PushVerifier checks the Pub/Sub push token; nil verifies against Google's
	// published keys when push is configured.
	PushVerifier auth.Verifier
	// Postings reads posting pages for POST /v1/postings/read; nil answers 503.
	Postings *posting.Reader
}

// maxBody caps request bodies; the largest valid document is far smaller.
const maxBody = 1 << 20

func Handler(d Deps) http.Handler {
	queue, queueErr := worker.NewQueue(d.Store)
	if queueErr != nil {
		d.Logger.Error("queue initialization failed", "err", queueErr)
		queue = nil
	}
	srv := &server{deps: d, lib: library.NewService(d.Store), jobs: jobs.NewService(d.Store), members: membership.New(d.Store, d.Keys), queue: queue, postings: d.Postings}
	srv.notify = notify.New(d.Store)
	srv.matcher = match.New(d.Store, queue)
	srv.jobs.Matcher = srv.matcher
	srv.cv = render.NewService(d.Store, render.NewCompiler(d.TypstDir))
	srv.decisionModels = openrouter.New("", "")
	var provider mail.Provider
	if d.Google.Enabled() {
		provider = google.New(google.Config{ClientID: d.Google.ClientID, ClientSecret: d.Google.ClientSecret, RedirectURL: d.Google.RedirectURL(), Topic: d.Google.PubSubTopic})
	}
	srv.mail = mail.New(d.Store, queue, d.Keys, provider, d.Google.PubSubTopic != "", d.Google.ReconcileInterval, d.Logger)
	if srv.pushVerifier = d.PushVerifier; srv.pushVerifier == nil && d.Google.PubSubTopic != "" {
		srv.pushVerifier = &lazyVerifier{audience: d.Google.PubSubAudience}
	}
	v1 := http.NewServeMux()
	gen.HandlerWithOptions(
		gen.NewStrictHandlerWithOptions(srv, nil, gen.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  requestError,
			ResponseErrorHandlerFunc: writeError,
		}),
		gen.StdHTTPServerOptions{BaseRouter: v1, ErrorHandlerFunc: requestError},
	)
	// SSE streams outside the strict server, which can't stream; the
	// endpoint is still documented in api/openapi.yaml.
	v1.HandleFunc("GET /v1/events", srv.events)
	v1.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, http.StatusNotFound, "")
	})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	root.HandleFunc("GET /readyz", d.ready)
	root.Handle("GET "+render.AssetsPath, templateAssets())
	root.HandleFunc("GET /oauth/google/callback", srv.googleCallback)
	root.HandleFunc("POST /hooks/google/pubsub", srv.googlePush)
	root.Handle("/v1/", httpx.Chain(v1, auth.Middleware(d.Verifier, d.Store), srv.ownerOnly, limitBody))
	if d.DevToken != nil {
		root.Handle("POST /dev/token", httpx.Chain(http.HandlerFunc(d.devToken), limitBody))
	}
	if d.Web != nil {
		root.Handle("/", d.Web)
	} else {
		root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteProblem(w, r, http.StatusNotFound, "")
		})
	}

	return httpx.Chain(root, httpx.RequestLog(d.Logger), httpx.Recover)
}

// limitBody reads the whole body (at most maxBody) before the handler runs,
// and rejects anything after the first JSON value. The generated decoder
// stops at the first value, so without this, trailing bytes would be
// ignored and could exceed the limit unnoticed.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "request body too large")
			return
		case err != nil:
			httpx.WriteProblem(w, r, http.StatusBadRequest, "could not read request body")
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(raw))
			var first json.RawMessage
			if dec.Decode(&first) == nil {
				if _, err := dec.Token(); err != io.EOF {
					httpx.WriteProblem(w, r, http.StatusBadRequest, "request body must be a single JSON value")
					return
				}
			} // malformed JSON is reported by the handler's decoder
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next.ServeHTTP(w, r)
	})
}

// devToken lets the web app sign in as any subject in development.
func (d Deps) devToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sub string `json:"sub"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Sub == "" || len(body.Sub) > 100 {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "send {\"sub\": \"<name>\"}")
		return
	}
	tok, err := d.DevToken(body.Sub)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (d Deps) ready(w http.ResponseWriter, r *http.Request) {
	if d.Draining != nil && d.Draining() {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "draining")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := d.Store.Ping(ctx); err != nil {
		logx.From(ctx).Warn("readiness: database", "err", err)
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errBadRequest is a malformed request the handler itself rejects.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

func badRequest(msg string) error { return errBadRequest{msg} }

// requestError handles requests the generated code could not bind:
// malformed JSON, bad path or query parameters, missing headers.
func requestError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	var missing *gen.RequiredHeaderError
	switch {
	case errors.As(err, &tooLarge):
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "request body too large")
	case errors.As(err, &missing) && missing.ParamName == "If-Match":
		httpx.WriteProblem(w, r, http.StatusPreconditionRequired, "If-Match is required: send the ETag from your last read")
	default:
		httpx.WriteProblem(w, r, http.StatusBadRequest, err.Error())
	}
}

// writeError maps domain errors to problem+json responses.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *library.ValidationError
	var conflict *library.ConflictError
	var bad errBadRequest
	var dup *jobs.DuplicateError
	var platformError *membership.Error
	var matchConflict *match.Conflict
	var unreadable *posting.UnreadableError
	switch {
	case errors.As(err, &matchConflict):
		httpx.WriteJSON(w, 409, map[string]any{"title": "A match run is already active", "status": 409, "existing_id": matchConflict.ExistingID})
	case errors.As(err, &platformError):
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(platformError.Status)
		_ = json.NewEncoder(w).Encode(struct {
			httpx.Problem
			Code string `json:"code"`
		}{httpx.Problem{Title: http.StatusText(platformError.Status), Status: platformError.Status, Detail: platformError.Detail, RequestID: httpx.RequestID(r.Context())}, platformError.Code})
	case errors.As(err, &dup):
		writeDuplicate(w, r, dup.ExistingID)
	case errors.As(err, &invalid):
		writeValidation(w, r, invalid.Fields)
	case errors.As(err, &conflict):
		httpx.WriteProblem(w, r, http.StatusConflict, conflict.Message)
	case errors.Is(err, posting.ErrNotConfigured):
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "reading postings is not configured on this server")
	case errors.Is(err, posting.ErrInvalidURL):
		writeValidation(w, r, []library.FieldError{{Field: "url", Message: "Enter an http(s) link."}})
	case errors.As(err, &unreadable):
		writeValidation(w, r, []library.FieldError{{Field: "url", Message: "Couldn't read this page: " + unreadable.Reason + "."}})
	case errors.Is(err, mail.ErrNotConfigured):
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "mail is not configured on this server")
	case errors.Is(err, library.ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "")
	case errors.Is(err, library.ErrVersionMismatch):
		httpx.WriteProblem(w, r, http.StatusPreconditionFailed, "the resource changed since your last read; fetch it and retry")
	case errors.As(err, &bad):
		httpx.WriteProblem(w, r, http.StatusBadRequest, bad.msg)
	case errors.Is(err, context.Canceled):
		// Client went away; nothing useful to write.
	default:
		logx.From(r.Context()).Error("request failed", "err", err)
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "")
	}
}

func writeValidation(w http.ResponseWriter, r *http.Request, fields []library.FieldError) {
	p := struct {
		httpx.Problem
		Errors []library.FieldError `json:"errors"`
	}{
		Problem: httpx.Problem{
			Title:     http.StatusText(http.StatusUnprocessableEntity),
			Status:    http.StatusUnprocessableEntity,
			Detail:    "the request has invalid fields",
			RequestID: httpx.RequestID(r.Context()),
		},
		Errors: fields,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(p)
}

// writeDuplicate is a 409 naming the job the user already saved.
func writeDuplicate(w http.ResponseWriter, r *http.Request, existing uuid.UUID) {
	p := struct {
		httpx.Problem
		ExistingID uuid.UUID `json:"existing_id"`
	}{
		Problem: httpx.Problem{
			Title:     http.StatusText(http.StatusConflict),
			Status:    http.StatusConflict,
			Detail:    "a job with this URL already exists",
			RequestID: httpx.RequestID(r.Context()),
		},
		ExistingID: existing,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(p)
}
