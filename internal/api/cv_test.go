package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

func (c *client) raw(path string) (*http.Response, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func sectionKeys(content map[string]any) []string {
	var out []string
	for _, s := range content["sections"].([]any) {
		out = append(out, s.(map[string]any)["key"].(string))
	}
	return out
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestCVDraftExportAndPDF(t *testing.T) {
	alice, bob := newServer(t)
	profile := alice.must(200, "GET", "/v1/profile", nil)
	alice.must(200, "PUT", "/v1/profile", map[string]any{"full_name": "Ada Lovelace", "auto_match": false}, "If-Match", profile.Header.Get("ETag"))
	engineer := alice.must(201, "POST", "/v1/evidences", map[string]any{
		"kind": "job", "category": "paid", "title": "Engineer", "start": "2021-03", "is_current": true,
		"highlights": []any{map[string]any{"text": "First #thing"}, map[string]any{"text": "Second"}, map[string]any{"text": "Third"}},
	})
	alice.must(201, "POST", "/v1/evidences", map[string]any{
		"kind": "degree_program", "category": "education", "title": "BSc", "start": "2010", "end": "2014",
		"details": map[string]any{"degree_type": "bachelor", "status": "completed"},
	})
	brk := alice.must(201, "POST", "/v1/evidences", map[string]any{
		"kind": "career_break", "category": "unpaid", "title": "Caregiving", "start": "2015", "end": "2016", "auto_include": false,
	}).Body["id"].(string)
	job := alice.must(201, "POST", "/v1/jobs", map[string]any{"title": "Data engineer", "company": "Acme/Labs"}).Body["id"].(string)
	cv := "/v1/jobs/" + job + "/cv"

	got := alice.must(200, "GET", cv, nil)
	if got.Header.Get("ETag") != `"1"` || got.Body["match"] != nil {
		t.Fatalf("first read: %v %v", got.Header.Get("ETag"), got.Body["match"])
	}
	content := got.Body["content"].(map[string]any)
	if content["profile"].(map[string]any)["name"] != "Ada Lovelace" || !slices.Equal(sectionKeys(content), []string{"experience", "education"}) {
		t.Fatalf("content %v", content)
	}
	if trim := strs(got.Body["trim"]); len(trim) != 4 {
		t.Fatalf("trim = %v, want two highlights and two entries", trim)
	}
	if tpl := got.Body["template"].(map[string]any); tpl["name"] != "classic" || len(tpl["assets"].(map[string]any)["fonts"].([]any)) == 0 {
		t.Fatalf("template %v", tpl)
	}
	bob.must(404, "GET", cv, nil)
	bob.must(404, "GET", cv+"/exports", nil)

	draft := map[string]any{"template": "classic", "locale": "es", "page_budget": 1, "picks": map[string]any{"include": []string{brk}, "exclude": []string{}}}
	saved := alice.must(200, "PUT", cv, draft, "If-Match", `"1"`)
	if saved.Header.Get("ETag") != `"2"` || !slices.Contains(sectionKeys(saved.Body["content"].(map[string]any)), "unpaid") {
		t.Fatalf("saved %v", saved.Body)
	}
	alice.must(412, "PUT", cv, draft, "If-Match", `"1"`)
	bob.must(404, "PUT", cv, draft, "If-Match", `"1"`)
	for field, bad := range map[string]map[string]any{
		"locale":      {"template": "classic", "locale": "fr", "page_budget": 1, "picks": draft["picks"]},
		"page_budget": {"template": "classic", "locale": "en", "page_budget": 4, "picks": draft["picks"]},
		"picks":       {"template": "classic", "locale": "en", "page_budget": 1, "picks": map[string]any{"include": []string{brk}, "exclude": []string{brk}}},
		"template":    {"template": "fancy", "locale": "en", "page_budget": 1, "picks": draft["picks"]},
	} {
		if r := alice.do("PUT", cv, bad, "If-Match", `"2"`); r.Status != 422 {
			t.Errorf("%s: %d %v", field, r.Status, r.Body)
		}
	}

	hash := saved.Body["content_hash"].(string)
	trim := strs(saved.Body["trim"])
	export := map[string]any{"draft_version": 2, "content_hash": hash, "dropped": trim[:1], "pages": 2}
	x := alice.must(201, "POST", cv+"/exports", export, "Idempotency-Key", "x1")
	id := x.Body["id"].(string)
	if x.Body["overflow"] != true || x.Body["template_version"] != float64(1) || x.Body["locale"] != "es" {
		t.Fatalf("export %v", x.Body)
	}
	if strings.Contains(string(mustJSON(t, x.Body["content"])), trim[0]) {
		t.Fatal("the dropped item is still in the snapshot")
	}
	if replay := alice.must(201, "POST", cv+"/exports", export, "Idempotency-Key", "x1"); replay.Body["id"] != id {
		t.Fatal("idempotent replay created a second export")
	}
	alice.must(412, "POST", cv+"/exports", map[string]any{"draft_version": 1, "content_hash": hash, "dropped": []string{}, "pages": 1})
	alice.must(409, "POST", cv+"/exports", map[string]any{"draft_version": 2, "content_hash": "stale", "dropped": []string{}, "pages": 1})
	alice.must(422, "POST", cv+"/exports", map[string]any{"draft_version": 2, "content_hash": hash, "dropped": []string{brk}, "pages": 1})
	bob.must(404, "POST", cv+"/exports", export)

	list := alice.must(200, "GET", cv+"/exports", nil).Body["items"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["content"] != nil {
		t.Fatalf("list %v", list)
	}
	if alice.must(200, "GET", "/v1/cv-exports/"+id, nil).Body["content"] == nil {
		t.Fatal("export detail lacks content")
	}
	bob.must(404, "GET", "/v1/cv-exports/"+id, nil)
	bob.must(404, "GET", "/v1/cv-exports/"+id+"/pdf", nil)

	// Server-side compile: needs typst 0.14.2 on PATH (CI puts it there).
	res, pdf := alice.raw("/v1/cv-exports/" + id + "/pdf")
	switch {
	case res.StatusCode == 200:
		if !bytes.HasPrefix(pdf, []byte("%PDF-")) || res.Header.Get("Content-Disposition") != `attachment; filename="Ada Lovelace - AcmeLabs.pdf"` {
			t.Fatalf("pdf: %q %q", res.Header.Get("Content-Disposition"), pdf[:min(len(pdf), 20)])
		}
		if _, again := alice.raw("/v1/cv-exports/" + id + "/pdf"); !bytes.Equal(pdf, again) {
			t.Fatal("recompiling an export changed the PDF")
		}
	case res.StatusCode == 503 && os.Getenv("ZAPPLY_REQUIRE_TYPST") != "1":
		t.Log("no typst 0.14.2 on PATH; server compile skipped")
	default:
		t.Fatalf("pdf: %d %s", res.StatusCode, pdf)
	}
	// Library edits and deletions must never rewrite an existing snapshot.
	snapshot := alice.must(200, "GET", "/v1/cv-exports/"+id, nil).Body["content"]
	alice.must(204, "DELETE", "/v1/evidences/"+engineer.Body["id"].(string), nil, "If-Match", engineer.Header.Get("ETag"))
	if !bytes.Equal(mustJSON(t, snapshot), mustJSON(t, alice.must(200, "GET", "/v1/cv-exports/"+id, nil).Body["content"])) {
		t.Fatal("deleting library evidence changed an exported CV")
	}
	alice.must(409, "POST", cv+"/exports", export)
	if res.StatusCode == 200 {
		if _, again := alice.raw("/v1/cv-exports/" + id + "/pdf"); !bytes.Equal(pdf, again) {
			t.Fatal("library deletion changed the exported PDF")
		}
	}
}

func TestTemplateAssets(t *testing.T) {
	alice, _ := newServer(t)
	res, body := alice.raw("/assets/templates/classic/1/main.typ")
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") || !strings.Contains(string(body), "json(\"/content.json\")") {
		t.Fatalf("main.typ: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("a published version must be immutable: %s", res.Header.Get("Cache-Control"))
	}
	res, _ = alice.raw("/assets/templates/fonts/source-sans-3/3.052/SourceSans3-Regular.ttf")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "font/ttf" {
		t.Fatalf("font: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	for _, p := range []string{"/assets/templates/", "/assets/templates/classic/1/", "/assets/templates/nope.typ"} {
		if res, _ := alice.raw(p); res.StatusCode != 404 {
			t.Errorf("%s = %d", p, res.StatusCode)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCVConcurrentExportRetries(t *testing.T) {
	alice, _ := newServer(t)
	job := alice.must(201, "POST", "/v1/jobs", map[string]any{"title": "Engineer", "company": "Acme"}).Body["id"].(string)
	cv := "/v1/jobs/" + job + "/cv"
	p := alice.must(200, "GET", cv, nil)
	digest := strings.Repeat("a", 64)
	body := map[string]any{"draft_version": 1, "content_hash": p.Body["content_hash"], "dropped": []string{}, "pages": 1, "client_sha256": digest}
	results := make(chan response, 4)
	for range 4 {
		go func() { results <- alice.do("POST", cv+"/exports", body, "Idempotency-Key", "concurrent") }()
	}
	var id string
	for range 4 {
		r := <-results
		if r.Status != 201 || r.Body["client_sha256"] != digest {
			t.Fatalf("export retry: %d %v", r.Status, r.Body)
		}
		got := r.Body["id"].(string)
		if id != "" && id != got {
			t.Fatal("concurrent retries created multiple exports")
		}
		id = got
	}
	if list := alice.must(200, "GET", cv+"/exports", nil).Body["items"].([]any); len(list) != 1 {
		t.Fatalf("want one snapshot, got %d", len(list))
	}
}
