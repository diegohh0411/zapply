package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/edgetoken"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/diegohh0411/zapply/internal/store"
)

const (
	defaultEdgePerHour    = 20
	defaultEdgeConcurrent = 1

	// claimWait is how long a claim request holds open before answering 204.
	claimWait = 25 * time.Second
	// claimPoll is how often a waiting claim looks for a job. It also
	// refreshes the worker's last-seen time (at most every 10 seconds).
	claimPoll = time.Second

	// Result limits. They match the database's check constraints, so an
	// oversized result is refused here with a clear status instead of there.
	maxResultMarkdown = 1 << 20
	maxResultHTML     = 4 << 20
	maxResultMetadata = 64 << 10
	maxResultBody     = 6 << 20
	maxResultError    = 500
)

// edgeWorkerLimits checks a worker's host list and limits; nil leaves a limit
// unchanged (patch) and the caller applies the default for a new worker.
func edgeWorkerLimits(hosts *[]string, perHour, concurrent *int) ([]string, error) {
	var out []string
	if hosts != nil {
		out = make([]string, 0, len(*hosts))
		seen := map[string]bool{}
		for i, h := range *hosts {
			h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
			if !posting.ValidHostname(h) {
				return nil, fieldError("allowed_hosts", "use bare lowercase hostnames such as linkedin.com, without a scheme, port or path")
			}
			if seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
			if i >= 50 {
				return nil, fieldError("allowed_hosts", "allow at most 50 hosts")
			}
		}
	}
	if perHour != nil && (*perHour < 1 || *perHour > 600) {
		return nil, fieldError("max_per_hour", "must be between 1 and 600")
	}
	if concurrent != nil && (*concurrent < 1 || *concurrent > 10) {
		return nil, fieldError("max_concurrent", "must be between 1 and 10")
	}
	return out, nil
}

func fieldError(field, message string) error {
	return &library.ValidationError{Fields: []library.FieldError{{Field: field, Message: message}}}
}

func validEdgeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return "", fieldError("name", "enter a name of up to 80 characters")
	}
	return name, nil
}

// ListEdgeWorkers lists the edge workers with their recent job counts (owner only).
func (s *server) ListEdgeWorkers(ctx context.Context, _ gen.ListEdgeWorkersRequestObject) (gen.ListEdgeWorkersResponseObject, error) {
	var raw json.RawMessage
	e := s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.edge_workers()`).Scan(&raw)
	})
	return decodePlatform[gen.ListEdgeWorkers200JSONResponse](raw, membership.Translate(e))
}

// CreateEdgeWorker registers a worker and returns its token, once (owner only).
func (s *server) CreateEdgeWorker(ctx context.Context, req gen.CreateEdgeWorkerRequestObject) (gen.CreateEdgeWorkerResponseObject, error) {
	b := req.Body
	name, e := validEdgeName(b.Name)
	if e != nil {
		return nil, e
	}
	hosts := b.AllowedHosts
	if hosts == nil {
		hosts = &[]string{}
	}
	perHour, concurrent := defaultEdgePerHour, defaultEdgeConcurrent
	if b.MaxPerHour != nil {
		perHour = *b.MaxPerHour
	}
	if b.MaxConcurrent != nil {
		concurrent = *b.MaxConcurrent
	}
	list, e := edgeWorkerLimits(hosts, &perHour, &concurrent)
	if e != nil {
		return nil, e
	}
	token, hash, e := edgetoken.New()
	if e != nil {
		return nil, e
	}
	id, e := uuid.NewV7()
	if e != nil {
		return nil, e
	}
	var raw json.RawMessage
	e = s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.create_edge_worker($1,$2,$3,$4,$5,$6)`, id, name, hash, list, perHour, concurrent).Scan(&raw)
	})
	worker, e := decodePlatform[gen.EdgeWorker](raw, membership.Translate(e))
	if e != nil {
		return nil, e
	}
	return gen.CreateEdgeWorker201JSONResponse{Worker: worker, Token: token}, nil
}

