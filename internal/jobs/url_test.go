package jobs_test

import (
	"testing"

	"github.com/diegohh0411/zapply/internal/jobs"
)

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Jobs.Example.com:443/a/b/?utm_source=li&b=2&a=1#apply": "https://jobs.example.com/a/b?a=1&b=2",
		"https://jobs.example/a%2Fb/":                                   "https://jobs.example/a%2Fb",
		"https://jobs.example/a%2F":                                     "https://jobs.example/a%2F",
		"http://example.com:80/":                                        "http://example.com",
		"https://user:pw@example.com:8443/x?gclid=1&trk=2&refId=3":      "https://example.com:8443/x",
		"https://www.linkedin.com/jobs/view/123/?trackingId=abc":        "https://www.linkedin.com/jobs/view/123",
	} {
		got, err := jobs.NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ftp://x.com", "example.com/jobs", "https://", "javascript:alert(1)"} {
		if _, err := jobs.NormalizeURL(bad); err == nil {
			t.Errorf("NormalizeURL(%q) accepted", bad)
		}
	}
}
