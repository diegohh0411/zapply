package api_test

import (
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"
)

// statuses returns the user's statuses by name, and their ids in order.
func statuses(c *client) (map[string]string, []string) {
	c.t.Helper()
	r := c.must(200, "GET", "/v1/job-statuses", nil)
	byName := map[string]string{}
	var order []string
	for _, s := range r.List {
		m := s.(map[string]any)
		byName[m["name"].(string)] = m["id"].(string)
		order = append(order, m["id"].(string))
	}
	return byName, order
}

// column lists the ids of a status's jobs in board order.
func column(c *client, statusID string) []string {
	c.t.Helper()
	r := c.must(200, "GET", "/v1/jobs?sort=position&status_id="+statusID, nil)
	var ids []string
	for _, it := range r.Body["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return ids
}

func activities(c *client, jobID string) []map[string]any {
	c.t.Helper()
	r := c.must(200, "GET", "/v1/jobs/"+jobID+"/activities", nil)
	out := make([]map[string]any, len(r.List))
	for i, a := range r.List {
		out[i] = a.(map[string]any)
	}
	return out
}

func TestJobStatusesSeedOnce(t *testing.T) {
	alice, bob := newServer(t)
	// Concurrent first reads seed one set.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); alice.do("GET", "/v1/job-statuses", nil) }()
	}
	wg.Wait()
	byName, order := statuses(alice)
	if len(order) != 7 || byName["Bookmarked"] != order[0] || byName["Posting closed"] != order[6] {
		t.Fatalf("seeded = %v", byName)
	}

	// Create, rename, reorder.
	r := alice.must(201, "POST", "/v1/job-statuses", map[string]any{"name": "Take-home", "stage": "active", "color": "orange"})
	th := r.Body["id"].(string)
	alice.must(409, "POST", "/v1/job-statuses", map[string]any{"name": " take-HOME ", "stage": "active"})
	bad := alice.must(422, "POST", "/v1/job-statuses", map[string]any{"name": "", "stage": "nope", "color": "teal"})
	for _, f := range []string{"name", "stage", "color"} {
		if !hasFieldError(bad, f) {
			t.Fatalf("missing %s error: %v", f, bad.Body)
		}
	}
	alice.must(200, "POST", "/v1/job-statuses/"+th+"/move", map[string]any{"after_id": byName["Applied"]})
	_, order = statuses(alice)
	if order[2] != th {
		t.Fatalf("take-home at %v", order)
	}
	alice.must(200, "PATCH", "/v1/job-statuses/"+th, map[string]any{"name": "Assignment", "stage": "active"})

	// Bob has his own board and can't touch alice's.
	_, bobOrder := statuses(bob)
	if len(bobOrder) != 7 {
		t.Fatalf("bob statuses = %d", len(bobOrder))
	}
	bob.must(404, "PATCH", "/v1/job-statuses/"+th, map[string]any{"name": "x", "stage": "saved"})
}

