package ats

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct{ in, system, board string }{
		{"https://job-boards.greenhouse.io/acme", Greenhouse, "acme"},
		{"boards.greenhouse.io/acme/jobs/123", Greenhouse, "acme"},
		{"https://boards.greenhouse.io/embed/job_board?for=acme", Greenhouse, "acme"},
		{"https://boards-api.greenhouse.io/v1/boards/acme/jobs", Greenhouse, "acme"},
		{"https://jobs.lever.co/acme-co/0a1b2c", Lever, "acme-co"},
		{"https://jobs.ashbyhq.com/Acme.AI?utm=x", Ashby, "Acme.AI"},
	}
	for _, c := range cases {
		s, b, err := Parse(c.in)
		if err != nil || s != c.system || b != c.board {
			t.Errorf("Parse(%q) = %q, %q, %v; want %q, %q", c.in, s, b, err, c.system, c.board)
		}
	}
	for _, bad := range []string{"https://www.linkedin.com/jobs/view/1", "https://jobs.lever.co/", "not a url", "https://jobs.ashbyhq.com/..%2f"} {
		if _, _, err := Parse(bad); !errors.Is(err, ErrUnknownURL) {
			t.Errorf("Parse(%q) err = %v, want ErrUnknownURL", bad, err)
		}
	}
}

func TestFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/boards/acme/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[{"id":42,"title":"Backend Engineer","absolute_url":"https://job-boards.greenhouse.io/acme/jobs/42","first_published":"2026-11-20T10:00:00-05:00","location":{"name":"Remote - US"}}]}`))
	})
	mux.HandleFunc("/v1/boards/acme", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"Acme Inc."}`))
	})
	mux.HandleFunc("/v0/postings/acme", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"abc","text":"Designer","hostedUrl":"https://jobs.lever.co/acme/abc","createdAt":1763640000000,"workplaceType":"onsite","categories":{"location":"Monterrey"}}]`))
	})
	mux.HandleFunc("/posting-api/job-board/acme", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[{"id":"u1","title":"ML Engineer","location":"NYC","isRemote":true,"jobUrl":"https://jobs.ashbyhq.com/acme/u1","publishedAt":"2026-11-21T00:00:00Z"},{"id":"u2","title":"Hidden","isListed":false,"jobUrl":"https://jobs.ashbyhq.com/acme/u2"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{Base: map[string]string{Greenhouse: srv.URL, Lever: srv.URL, Ashby: srv.URL}}
	ctx := context.Background()

	gh, err := c.Fetch(ctx, Greenhouse, "acme")
	if err != nil || gh.Company != "Acme Inc." || len(gh.Postings) != 1 || gh.Postings[0].Key != "42" || !gh.Postings[0].Remote || gh.Postings[0].PublishedAt == nil {
		t.Fatalf("greenhouse = %+v, %v", gh, err)
	}
	lv, err := c.Fetch(ctx, Lever, "acme")
	if err != nil || len(lv.Postings) != 1 || lv.Postings[0].Location != "Monterrey" || lv.Postings[0].Remote {
		t.Fatalf("lever = %+v, %v", lv, err)
	}
	ab, err := c.Fetch(ctx, Ashby, "acme")
	if err != nil || len(ab.Postings) != 1 || !ab.Postings[0].Remote {
		t.Fatalf("ashby = %+v, %v", ab, err)
	}
	if _, err := c.Fetch(ctx, Lever, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing board err = %v", err)
	}
}

func TestCompanyFromSlug(t *testing.T) {
	if got := CompanyFromSlug("acme-labs"); got != "Acme Labs" {
		t.Fatal(got)
	}
}
