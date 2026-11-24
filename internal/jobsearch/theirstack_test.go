package jobsearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTheirStackSearch(t *testing.T) {
	since := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/jobs/search" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("X-Credits-Charged", "2")
		_, _ = w.Write([]byte(`{"metadata":{},"data":[
 {"id":123,"job_title":"Backend Engineer","url":"https://www.linkedin.com/jobs/view/1","final_url":"https://jobs.lever.co/acme/1",
  "date_posted":"2026-11-24","discovered_at":"2026-11-24T10:05:00","company":"Acme","location":"Monterrey, Nuevo León, Mexico","remote":false},
 {"id":124,"job_title":"No link","url":"http://insecure.example/1","discovered_at":"2026-11-24T10:06:00","company_object":{"name":"Beta"}}]}`))
	}))
	defer srv.Close()
	ts := &TheirStack{BaseURL: srv.URL}
	res, err := ts.Search(context.Background(), "k", Query{Titles: []string{"backend engineer"}, Locations: []string{"Monterrey (NL)"}, Country: "mx", RemoteOnly: true, Since: since, Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if res.Credits != 2 || len(res.Postings) != 1 {
		t.Fatalf("result = %+v", res)
	}
	p := res.Postings[0]
	if p.ID != "123" || p.URL != "https://jobs.lever.co/acme/1" || p.Company != "Acme" || !p.DiscoveredAt.Equal(since.Add(5*time.Minute)) || p.PostedAt == nil {
		t.Fatalf("posting = %+v", p)
	}
	if got["discovered_at_gte"] != "2026-11-24T10:00:00Z" || got["posted_at_max_age_days"] != float64(30) || got["limit"] != float64(25) {
		t.Fatalf("body = %v", got)
	}
	if c := got["job_country_code_or"].([]any); c[0] != "MX" {
		t.Fatalf("country = %v", c)
	}
	if l := got["job_location_pattern_or"].([]any); l[0] != `(?i)Monterrey \(NL\)` {
		t.Fatalf("locations = %v", l)
	}
	if w := got["workplace_types_or"].([]any); w[0] != "remote" {
		t.Fatalf("workplace = %v", w)
	}
}

func TestTheirStackErrors(t *testing.T) {
	for _, tc := range []struct {
		status  int
		charged bool
	}{{401, false}, {402, false}, {422, false}, {429, false}, {500, true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) }))
		_, err := (&TheirStack{BaseURL: srv.URL}).Search(context.Background(), "k", Query{Titles: []string{"x"}, Limit: 1})
		srv.Close()
		var e *Error
		if !errors.As(err, &e) || e.Charged != tc.charged {
			t.Errorf("%d: %v", tc.status, err)
		}
	}
	_, err := (&TheirStack{BaseURL: "http://127.0.0.1:1"}).Search(context.Background(), "k", Query{Titles: []string{"x"}, Limit: 1})
	var e *Error
	if !errors.As(err, &e) || !e.Charged {
		t.Errorf("unreachable: %v", err)
	}
}