func TestJobLifecycle(t *testing.T) {
	alice, bob := newServer(t)
	byName, _ := statuses(alice)

	r := alice.must(201, "POST", "/v1/jobs", map[string]any{
		"title": "Backend Engineer", "company": "Acme", "url": "HTTPS://Jobs.Acme.com/42/?utm_source=x&gh_jid=7#apply",
		"salary_min": 100000, "salary_max": 120000, "salary_currency": "usd", "salary_period": "year",
		"work_mode": "remote", "description": "Go and Postgres",
	})
	id := r.Body["id"].(string)
	if r.Header.Get("ETag") != `"1"` || r.Body["status_id"] != byName["Bookmarked"] || r.Body["salary_currency"] != "USD" ||
		r.Body["applied_on"] != nil {
		t.Fatalf("created = %v", r.Body)
	}

	// The same posting through another link is a duplicate.
	dup := alice.must(409, "POST", "/v1/jobs", map[string]any{"title": "Again", "url": "https://jobs.acme.com/42?gh_jid=7&utm_medium=y"})
	if dup.Body["existing_id"] != id {
		t.Fatalf("duplicate = %v", dup.Body)
	}
	// Another user may save it.
	bob.must(201, "POST", "/v1/jobs", map[string]any{"title": "Backend Engineer", "url": "https://jobs.acme.com/42?gh_jid=7"})

	bad := alice.must(422, "POST", "/v1/jobs", map[string]any{
		"title": " ", "url": "ftp://x", "salary_min": 5, "salary_max": 1, "work_mode": "space",
	})
	for _, f := range []string{"title", "url", "salary_max", "salary_currency", "work_mode"} {
		if !hasFieldError(bad, f) {
			t.Fatalf("missing %s error: %v", f, bad.Body)
		}
	}

	// Moving to an active stage records the change and sets applied_on.
	m := alice.must(200, "POST", "/v1/jobs/"+id+"/move", map[string]any{"status_id": byName["Applied"]})
	if m.Body["status_id"] != byName["Applied"] || m.Body["applied_on"] != time.Now().UTC().Format("2006-01-02") || m.Header.Get("ETag") != `"2"` {
		t.Fatalf("moved = %v", m.Body)
	}
	acts := activities(alice, id)
	if len(acts) != 2 || acts[0]["type"] != "status_changed" || acts[0]["from_status"] != "Bookmarked" || acts[0]["to_status"] != "Applied" ||
		acts[1]["type"] != "created" {
		t.Fatalf("activities = %v", acts)
	}

	// PUT replaces; a stale version fails; changing status_id moves it.
	put := map[string]any{"title": "Senior Backend Engineer", "company": "Acme", "status_id": byName["Interviewing"], "applied_on": "2026-10-01"}
	alice.must(428, "PUT", "/v1/jobs/"+id, put)
	alice.must(412, "PUT", "/v1/jobs/"+id, put, "If-Match", `"1"`)
	p := alice.must(200, "PUT", "/v1/jobs/"+id, put, "If-Match", `"2"`)
	if p.Body["title"] != "Senior Backend Engineer" || p.Body["status_id"] != byName["Interviewing"] ||
		p.Body["applied_on"] != "2026-10-01" || p.Body["url"] != "" || p.Body["salary_min"] != nil {
		t.Fatalf("put = %v", p.Body)
	}
	if acts = activities(alice, id); len(acts) != 3 || acts[0]["to_status"] != "Interviewing" {
		t.Fatalf("activities after put = %v", acts)
	}

	// Notes: create, edit, delete; generated entries are read-only.
	n := alice.must(201, "POST", "/v1/jobs/"+id+"/activities", map[string]any{"body": "Phone screen with Sam", "occurred_at": "2026-10-20T15:00:00Z"})
	nid := n.Body["id"].(string)
	alice.must(200, "PATCH", "/v1/jobs/"+id+"/activities/"+nid, map[string]any{"body": "Phone screen with Sam (went well)"})
	alice.must(409, "DELETE", "/v1/jobs/"+id+"/activities/"+acts[0]["id"].(string), nil)
	alice.must(422, "POST", "/v1/jobs/"+id+"/activities", map[string]any{"body": " "})
	bob.must(404, "GET", "/v1/jobs/"+id+"/activities", nil)
	alice.must(204, "DELETE", "/v1/jobs/"+id+"/activities/"+nid, nil)

	// Archive hides it from lists by default.
	alice.must(200, "POST", "/v1/jobs/"+id+"/archive", nil)
	if l := alice.must(200, "GET", "/v1/jobs", nil); len(l.Body["items"].([]any)) != 0 {
		t.Fatalf("archived job listed: %v", l.Body)
	}
	if l := alice.must(200, "GET", "/v1/jobs?include_archived=true", nil); len(l.Body["items"].([]any)) != 1 {
		t.Fatalf("include_archived: %v", l.Body)
	}
	alice.must(200, "POST", "/v1/jobs/"+id+"/unarchive", nil)

	got := alice.must(200, "GET", "/v1/jobs/"+id, nil)
	if got.Body["description"] != "" || got.Header.Get("ETag") == "" {
		t.Fatalf("get = %v", got.Body)
	}
	bob.must(404, "GET", "/v1/jobs/"+id, nil)
	bob.must(404, "DELETE", "/v1/jobs/"+id, nil)
	alice.must(204, "DELETE", "/v1/jobs/"+id, nil)
	alice.must(404, "GET", "/v1/jobs/"+id, nil)
}

