package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/api"
	"github.com/diegohh0411/zapply/internal/auth"
	"github.com/diegohh0411/zapply/internal/testdb"
)

const secret = "0123456789abcdef0123456789abcdef"

func TestEndToEnd(t *testing.T) {
	db := testdb.New(t)
	draining := false
	srv := httptest.NewServer(api.Handler(api.Deps{
		Store:    db.Store,
		Verifier: auth.NewDev(secret, "authenticated"),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Draining: func() bool { return draining },
	}))
	defer srv.Close()

	get := func(path, token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	me := func(token string) string {
		t.Helper()
		res := get("/v1/me", token)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("/v1/me status = %d", res.StatusCode)
		}
		var body struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.ID == "" || body.CreatedAt.IsZero() {
			t.Fatalf("body = %+v, err %v", body, err)
		}
		return body.ID
	}

	for path, want := range map[string]int{
		"/healthz": http.StatusNoContent,
		"/readyz":  http.StatusNoContent,
		"/v1/me":   http.StatusUnauthorized,
		"/nope":    http.StatusNotFound,
	} {
		if got := get(path, "").StatusCode; got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}

	alice, _ := auth.MintDev(secret, "authenticated", "alice", time.Hour)
	bob, _ := auth.MintDev(secret, "authenticated", "bob", time.Hour)
	a1, a2, b := me(alice), me(alice), me(bob)
	if a1 != a2 {
		t.Fatalf("alice resolved to %s then %s", a1, a2)
	}
	if a1 == b {
		t.Fatal("alice and bob share a user")
	}

	draining = true
	if got := get("/readyz", "").StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while draining = %d", got)
	}
}
