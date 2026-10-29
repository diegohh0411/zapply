package webui

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/fstest"
)

func TestWasmDelivery(t *testing.T) {
	const name = "typst/0.7.0/typst_ts_web_compiler_bg.wasm"
	raw := []byte("wasm compiler")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	br := []byte("precompressed brotli representation")
	root := fstest.MapFS{
		name:         {Data: raw},
		name + ".gz": {Data: gz.Bytes()},
		name + ".br": {Data: br},
	}
	h := handler(Config{}, root)
	for _, tc := range []struct{ accept, encoding string }{
		{"", ""},
		{"deflate", ""},
		{"gzip", "gzip"},
		{"gzip, br", "br"},
		{"br;q=0, gzip", "gzip"},
		{"br;q=0.2, gzip;q=0.8", "gzip"},
		{"br;q=0, gzip;q=0", ""},
		{"*;q=0.5, br;q=0", "gzip"},
		{"br;q=invalid, gzip", "gzip"},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD"} {
				r := httptest.NewRequest(method, "/"+name, nil)
				r.Header.Set("Accept-Encoding", tc.accept)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 || w.Header().Get("Content-Encoding") != tc.encoding {
					t.Fatalf("%s: status=%d encoding=%q", method, w.Code, w.Header().Get("Content-Encoding"))
				}
				if w.Header().Get("Content-Type") != "application/wasm" || w.Header().Get("Vary") != "Accept-Encoding" ||
					w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
					t.Fatalf("%s: incorrect streaming/cache headers: %v", method, w.Header())
				}
				want := raw
				switch tc.encoding {
				case "br":
					want = br
				case "gzip":
					want = gz.Bytes()
				}
				// ServeContent may omit Content-Length for encoded responses.
				if length := w.Header().Get("Content-Length"); length != "" && length != strconv.Itoa(len(want)) {
					t.Fatalf("%s: incorrect representation length: %v", method, w.Header())
				}
				if method == "HEAD" {
					if w.Body.Len() != 0 {
						t.Fatal("HEAD must not send a body")
					}
				} else if !bytes.Equal(w.Body.Bytes(), want) {
					t.Fatal("incorrect representation body")
				} else if tc.encoding == "gzip" {
					reader, err := gzip.NewReader(w.Body)
					if err != nil {
						t.Fatal(err)
					}
					decompressed, err := io.ReadAll(reader)
					_ = reader.Close()
					if err != nil || !bytes.Equal(decompressed, raw) {
						t.Fatal("gzip did not reproduce the compiler bytes")
					}
				}
			}
		})
	}
}

func TestWasmEncodingMissingVariants(t *testing.T) {
	const name = "typst/0.7.0/typst_ts_web_compiler_bg.wasm"
	root := fstest.MapFS{name: {Data: []byte("wasm")}, name + ".gz": {Data: []byte("gzip")}}
	if got := wasmEncoding(root, name, "br, gzip"); got != "gzip" {
		t.Fatalf("missing Brotli should fall back to gzip, got %q", got)
	}
	if got := wasmEncoding(root, name, "br"); got != "" {
		t.Fatalf("missing Brotli should fall back to identity, got %q", got)
	}
}
