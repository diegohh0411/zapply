// Package api wires HTTP routes to the domain.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/httpx"
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

func Handler(d Deps) http.Handler {
	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/me", d.me)

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	root.HandleFunc("GET /readyz", d.ready)
	root.Handle("/v1/", auth.Middleware(d.Verifier, d.Store)(v1))
	root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, http.StatusNotFound, "")
	})

	return httpx.Chain(root, httpx.RequestLog(d.Logger), httpx.Recover)
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

type me struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

func (d Deps) me(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	var out me
	err := d.Store.InTenant(r.Context(), uid, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id, created_at FROM app_user WHERE id = $1`, uid,
		).Scan(&out.ID, &out.CreatedAt)
	})
	if err != nil {
		logx.From(r.Context()).Error("get me", "err", err)
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
