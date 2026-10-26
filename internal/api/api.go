// Package api wires HTTP routes to the domain. /v1 is generated from
// api/openapi.yaml (internal/api/gen); handlers live in library.go.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/store"
)

type Deps struct {
	Store    *store.Store
	Verifier auth.Verifier
	Logger   *slog.Logger
	// Draining reports shutdown in progress, so /readyz fails first.
	Draining func() bool
}

// maxBody caps request bodies; the largest valid document is far smaller.
const maxBody = 1 << 20

func Handler(d Deps) http.Handler {
	srv := &server{deps: d, lib: library.NewService(d.Store)}
	v1 := http.NewServeMux()
	gen.HandlerWithOptions(
		gen.NewStrictHandlerWithOptions(srv, nil, gen.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  requestError,
			ResponseErrorHandlerFunc: writeError,
		}),
		gen.StdHTTPServerOptions{BaseRouter: v1, ErrorHandlerFunc: requestError},
	)
	v1.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, http.StatusNotFound, "")
	})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	root.HandleFunc("GET /readyz", d.ready)
	root.Handle("/v1/", httpx.Chain(v1, auth.Middleware(d.Verifier, d.Store), limitBody))
	root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, http.StatusNotFound, "")
	})

	return httpx.Chain(root, httpx.RequestLog(d.Logger), httpx.Recover)
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
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
	switch {
	case errors.As(err, &invalid):
		writeValidation(w, r, invalid.Fields)
	case errors.As(err, &conflict):
		httpx.WriteProblem(w, r, http.StatusConflict, conflict.Message)
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
