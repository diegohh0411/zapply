package httpx_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/httpx"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestRequestID(t *testing.T) {
	var seen string
	h := httpx.RequestLog(discard)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestID(r.Context())
	}))

	for _, tc := range []struct{ in, wantSame string }{
		{"abc-123", "abc-123"},
		{"bad id with spaces", ""},
		{"", ""},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.in != "" {
			r.Header.Set("X-Request-ID", tc.in)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("X-Request-ID"); got != seen || got == "" {
			t.Fatalf("header %q, context %q", got, seen)
		}
		if tc.wantSame != "" && seen != tc.wantSame {
			t.Fatalf("in %q: id = %q, want reused", tc.in, seen)
		}
		if tc.wantSame == "" && seen == tc.in {
			t.Fatalf("in %q: invalid id reused", tc.in)
		}
	}
}

func TestRecover(t *testing.T) {
	h := httpx.Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		httpx.RequestLog(discard), httpx.Recover,
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type = %q", ct)
	}
	var p httpx.Problem
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil || p.RequestID == "" {
		t.Fatalf("problem = %+v, err %v", p, err)
	}
}

func TestServerDrains(t *testing.T) {
	release := make(chan struct{})
	srv := &httpx.Server{
		Addr:       "127.0.0.1:0",
		DrainDelay: 50 * time.Millisecond,
		Logger:     discard,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-release
			w.WriteHeader(http.StatusNoContent)
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, func(a net.Addr) { addrc <- a }) }()
	addr := <-addrc

	// Start a slow request, then begin shutdown while it is in flight.
	resc := make(chan int, 1)
	go func() {
		res, err := http.Get("http://" + addr.String())
		if err != nil {
			resc <- 0
			return
		}
		res.Body.Close()
		resc <- res.StatusCode
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond)
	if !srv.Draining() {
		t.Fatal("not draining after cancel")
	}
	close(release)

	if code := <-resc; code != http.StatusNoContent {
		t.Fatalf("in-flight request status = %d", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
