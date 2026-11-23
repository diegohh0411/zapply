package api_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/posting"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/testdb"
)

// edgeCall is a worker's request: its own bearer token, not a user's.
func edgeCall(t *testing.T, base, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

func TestEdgeWorkers(t *testing.T) {
	db := testdb.New(t)
	ring, err := secrets.New(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	chain := posting.NewChain(posting.NewDBSource(db.Store, ring))
	srv := httptest.NewServer(api.Handler(api.Deps{
		Store: db.Store, Keys: ring, Postings: posting.New(chain),
		Verifier:      auth.NewDev(secret, "authenticated"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		EdgeClaimWait: 300 * time.Millisecond,
	}))
	t.Cleanup(srv.Close)
	mk := func(sub string) *client {
		tok, err := auth.MintDev(secret, "authenticated", sub, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: srv.URL, token: tok}
	}
	alice, bob := mk("alice"), mk("bob")

	// Members cannot see or manage workers.
	bob.must(403, "GET", "/v1/admin/edge-workers", nil)
	bob.must(403, "POST", "/v1/admin/edge-workers", map[string]any{"name": "laptop"})
	owner := uuid.MustParse(alice.must(200, "GET", "/v1/me", nil).Body["id"].(string))
	if err := store.SetOwner(t.Context(), db.AdminURL, owner, true); err != nil {
		t.Fatal(err)
	}

	// Validation names the field at fault.
	for field, body := range map[string]map[string]any{
		"name":           {"name": " "},
		"allowed_hosts":  {"name": "x", "allowed_hosts": []string{"https://linkedin.com"}},
		"max_per_hour":   {"name": "x", "max_per_hour": 0},
		"max_concurrent": {"name": "x", "max_concurrent": 11},
	} {
		if r := alice.must(422, "POST", "/v1/admin/edge-workers", body); !hasFieldError(r, field) {
			t.Errorf("no error for %s: %v", field, r.Body)
		}
	}

	created := alice.must(201, "POST", "/v1/admin/edge-workers", map[string]any{"name": "laptop", "allowed_hosts": []string{"Example.com", "example.com"}, "max_per_hour": 5})
	token, _ := created.Body["token"].(string)
	worker := created.Body["worker"].(map[string]any)
	workerID := worker["id"].(string)
	if !strings.HasPrefix(token, "zedge_") || worker["max_per_hour"] != float64(5) || worker["max_concurrent"] != float64(1) || len(worker["allowed_hosts"].([]any)) != 1 || worker["enabled"] != true {
		t.Fatalf("created = %v", created.Body)
	}
	if list := alice.must(200, "GET", "/v1/admin/edge-workers", nil); len(list.List) != 1 || strings.Contains(toJSON(list.List), "token") {
		t.Fatalf("list = %v", list.List)
	}

	// Workers authenticate with their token only.
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/claim", "", nil); status != 401 {
		t.Errorf("claim without a token = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/claim", alice.token, nil); status != 401 {
		t.Errorf("claim with a user's JWT = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/claim", "zedge_unknown", nil); status != 401 {
		t.Errorf("claim with an unknown token = %d", status)
	}
	// And a worker token is no user credential.
	if status, _ := edgeCall(t, srv.URL, "GET", "/v1/admin/edge-workers", token, nil); status != 401 {
		t.Errorf("admin API with a worker token = %d", status)
	}
	// An empty queue answers 204 after the wait.
	start := time.Now()
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/claim", token, nil); status != 204 || time.Since(start) < 250*time.Millisecond {
		t.Errorf("empty claim = %d after %v", status, time.Since(start))
	}

	// The edge provider reads through the worker.
	alice.must(200, "PUT", "/v1/admin/scraper-settings", map[string]any{"providers": []any{map[string]any{"kind": "edge"}}, "rules": []any{}}, "If-Match", "1")
	read := func(rawURL string) <-chan response {
		out := make(chan response, 1)
		go func() { out <- alice.do("POST", "/v1/postings/read", map[string]any{"url": rawURL, "text": "pasted"}) }()
		return out
	}
	claim := func() map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			status, raw := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/claim", token, nil)
			if status == 200 {
				var job map[string]any
				if err := json.Unmarshal(raw, &job); err != nil {
					t.Fatal(err)
				}
				return job
			}
		}
		t.Fatal("no job was queued")
		return nil
	}

	pending := read("https://www.example.com/jobs/1")
	job := claim()
	if job["url"] != "https://www.example.com/jobs/1" {
		t.Fatalf("job = %v", job)
	}
	jobID := job["id"].(string)
	path := "/v1/edge/jobs/" + jobID + "/result"

	// Bad results are refused; the job stays claimed.
	if status, _ := edgeCall(t, srv.URL, "POST", path, token, []byte(`{not json`)); status != 400 {
		t.Errorf("malformed result = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", path, token, []byte(`{"markdown":"`+strings.Repeat("a", 7<<20)+`"}`)); status != 413 {
		t.Errorf("oversized body = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", path, token, map[string]any{"markdown": strings.Repeat("a", (1<<20)+1)}); status != 413 {
		t.Errorf("oversized markdown = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", path, "zedge_unknown", map[string]any{"markdown": "x"}); status != 401 {
		t.Errorf("result with an unknown token = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/"+uuid.NewString()+"/result", token, map[string]any{"markdown": "x"}); status != 404 {
		t.Errorf("result for an unknown job = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", path, token, map[string]any{"markdown": "# Staff Engineer\nBuild things.", "raw_html": "<h1>x</h1>", "metadata": map[string]any{"ogTitle": "Staff Engineer"}, "status_code": 200}); status != 204 {
		t.Fatalf("result = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", path, token, map[string]any{"markdown": "again"}); status != 404 {
		t.Errorf("a second result = %d", status)
	}
	if r := <-pending; r.Status != 200 || r.Body["description"] != "# Staff Engineer\nBuild things." || r.Body["title"] != "Staff Engineer" {
		t.Fatalf("read = %d %v", r.Status, r.Body)
	}

	// A worker that reached the site but was refused makes the page unreadable.
	pending = read("https://www.example.com/jobs/2")
	job = claim()
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/"+job["id"].(string)+"/result", token, map[string]any{"status_code": 999, "error": "login wall"}); status != 204 {
		t.Fatalf("failed result = %d", status)
	}
	if r := <-pending; r.Status != 200 || r.Body["page_error"] != "login wall" {
		t.Fatalf("unreadable read = %d %v", r.Status, r.Body)
	}

	// A host the worker may not open is never queued: the read fails at once.
	start = time.Now()
	if r := <-read("https://www.elsewhere.org/jobs/3"); r.Status != 500 || time.Since(start) > 5*time.Second {
		t.Errorf("read for a host without a worker = %d after %v", r.Status, time.Since(start))
	}

	// Disabling and rotating cut the old token off.
	alice.must(200, "PATCH", "/v1/admin/edge-workers/"+workerID, map[string]any{"enabled": false}, "If-Match", "1")
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/claim", token, nil); status != 401 {
		t.Errorf("claim by a disabled worker = %d", status)
	}
	alice.must(412, "PATCH", "/v1/admin/edge-workers/"+workerID, map[string]any{"enabled": true}, "If-Match", "1")
	alice.must(200, "PATCH", "/v1/admin/edge-workers/"+workerID, map[string]any{"enabled": true, "max_per_hour": 7}, "If-Match", "2")
	rotated := alice.must(200, "POST", "/v1/admin/edge-workers/"+workerID+"/token", nil)
	newToken, _ := rotated.Body["token"].(string)
	if newToken == "" || newToken == token {
		t.Fatalf("rotated = %v", rotated.Body)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/claim", token, nil); status != 401 {
		t.Errorf("claim with the old token = %d", status)
	}
	if status, _ := edgeCall(t, srv.URL, "POST", "/v1/edge/jobs/claim", newToken, nil); status != 204 {
		t.Errorf("claim with the new token = %d", status)
	}
	alice.must(404, "POST", "/v1/admin/edge-workers/"+uuid.NewString()+"/token", nil)
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
