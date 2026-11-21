package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/diegohh0411/zapply/internal/testdb"
)

func TestPostingExtractionLifecycle(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	firecrawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
			"markdown": "# Staff Engineer\n\nAcme is hiring. Remote. $150,000–$180,000 a year.", "rawHtml": "<p></p>",
			"metadata": map[string]any{"statusCode": 200}}})
	}))
	defer firecrawl.Close()
	srv := httptest.NewServer(api.Handler(api.Deps{Store: db.Store, Verifier: auth.NewDev(secret, "authenticated"),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)), Postings: posting.New(firecrawl.URL, "")}))
	defer srv.Close()
	token, err := auth.MintDev(secret, "authenticated", "posting-user", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: srv.URL, token: token}
	admin, err := pgx.Connect(ctx, db.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	read := func() string {
		body := c.must(200, "POST", "/v1/postings/read", map[string]any{"url": "https://jobs.example.com/1"}).Body
		id, _ := body["extraction_id"].(string)
		return id
	}
	poll := func(id string) map[string]any {
		return c.must(200, "GET", "/v1/postings/extractions/"+id, nil).Body
	}

	id := read()
	if id == "" {
		t.Fatal("no extraction started with the default model")
	}
	if got := poll(id); got["status"] != "pending" {
		t.Fatalf("new extraction = %v", got)
	}
	answers := `{"title":{"type":"text","value":"Staff Engineer","confidence":0.97},
	 "work_mode":{"type":"text","value":"Remote","confidence":0.9},
	 "salary_min":{"type":"number","value":150000,"confidence":0.9},
	 "salary_max":{"type":"number","value":180000,"confidence":0.9},
	 "salary_period":{"type":"text","value":"a year","confidence":0.8}}`
	if _, err := admin.Exec(ctx, `UPDATE model_operation SET status='completed',result=$2 WHERE id=$1`, id, answers); err != nil {
		t.Fatal(err)
	}
	done := poll(id)
	fields, _ := done["fields"].(map[string]any)
	if done["status"] != "completed" || fields["title"] != "Staff Engineer" || fields["work_mode"] != "remote" ||
		fields["salary_min"] != float64(150000) || fields["salary_period"] != "year" {
		t.Fatalf("completed extraction = %v", done)
	}
	if certainty, _ := done["certainty"].(map[string]any); certainty["title"] != 0.97 {
		t.Fatalf("certainty = %v", done["certainty"])
	}

	// A request that cannot run now is cancelled, not left to run later.
	blocked := read()
	if _, err := admin.Exec(ctx, `UPDATE model_operation SET status='blocked',blocked_reason='membership_required' WHERE id=$1`, blocked); err != nil {
		t.Fatal(err)
	}
	if got := poll(blocked); got["status"] != "unavailable" || got["reason"] != "membership_required" {
		t.Fatalf("blocked extraction = %v", got)
	}
	var status string
	if err := admin.QueryRow(ctx, `SELECT status FROM model_operation WHERE id=$1`, blocked).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("blocked extraction left %q, %v", status, err)
	}

	// Another member cannot read it, and without a model nothing starts.
	other, _ := auth.MintDev(secret, "authenticated", "someone-else", time.Hour)
	(&client{t: t, base: srv.URL, token: other}).must(404, "GET", "/v1/postings/extractions/"+id, nil)
	if _, err := admin.Exec(ctx, `UPDATE platform.model_settings SET extraction_model=NULL`); err != nil {
		t.Fatal(err)
	}
	if id := read(); id != "" {
		t.Fatalf("extraction started without a model: %s", id)
	}
}
