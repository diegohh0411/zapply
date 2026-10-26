package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/testdb"
)

// client is a tiny JSON client for one user against the test server.
type client struct {
	t     *testing.T
	base  string
	token string
}

type response struct {
	Status int
	Header http.Header
	Body   map[string]any
	List   []any
}

func newServer(t *testing.T) (alice, bob *client) {
	t.Helper()
	db := testdb.New(t)
	srv := httptest.NewServer(api.Handler(api.Deps{
		Store:    db.Store,
		Verifier: auth.NewDev(secret, "authenticated"),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(srv.Close)
	mk := func(sub string) *client {
		tok, err := auth.MintDev(secret, "authenticated", sub, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: srv.URL, token: tok}
	}
	return mk("alice"), mk("bob")
}

func (c *client) do(method, path string, body any, headers ...string) response {
	c.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := response{Status: res.StatusCode, Header: res.Header}
	if len(raw) > 0 {
		if raw[0] == '[' {
			_ = json.Unmarshal(raw, &out.List)
		} else if err := json.Unmarshal(raw, &out.Body); err != nil {
			c.t.Fatalf("%s %s: non-JSON body %q", method, path, raw)
		}
	}
	return out
}

func (c *client) must(want int, method, path string, body any, headers ...string) response {
	c.t.Helper()
	r := c.do(method, path, body, headers...)
	if r.Status != want {
		c.t.Fatalf("%s %s = %d, want %d: %v", method, path, r.Status, want, r.Body)
	}
	return r
}

func hasFieldError(r response, field string) bool {
	errs, _ := r.Body["errors"].([]any)
	for _, e := range errs {
		if m, _ := e.(map[string]any); m["field"] == field {
			return true
		}
	}
	return false
}

func TestKindsCatalog(t *testing.T) {
	alice, _ := newServer(t)
	r := alice.must(200, "GET", "/v1/kinds", nil)
	if len(r.List) != 22 {
		t.Fatalf("kinds = %d, want 22", len(r.List))
	}
	first := r.List[0].(map[string]any)
	schema := first["details_schema"].(map[string]any)
	if first["kind"] != "degree_program" || schema["additionalProperties"] != false {
		t.Fatalf("first kind = %v", first)
	}
}

func TestProfileConcurrency(t *testing.T) {
	alice, _ := newServer(t)
	r := alice.must(200, "GET", "/v1/profile", nil)
	if r.Header.Get("ETag") != `"0"` || r.Body["version"].(float64) != 0 {
		t.Fatalf("empty profile: etag %q body %v", r.Header.Get("ETag"), r.Body)
	}
	body := map[string]any{"full_name": "Ada Lovelace", "links": []any{map[string]any{"label": "Site", "url": "https://ada.example"}}}

	alice.must(428, "PUT", "/v1/profile", body)
	r = alice.must(200, "PUT", "/v1/profile", body, "If-Match", `"0"`)
	if r.Header.Get("ETag") != `"1"` || r.Body["full_name"] != "Ada Lovelace" {
		t.Fatalf("saved profile: %v", r.Body)
	}
	alice.must(412, "PUT", "/v1/profile", body, "If-Match", `"0"`)
	alice.must(200, "PUT", "/v1/profile", body, "If-Match", `"1"`)

	bad := alice.must(422, "PUT", "/v1/profile", map[string]any{"email": "nope"}, "If-Match", `"2"`)
	if !hasFieldError(bad, "email") {
		t.Fatalf("errors = %v", bad.Body)
	}
}

func TestEvidenceAPI(t *testing.T) {
	alice, bob := newServer(t)

	org := alice.must(201, "POST", "/v1/organizations", map[string]any{"name": "Acme", "type": "company"})
	orgID := org.Body["id"].(string)
	alice.must(409, "POST", "/v1/organizations", map[string]any{"name": "  acme "})

	job := map[string]any{
		"kind": "job", "category": "paid", "organization_id": orgID, "title": "Engineer",
		"start": "2021-03", "is_current": true, "skills": []string{"Go"},
		"highlights": []any{map[string]any{"text": "Cut deploy time", "metric_value": 40, "metric_unit": "%"}},
	}
	created := alice.must(201, "POST", "/v1/evidences", job, "Idempotency-Key", "k1")
	id := created.Body["id"].(string)
	if created.Header.Get("ETag") != `"1"` || created.Body["form"] != "engagement" || created.Body["start"] != "2021-03" {
		t.Fatalf("created = %v", created.Body)
	}

	// Same key and body: the same evidence. Same key, other body: rejected.
	replay := alice.must(201, "POST", "/v1/evidences", job, "Idempotency-Key", "k1")
	if replay.Body["id"] != id {
		t.Fatalf("replay created %v, want %s", replay.Body["id"], id)
	}
	job["title"] = "Other"
	alice.must(422, "POST", "/v1/evidences", job, "Idempotency-Key", "k1")
	if n := len(alice.must(200, "GET", "/v1/evidences", nil).Body["items"].([]any)); n != 1 {
		t.Fatalf("evidences after replays = %d, want 1", n)
	}

	alice.must(409, "DELETE", "/v1/organizations/"+orgID, nil)

	// Validation errors name every bad field.
	bad := alice.must(422, "POST", "/v1/evidences", map[string]any{
		"kind": "license", "category": "paid", "title": "", "details": map[string]any{"color": "red"},
	})
	for _, f := range []string{"category", "title", "details.issuer", "details.color"} {
		if !hasFieldError(bad, f) {
			t.Errorf("missing field error %q in %v", f, bad.Body["errors"])
		}
	}

	// Child inherits the category; PUT needs the current ETag.
	child := alice.must(201, "POST", "/v1/evidences", map[string]any{"kind": "project", "parent_id": id, "title": "Billing"})
	if child.Body["category"] != "paid" {
		t.Fatalf("child = %v", child.Body)
	}
	got := alice.must(200, "GET", "/v1/evidences/"+id, nil)
	if kids := got.Body["children"].([]any); len(kids) != 1 {
		t.Fatalf("children = %v", kids)
	}
	hl := got.Body["highlights"].([]any)[0].(map[string]any)
	update := map[string]any{
		"kind": "job", "category": "paid", "title": "Senior Engineer", "start": "2021-03", "is_current": true,
		"highlights": []any{map[string]any{"id": hl["id"], "text": "Cut deploy time by 40%"}},
	}
	alice.must(428, "PUT", "/v1/evidences/"+id, update)
	put := alice.must(200, "PUT", "/v1/evidences/"+id, update, "If-Match", got.Header.Get("ETag"))
	if put.Header.Get("ETag") != `"2"` || put.Body["highlights"].([]any)[0].(map[string]any)["id"] != hl["id"] {
		t.Fatalf("put = %v", put.Body)
	}
	alice.must(412, "PUT", "/v1/evidences/"+id, update, "If-Match", got.Header.Get("ETag"))

	// Other tenants see nothing.
	bob.must(404, "GET", "/v1/evidences/"+id, nil)
	bob.must(404, "PUT", "/v1/evidences/"+id, update, "If-Match", `"2"`)
	bob.must(422, "POST", "/v1/evidences", map[string]any{"kind": "project", "parent_id": id, "title": "x"})
	if n := len(bob.must(200, "GET", "/v1/evidences", nil).Body["items"].([]any)); n != 0 {
		t.Fatalf("bob lists %d evidences", n)
	}

	// Archive hides from the default list.
	alice.must(200, "POST", "/v1/evidences/"+child.Body["id"].(string)+"/archive", nil)
	if n := len(alice.must(200, "GET", "/v1/evidences?parent_id="+id, nil).Body["items"].([]any)); n != 0 {
		t.Fatalf("archived child still listed")
	}
	if n := len(alice.must(200, "GET", "/v1/evidences?parent_id="+id+"&include_archived=true", nil).Body["items"].([]any)); n != 1 {
		t.Fatalf("include_archived missed the child")
	}

	alice.must(409, "DELETE", "/v1/evidences/"+id, nil)
	alice.must(204, "DELETE", "/v1/evidences/"+id+"?cascade=true", nil)
	alice.must(404, "GET", "/v1/evidences/"+id, nil)
}

func TestEvidencePaginationAndMove(t *testing.T) {
	alice, _ := newServer(t)
	var ids []string
	for _, title := range []string{"A", "B", "C", "D", "E"} {
		r := alice.must(201, "POST", "/v1/evidences", map[string]any{
			"kind": "volunteering", "category": "unpaid", "title": title, "start": "2020",
		})
		ids = append(ids, r.Body["id"].(string))
	}

	titles := func() []string {
		var out []string
		cursor := ""
		for range 10 {
			path := "/v1/evidences?limit=2"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			r := alice.must(200, "GET", path, nil)
			for _, it := range r.Body["items"].([]any) {
				out = append(out, it.(map[string]any)["title"].(string))
			}
			next, _ := r.Body["next_cursor"].(string)
			if next == "" {
				return out
			}
			cursor = next
		}
		t.Fatal("pagination did not end")
		return nil
	}
	if got := strings.Join(titles(), ""); got != "ABCDE" {
		t.Fatalf("order = %s", got)
	}

	// Move E before B, then A after D.
	alice.must(200, "POST", "/v1/evidences/"+ids[4]+"/move", map[string]any{"before_id": ids[1]})
	alice.must(200, "POST", "/v1/evidences/"+ids[0]+"/move", map[string]any{"after_id": ids[3]})
	if got := strings.Join(titles(), ""); got != "EBCDA" {
		t.Fatalf("order after moves = %s", got)
	}
	alice.must(422, "POST", "/v1/evidences/"+ids[0]+"/move", map[string]any{"after_id": ids[1], "before_id": ids[2]})
	alice.must(422, "GET", "/v1/evidences?cursor=garbage!", nil)
}

func TestRequestErrors(t *testing.T) {
	alice, _ := newServer(t)
	alice.must(400, "POST", "/v1/evidences", "{not json")
	alice.must(400, "GET", "/v1/evidences/not-a-uuid", nil)
	alice.must(404, "GET", "/v1/evidences/01a17616-c0fb-7c40-bb36-2a3fec92209a", nil)
	alice.must(413, "POST", "/v1/evidences", `{"kind":"job","title":"`+strings.Repeat("x", 2<<20)+`"}`)
	alice.must(404, "GET", "/v1/nope", nil)
}

func TestBodyLimits(t *testing.T) {
	alice, _ := newServer(t)
	valid := `{"kind":"project","category":"paid","title":"x"}`
	alice.must(413, "POST", "/v1/evidences", valid+strings.Repeat(" ", 2<<20))
	alice.must(400, "POST", "/v1/evidences", valid+valid)
	alice.must(201, "POST", "/v1/evidences", valid+"\n")
	r := alice.must(422, "POST", "/v1/evidences", `{"kind":"project","category":"paid","title":"x","highlights":[`+strings.Repeat(`{"text":""},`, 20000)+`{"text":""}]}`)
	if n := len(r.Body["errors"].([]any)); n > 51 {
		t.Fatalf("%d errors in response", n)
	}
	alice.must(422, "GET", "/v1/evidences?category=bogus", nil)
}
