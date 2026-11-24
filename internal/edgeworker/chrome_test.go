package edgeworker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// chromeBinary returns the Chrome the browser tests run, named by
// ZAPPLY_EDGE_TEST_CHROME. The tests are opt-in: a Chrome found on a CI runner
// often cannot start (its sandbox needs user namespaces the runner forbids).
func chromeBinary(t *testing.T) string {
	t.Helper()
	p := os.Getenv("ZAPPLY_EDGE_TEST_CHROME")
	if p == "" {
		t.Skip("set ZAPPLY_EDGE_TEST_CHROME to a Chrome binary to run the browser tests")
	}
	return p
}

func TestChromeRendersWithAFreshContextPerJob(t *testing.T) {
	var mu sync.Mutex
	var cookies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing", "/favicon.ico":
			http.NotFound(w, r)
		default:
			mu.Lock()
			cookies = append(cookies, r.Header.Get("Cookie"))
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "secret", Path: "/"})
			fmt.Fprint(w, `<html><head><title>T</title></head><body><p id="x">loading</p><script>document.getElementById('x').textContent='rendered by script'; localStorage.setItem('k','v');</script></body></html>`)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewChrome(chromeBinary(t), os.Getenv("ZAPPLY_EDGE_NO_SANDBOX") == "1", &Guard{allowPrivate: true}, discard())
	t.Cleanup(c.Close)

	for i := 0; i < 2; i++ {
		page, err := c.Render(t.Context(), srv.URL+"/")
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		if !strings.Contains(page.HTML, "rendered by script") || page.Status != 200 {
			t.Fatalf("page %d = %d %s", i, page.Status, page.HTML)
		}
	}
	// The second job did not see the first one's cookie.
	if len(cookies) != 2 || cookies[0] != "" || cookies[1] != "" {
		t.Errorf("cookies sent = %q, want none: contexts must not share state", cookies)
	}

	if page, err := c.Render(t.Context(), srv.URL+"/missing"); err != nil || page.Status != 404 {
		t.Errorf("missing page = %+v, %v", page, err)
	}
}

func TestChromeRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a private address was requested: %s", r.URL)
	}))
	t.Cleanup(srv.Close)
	c := NewChrome(chromeBinary(t), os.Getenv("ZAPPLY_EDGE_NO_SANDBOX") == "1", NewGuard(), discard())
	t.Cleanup(c.Close)

	// Loopback is not a public address: the navigation itself is stopped.
	if _, err := c.Render(t.Context(), srv.URL+"/"); !errors.Is(err, ErrRefused) {
		t.Errorf("render of loopback = %v, want the address refused", err)
	}
}

func TestChromeStopsWhenTheJobIsCancelled(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	t.Cleanup(func() { close(hang); srv.Close() })
	c := NewChrome(chromeBinary(t), os.Getenv("ZAPPLY_EDGE_NO_SANDBOX") == "1", &Guard{allowPrivate: true}, discard())
	t.Cleanup(c.Close)

	ctx, cancel := context.WithCancel(t.Context())
	go func() { cancel() }()
	if _, err := c.Render(ctx, srv.URL+"/"); err == nil {
		t.Error("render of a hanging page returned no error")
	}
}
