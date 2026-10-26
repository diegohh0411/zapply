package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsHashed(t *testing.T) {
	for name, want := range map[string]bool{
		"main-HFRQZ5VH.js":     true,
		"chunk-C72Nx81-.js":    true,
		"chunk-oNP7xNs0.js":    true,
		"styles-EIQIAZKX.css":  true,
		"index.html":           false,
		"icon.svg":             false,
		"media/font-short.css": false,
	} {
		if got := isHashed(name); got != want {
			t.Errorf("isHashed(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestHandler(t *testing.T) {
	h := Handler(Config{SupabaseURL: "https://x.supabase.co", DevLogin: true})
	get := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}

	w := get("GET", "/config.json")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"supabase_url":"https://x.supabase.co"`) || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("config.json: %d %q %q", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
	if w := get("GET", "/missing-ABCDEFGH.js"); w.Code != 404 {
		t.Fatalf("missing asset = %d, want 404", w.Code)
	}
	if w := get("POST", "/jobs"); w.Code != 405 {
		t.Fatalf("POST = %d, want 405", w.Code)
	}
	// App routes serve index.html, or a 404 with a hint when the app isn't built.
	w = get("GET", "/jobs/board")
	if Built() {
		if w.Code != 200 || !strings.Contains(w.Body.String(), "<app-root") || w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("app route: %d %q", w.Code, w.Header().Get("Cache-Control"))
		}
	} else if w.Code != 404 || !strings.Contains(w.Body.String(), "make web") {
		t.Fatalf("unbuilt app route: %d %s", w.Code, w.Body)
	}
}