func TestJobBoardOrdering(t *testing.T) {
	alice, _ := newServer(t)
	byName, _ := statuses(alice)
	saved := byName["Bookmarked"]
	var ids []string
	for i := range 3 {
		r := alice.must(201, "POST", "/v1/jobs", map[string]any{"title": fmt.Sprintf("Job %d", i)})
		ids = append(ids, r.Body["id"].(string))
	}
	// Newest first.
	if col := column(alice, saved); fmt.Sprint(col) != fmt.Sprint([]string{ids[2], ids[1], ids[0]}) {
		t.Fatalf("column = %v", col)
	}
	// Reorder within the column.
	alice.must(200, "POST", "/v1/jobs/"+ids[2]+"/move", map[string]any{"after_id": ids[0]})
	if col := column(alice, saved); fmt.Sprint(col) != fmt.Sprint([]string{ids[1], ids[0], ids[2]}) {
		t.Fatalf("after reorder = %v", col)
	}
	alice.must(422, "POST", "/v1/jobs/"+ids[2]+"/move", map[string]any{"after_id": ids[0], "before_id": ids[1]})
	alice.must(422, "POST", "/v1/jobs/"+ids[2]+"/move", map[string]any{"status_id": byName["Offer"], "after_id": ids[0]})
	// Same-column moves record no activity.
	if acts := activities(alice, ids[2]); len(acts) != 1 {
		t.Fatalf("activities = %v", acts)
	}

	// Closing a saved posting doesn't count as applying.
	c := alice.must(200, "POST", "/v1/jobs/"+ids[0]+"/move", map[string]any{"status_id": byName["Posting closed"]})
	if c.Body["applied_on"] != nil {
		t.Fatalf("closed = %v", c.Body)
	}

	// Deleting a status with jobs needs move_to; the last status can't go.
	alice.must(409, "DELETE", "/v1/job-statuses/"+saved, nil)
	alice.must(422, "DELETE", "/v1/job-statuses/"+saved+"?move_to="+saved, nil)
	alice.must(204, "DELETE", "/v1/job-statuses/"+saved+"?move_to="+byName["Dropped"], nil)
	if col := column(alice, byName["Dropped"]); fmt.Sprint(col) != fmt.Sprint([]string{ids[1], ids[2]}) {
		t.Fatalf("moved column = %v", col)
	}
	if acts := activities(alice, ids[1]); acts[0]["from_status"] != "Bookmarked" || acts[0]["to_status"] != "Dropped" {
		t.Fatalf("activities = %v", acts)
	}
	_, order := statuses(alice)
	for _, sid := range order[:len(order)-1] {
		alice.must(204, "DELETE", "/v1/job-statuses/"+sid+"?move_to="+order[len(order)-1], nil)
	}
	alice.must(409, "DELETE", "/v1/job-statuses/"+order[len(order)-1], nil)
}

func TestJobConcurrentInsertsKeepDistinctPositions(t *testing.T) {
	alice, _ := newServer(t)
	byName, _ := statuses(alice)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			alice.must(201, "POST", "/v1/jobs", map[string]any{"title": fmt.Sprintf("J%d", i)})
		}()
	}
	wg.Wait()
	r := alice.must(200, "GET", "/v1/jobs?sort=position&status_id="+byName["Bookmarked"], nil)
	seen := map[string]bool{}
	for _, it := range r.Body["items"].([]any) {
		p := it.(map[string]any)["position"].(string)
		if seen[p] {
			t.Fatalf("duplicate position %q", p)
		}
		seen[p] = true
	}
	if len(seen) != 10 {
		t.Fatalf("jobs = %d", len(seen))
	}
}

