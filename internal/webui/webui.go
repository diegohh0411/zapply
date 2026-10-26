// Package webui serves the single-page app (web/) embedded in the binary,
// plus the runtime settings the app reads at startup.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/diegohh0411/zapply/internal/httpx"
)

//go:embed all:dist
var dist embed.FS

// Config is served as /config.json. Everything in it is public: the
// Supabase URL and publishable (anon) key are meant for browsers.
type Config struct {
	SupabaseURL     string `json:"supabase_url"`
	SupabaseAnonKey string `json:"supabase_anon_key"`
	// DevLogin lets the app mint tokens through POST /dev/token.
	DevLogin bool `json:"dev_login"`
}

// Built reports whether the app was built into this binary.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// Handler serves static files, falling back to index.html for app routes.
// Hashed build assets are cached for a year; everything else revalidates.
func Handler(cfg Config) http.Handler {
	root, _ := fs.Sub(dist, "dist")
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			httpx.WriteProblem(w, r, http.StatusMethodNotAllowed, "")
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		if r.URL.Path == "/config.json" {
			h.Set("Cache-Control", "no-cache")
			httpx.WriteJSON(w, http.StatusOK, cfg)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(root, name); err != nil {
			if path.Ext(name) != "" { // a missing asset, not an app route
				httpx.WriteProblem(w, r, http.StatusNotFound, "")
				return
			}
			name = "index.html"
		}
		if name == "index.html" {
			if !Built() {
				httpx.WriteProblem(w, r, http.StatusNotFound, "the web app is not built into this binary (make web)")
				return
			}
			h.Set("Cache-Control", "no-cache")
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			files.ServeHTTP(w, r2)
			return
		}
		if isHashed(name) {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + name
		files.ServeHTTP(w, r2)
	})
}

// isHashed matches Angular's output names: <name>-<8-char hash>.<ext>,
// e.g. main-HFRQZ5VH.js or chunk-C72Nx81-.js.
func isHashed(name string) bool {
	base := strings.TrimSuffix(path.Base(name), path.Ext(name))
	if len(base) < 10 || base[len(base)-9] != '-' {
		return false
	}
	for _, c := range base[len(base)-8:] {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