// UpdateEdgeWorker changes a worker's name, state, hosts or limits (owner only).
func (s *server) UpdateEdgeWorker(ctx context.Context, req gen.UpdateEdgeWorkerRequestObject) (gen.UpdateEdgeWorkerResponseObject, error) {
	version, e := parseIfMatch(req.Params.IfMatch)
	if e != nil {
		return nil, e
	}
	b := req.Body
	name := b.Name
	if name != nil {
		n, e := validEdgeName(*name)
		if e != nil {
			return nil, e
		}
		name = &n
	}
	list, e := edgeWorkerLimits(b.AllowedHosts, b.MaxPerHour, b.MaxConcurrent)
	if e != nil {
		return nil, e
	}
	var raw json.RawMessage
	e = s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.update_edge_worker($1,$2,$3,$4,$5,$6,$7)`, req.Id, name, b.Enabled, list, b.MaxPerHour, b.MaxConcurrent, version).Scan(&raw)
	})
	return decodePlatform[gen.UpdateEdgeWorker200JSONResponse](raw, membership.Translate(e))
}

// RotateEdgeWorkerToken replaces a worker's token and returns the new one, once (owner only).
func (s *server) RotateEdgeWorkerToken(ctx context.Context, req gen.RotateEdgeWorkerTokenRequestObject) (gen.RotateEdgeWorkerTokenResponseObject, error) {
	token, hash, e := edgetoken.New()
	if e != nil {
		return nil, e
	}
	var raw json.RawMessage
	e = s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT platform.rotate_edge_worker_token($1,$2)`, req.Id, hash).Scan(&raw)
	})
	worker, e := decodePlatform[gen.EdgeWorker](raw, membership.Translate(e))
	if e != nil {
		return nil, e
	}
	return gen.RotateEdgeWorkerToken200JSONResponse{Worker: worker, Token: token}, nil
}

// edgeAuth reads the worker token and answers 401 itself when it is missing.
func edgeAuth(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	token, ok := edgetoken.Bearer(r.Header.Get("Authorization"))
	if !ok {
		edgeUnauthorized(w, r)
		return nil, false
	}
	return edgetoken.Hash(token), true
}

func edgeUnauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="zapply-edge"`)
	httpx.WriteProblem(w, r, http.StatusUnauthorized, "")
}

// edgeClaim long-polls for the worker's next job: 200 with the job, or 204
// after claimWait (or when the server drains) so the worker polls again.
func (s *server) edgeClaim(w http.ResponseWriter, r *http.Request) {
	hash, ok := edgeAuth(w, r)
	if !ok {
		return
	}
	// A claim carries no body; refuse one rather than read it.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	wait := s.deps.EdgeClaimWait
	if wait <= 0 {
		wait = claimWait
	}
	timeout := time.NewTimer(wait)
	defer timeout.Stop()
	tick := time.NewTicker(claimPoll)
	defer tick.Stop()
	for {
		job, err := s.deps.Store.ClaimScrapeJob(r.Context(), hash)
		switch {
		case errors.Is(err, store.ErrEdgeUnauthorized):
			edgeUnauthorized(w, r)
			return
		case err != nil:
			if r.Context().Err() == nil {
				logx.From(r.Context()).Error("edge claim", "err", err)
				httpx.WriteProblem(w, r, http.StatusInternalServerError, "")
			}
			return
		case job != nil:
			httpx.WriteJSON(w, http.StatusOK, gen.EdgeJob{Id: job.ID, Url: job.URL})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.deps.Drain:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-timeout.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-tick.C:
		}
	}
}

// edgeResult records what a worker read for a job it claimed.
func (s *server) edgeResult(w http.ResponseWriter, r *http.Request) {
	hash, ok := edgeAuth(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResultBody))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "result too large")
		return
	case err != nil:
		httpx.WriteProblem(w, r, http.StatusBadRequest, "could not read the result")
		return
	}
	var body gen.EdgeJobResult
	if err := json.Unmarshal(raw, &body); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "malformed result")
		return
	}
	res := store.EdgeResult{
		Markdown:   deref(body.Markdown),
		RawHTML:    deref(body.RawHtml),
		StatusCode: deref(body.StatusCode),
		Error:      truncateRunes(deref(body.Error), maxResultError),
	}
	if len(res.Markdown) > maxResultMarkdown || len(res.RawHTML) > maxResultHTML {
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "markdown or raw_html too large")
		return
	}
	if res.StatusCode < 0 || res.StatusCode > 999 {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "status_code is not an HTTP status")
		return
	}
	if body.Metadata != nil {
		meta, err := json.Marshal(*body.Metadata)
		if err != nil || len(meta) > maxResultMetadata {
			httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "metadata too large")
			return
		}
		// Postgres jsonb refuses \u0000.
		if !bytes.Contains(meta, []byte(`\u0000`)) {
			res.Metadata = *body.Metadata
		}
	}
	res.Markdown = strings.ReplaceAll(res.Markdown, "\x00", "")
	res.RawHTML = strings.ReplaceAll(res.RawHTML, "\x00", "")
	res.Error = strings.ReplaceAll(res.Error, "\x00", "")
	res.Status = "done"
	if res.Error != "" || res.StatusCode >= 400 {
		res.Status = "failed"
	}
	err = s.deps.Store.FinishScrapeJob(r.Context(), hash, id, res)
	switch {
	case errors.Is(err, store.ErrEdgeUnauthorized):
		edgeUnauthorized(w, r)
	case errors.Is(err, store.ErrScrapeJobGone):
		httpx.WriteProblem(w, r, http.StatusNotFound, "")
	case err != nil:
		logx.From(r.Context()).Error("edge result", "err", err)
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