func TestJobListFiltersAndPages(t *testing.T) {
	alice, _ := newServer(t)
	byName, _ := statuses(alice)
	for i := range 5 {
		body := map[string]any{"title": fmt.Sprintf("Engineer %d", i), "company": "Globex"}
		if i%2 == 0 {
			body["status_id"] = byName["Applied"]
			body["company"] = "100%_Pure"
		}
		alice.must(201, "POST", "/v1/jobs", body)
	}

	var all []string
	cursor := ""
	for {
		r := alice.must(200, "GET", "/v1/jobs?limit=2&cursor="+url.QueryEscape(cursor), nil)
		for _, it := range r.Body["items"].([]any) {
			all = append(all, it.(map[string]any)["title"].(string))
		}
		next, _ := r.Body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if fmt.Sprint(all) != "[Engineer 4 Engineer 3 Engineer 2 Engineer 1 Engineer 0]" {
		t.Fatalf("pages = %v", all)
	}

	count := func(query string) int {
		r := alice.must(200, "GET", "/v1/jobs?"+query, nil)
		return len(r.Body["items"].([]any))
	}
	if n := count("stage=active"); n != 3 {
		t.Fatalf("stage=active: %d", n)
	}
	if n := count("stage=saved&stage=offer"); n != 2 {
		t.Fatalf("stage=saved,offer: %d", n)
	}
	if n := count("q=" + url.QueryEscape("100%_")); n != 3 {
		t.Fatalf("q literal wildcard: %d", n)
	}
	if n := count("q=glob"); n != 2 {
		t.Fatalf("q=glob: %d", n)
	}
	if n := count("q=%25"); n != 3 { // a lone % matches only literal percent signs
		t.Fatalf("q=%%: %d", n)
	}
	if n := count("sort=-created_at&status_id=" + byName["Applied"] + "&status_id=" + byName["Bookmarked"]); n != 5 {
		t.Fatalf("status filter: %d", n)
	}
	alice.must(422, "GET", "/v1/jobs?stage=nope", nil)
	alice.must(422, "GET", "/v1/jobs?cursor=garbage", nil)
	// A cursor from one sort is rejected by another.
	r := alice.must(200, "GET", "/v1/jobs?limit=1", nil)
	alice.must(422, "GET", "/v1/jobs?sort=position&cursor="+url.QueryEscape(r.Body["next_cursor"].(string)), nil)
}

func TestJobViews(t *testing.T) {
	alice, bob := newServer(t)
	byName, _ := statuses(alice)
	r := alice.must(201, "POST", "/v1/job-views", map[string]any{
		"name": "Active", "layout": "table", "sort": "-created_at",
		"filters": map[string]any{"stages": []string{"active", "offer"}, "status_ids": []string{byName["Applied"]}, "q": "eng"},
	})
	id := r.Body["id"].(string)
	f := r.Body["filters"].(map[string]any)
	if r.Body["layout"] != "table" || len(f["stages"].([]any)) != 2 || f["q"] != "eng" {
		t.Fatalf("view = %v", r.Body)
	}
	alice.must(201, "POST", "/v1/job-views", map[string]any{"name": "Everything"})
	l := alice.must(200, "GET", "/v1/job-views", nil)
	if len(l.List) != 2 || l.List[0].(map[string]any)["name"] != "Active" || l.List[1].(map[string]any)["layout"] != "board" {
		t.Fatalf("views = %v", l.List)
	}
	bad := alice.must(422, "POST", "/v1/job-views", map[string]any{"name": "x", "sort": "title", "filters": map[string]any{"stages": []string{"won"}}})
	if !hasFieldError(bad, "sort") || !hasFieldError(bad, "filters.stages[0]") {
		t.Fatalf("errors = %v", bad.Body)
	}
	alice.must(200, "PUT", "/v1/job-views/"+id, map[string]any{"name": "Renamed"})
	bob.must(404, "PUT", "/v1/job-views/"+id, map[string]any{"name": "Mine"})
	if l := bob.must(200, "GET", "/v1/job-views", nil); len(l.List) != 0 {
		t.Fatalf("bob sees %v", l.List)
	}
	alice.must(204, "DELETE", "/v1/job-views/"+id, nil)
	alice.must(404, "DELETE", "/v1/job-views/"+id, nil)
}
