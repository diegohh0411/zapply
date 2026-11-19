package api_test

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/render"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/summary"
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
	me := c.must(200, "GET", "/v1/me", nil)
	userID := uuid.MustParse(me.Body["id"].(string))
	c.must(403, "PUT", "/v1/admin/model-settings", map[string]any{"decision_model": nil, "summary_rows": 3}, "If-Match", "1")
	if err = store.SetOwner(ctx, db.AdminURL, userID, true); err != nil {
		t.Fatal(err)
	}
	settings := c.must(200, "GET", "/v1/admin/model-settings", nil)
	if settings.Body["summary_rows"] != float64(2) {
		t.Fatalf("unexpected default: %v", settings.Body)
	}
	if _, ok := settings.Body["summary_max_output_tokens"]; ok {
		t.Fatalf("token cap still exposed: %v", settings.Body)
	}
	for _, rows := range []int{1, 2, 4, 3} {
		settings = c.must(200, "PUT", "/v1/admin/model-settings", map[string]any{"decision_model": nil, "summary_rows": rows}, "If-Match", fmt.Sprint(settings.Body["version"]))
		if settings.Body["summary_rows"] != float64(rows) {
			t.Fatalf("rows not saved: %v", settings.Body)
		}
	}
	for _, rows := range []int{0, 5, -1} {
		c.must(422, "PUT", "/v1/admin/model-settings", map[string]any{"decision_model": nil, "summary_rows": rows}, "If-Match", fmt.Sprint(settings.Body["version"]))
	}
	// Clients that omit the setting preserve it.
	settings = c.must(200, "PUT", "/v1/admin/model-settings", map[string]any{"decision_model": nil}, "If-Match", fmt.Sprint(settings.Body["version"]))
	if settings.Body["summary_rows"] != float64(3) {
		t.Fatal("omitted rows reset the setting")
	}
	if _, err = admin.Exec(ctx, `UPDATE platform.model_settings SET summary_model='test/model',summary_endpoint='test'`); err != nil {
		t.Fatal(err)
	}
	profile := c.must(200, "GET", "/v1/profile", nil)
	c.must(200, "PUT", "/v1/profile", map[string]any{"full_name": "Ada", "auto_match": false}, "If-Match", profile.Header.Get("ETag"))
	c.must(201, "POST", "/v1/evidences", map[string]any{"kind": "job", "category": "paid", "title": "Engineer", "start": "2024-01", "highlights": []any{map[string]any{"text": "Built a compiler"}}})
	job := c.must(201, "POST", "/v1/jobs", map[string]any{"title": "Engineer", "description": "Build compilers"}).Body["id"].(string)
	path := "/v1/jobs/" + job + "/cv"
	first := c.must(200, "GET", path, nil)
	if first.Body["summary_status"] != "waiting_for_fit" {
		t.Fatalf("summary queued before a fit: %v", first.Body)
	}
	// The browser reports the page fit it computed; only then is a summary written.
	fitKey, _ := first.Body["fit_key"].(string)
	first = c.must(200, "PUT", path+"/fit", map[string]any{"fit_key": fitKey, "dropped": []string{}})
	if first.Body["summary_status"] != "pending" {
		t.Fatalf("summary not queued after fit: %v", first.Body)
	}
	c.must(409, "PUT", path+"/fit", map[string]any{"fit_key": "stale", "dropped": []string{}})
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
	if first.Body["summary_rows"] != float64(3) {
		t.Fatalf("preview rows: %v", first.Body["summary_rows"])
	}
	var rows, tokens int
	if err = admin.QueryRow(ctx, `SELECT (body->>'rows')::int,(body->>'max_output_tokens')::int FROM model_request WHERE operation_id=$1`, old).Scan(&rows, &tokens); err != nil || rows != 3 || tokens != summary.OutputTokens {
		t.Fatalf("queued rows %d, tokens %d: %v", rows, tokens, err)
	}
	// A rows change changes the fit key, so the summary waits for a new fit.
	err = pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.user_id',$1,true)`, userID.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT platform.save_summary_settings('test/model','test',1,NULL)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	first = c.must(200, "GET", path, nil)
	if first.Body["summary_status"] != "waiting_for_fit" || first.Body["fit_key"] == fitKey {
		t.Fatalf("rows change kept the old fit: %v", first.Body)
	}
	fitKey, _ = first.Body["fit_key"].(string)
	first = c.must(200, "PUT", path+"/fit", map[string]any{"fit_key": fitKey, "dropped": []string{}})
	var replacement string
	if err = admin.QueryRow(ctx, `SELECT operation_id::text FROM cv_summary WHERE job_id=$1`, job).Scan(&replacement); err != nil || replacement == old {
		t.Fatalf("rows change did not replace snapshot: %v", err)
	}
	if err = admin.QueryRow(ctx, `SELECT (body->>'rows')::int FROM model_request WHERE operation_id=$1`, replacement).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("replacement rows: %d %v", rows, err)
	}
	old = replacement
	// An explicit thinking effort is stored and, unlike automatic, changes the snapshot.
	err = pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.user_id',$1,true)`, userID.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT platform.save_summary_settings('test/model','test',NULL,'high')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var effort string
	if err = admin.QueryRow(ctx, `SELECT platform.summary_settings()->>'reasoning_effort'`).Scan(&effort); err != nil || effort != "high" {
		t.Fatalf("saved effort = %q, err = %v", effort, err)
	}
	if _, err = admin.Exec(ctx, `UPDATE platform.model_settings SET summary_reasoning_effort='none'`); err == nil {
		t.Fatal("gateway none must not be stored; use off")
	}
	first = c.must(200, "GET", path, nil)
	if err = admin.QueryRow(ctx, `SELECT operation_id::text FROM cv_summary WHERE job_id=$1`, job).Scan(&replacement); err != nil || replacement == old {
		t.Fatalf("effort change did not replace snapshot: %v", err)
	}
	old = replacement
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
