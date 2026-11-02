package api_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/render"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
	"github.com/jackc/pgx/v5"
)

func TestSummaryReplacesInputsAndProtectsManualText(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.Handler(api.Deps{Store: db.Store, Verifier: auth.NewDev(secret, "authenticated"), Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}))
	defer srv.Close()
	token, err := auth.MintDev(secret, "authenticated", "summary-user", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: srv.URL, token: token}
	admin, err := pgx.Connect(ctx, db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err = admin.Exec(ctx, `UPDATE platform.model_settings SET summary_model='test/model',summary_endpoint='test'`); err != nil {
		t.Fatal(err)
	}
	profile := c.must(200, "GET", "/v1/profile", nil)
	c.must(200, "PUT", "/v1/profile", map[string]any{"full_name": "Ada", "auto_match": false}, "If-Match", profile.Header.Get("ETag"))
	c.must(201, "POST", "/v1/evidences", map[string]any{"kind": "job", "category": "paid", "title": "Engineer", "start": "2024-01", "highlights": []any{map[string]any{"text": "Built a compiler"}}})
	job := c.must(201, "POST", "/v1/jobs", map[string]any{"title": "Engineer", "description": "Build compilers"}).Body["id"].(string)
	path := "/v1/jobs/" + job + "/cv"
	first := c.must(200, "GET", path, nil)
	if first.Body["summary_status"] != "pending" {
		t.Fatalf("summary not queued: %v", first.Body)
	}
	var old string
	if err = admin.QueryRow(ctx, `SELECT operation_id::text FROM cv_summary WHERE job_id=$1`, job).Scan(&old); err != nil {
		t.Fatal(err)
	}
	// Repeated reads reuse the same immutable request.
	c.must(200, "GET", path, nil)
	var n int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM model_operation WHERE kind='summary'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate generation: %d %v", n, err)
	}
	draft := first.Body["draft"].(map[string]any)
	draft["summary"] = "My manually written summary"
	saved := c.must(200, "PUT", path, draft, "If-Match", first.Header.Get("ETag"))
	if got := saved.Body["content"].(map[string]any)["profile"].(map[string]any)["summary"]; got != "My manually written summary" {
		t.Fatalf("lost manual text: %v", got)
	}
	var status string
	if err = admin.QueryRow(ctx, `SELECT status FROM model_operation WHERE id=$1`, old).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("old run not cancelled: %s %v", status, err)
	}
	// A job change invalidates the snapshot, even with no browser request.
	j := c.must(200, "GET", "/v1/jobs/"+job, nil)
	j.Body["description"] = "Build distributed compilers"
	c.must(200, "PUT", "/v1/jobs/"+job, j.Body, "If-Match", j.Header.Get("ETag"))
	if _, err = admin.Exec(ctx, `UPDATE cv_summary SET changed_at=now()-interval '1 second' WHERE job_id=$1`, job); err != nil {
		t.Fatal(err)
	}
	workerStore, err := store.Open(ctx, db.WorkerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer workerStore.Close()
	if err = render.NewService(workerStore, nil).RefreshSummaries(ctx); err != nil {
		t.Fatal(err)
	}
	var dirty bool
	if err = admin.QueryRow(ctx, `SELECT dirty FROM cv_summary WHERE job_id=$1`, job).Scan(&dirty); err != nil || dirty {
		t.Fatalf("background refresh failed: %v %v", dirty, err)
	}
}
