package webui

import (
	"io/fs"
	"strconv"
	"strings"
)

// The directory pins the compiler release, so these URLs never change bytes.
func isVersionedWasm(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 3 && parts[0] == "typst" && parts[1] != "" && parts[2] == "typst_ts_web_compiler_bg.wasm"
}

// Prefer Brotli on a tie; fall back to gzip or the original when a build
// doesn't contain the requested representation (for example, ng serve).
func wasmEncoding(root fs.FS, name, accept string) string {
	qualities := map[string]float64{}
	for _, item := range strings.Split(accept, ",") {
		fields := strings.Split(item, ";")
		q := 1.0
		for _, param := range fields[1:] {
			key, value, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.EqualFold(key, "q") {
				var err error
				q, err = strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil || q < 0 || q > 1 {
					q = 0
				}
			}
		}
		qualities[strings.ToLower(strings.TrimSpace(fields[0]))] = q
	}
	best, bestQ := "", 0.0
	for _, candidate := range []struct{ encoding, suffix string }{{"br", ".br"}, {"gzip", ".gz"}} {
		q, explicit := qualities[candidate.encoding]
		if !explicit {
			q = qualities["*"]
		}
		if q > bestQ {
			if _, err := fs.Stat(root, name+candidate.suffix); err == nil {
				best, bestQ = candidate.encoding, q
			}
		}
	}
	return best
}
