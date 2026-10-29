package templates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The determinism gate (docs/parts/07-rendering.md). golden.json holds the
// SHA-256 of every fixture compiled by every template version. This test
// checks it against the native Typst CLI; web/tests/typst-golden.cjs checks
// the same hashes against the WASM compiler the browser runs. Both passing
// means the two produce identical PDFs.
//
//	go test ./templates -update   rewrites golden.json and published.json
//
// The CLI comes from ZAPPLY_TYPST, or typst on PATH. Without one the
// compile tests skip, unless ZAPPLY_REQUIRE_TYPST=1 (as in CI).

var update = flag.Bool("update", false, "rewrite golden.json and published.json for unpublished versions")

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func writeJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func manifests(t *testing.T) []Manifest {
	t.Helper()
	ms, err := Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no templates")
	}
	return ms
}

func TestManifests(t *testing.T) {
	for _, m := range manifests(t) {
		if m.Typst == "" || len(m.Locales) == 0 {
			t.Errorf("%s: typst and locales are required", m.Dir())
		}
		if _, err := fs.Stat(files, m.FontDir()); err != nil {
			t.Errorf("%s: fonts %s: %v", m.Dir(), m.Fonts, err)
		}
		var locales map[string]json.RawMessage
		readJSON(t, m.Dir()+"/strings.json", &locales)
		for _, l := range m.Locales {
			if _, ok := locales[l]; !ok {
				t.Errorf("%s: no strings for locale %s", m.Dir(), l)
			}
		}
	}
}

// versionFiles hashes every file a version compiles with, fonts included.
// The manifest is hashed without its published flag, so publishing a
// version doesn't count as changing it.
func versionFiles(t *testing.T, m Manifest) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range []string{m.Dir(), m.FontDir()} {
		err := fs.WalkDir(files, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := files.ReadFile(p)
			if p == m.Dir()+"/manifest.json" {
				unpublished := m
				unpublished.Published = false
				b, err = json.Marshal(unpublished)
			}
			out[p] = digest(b)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestPublishedImmutable fails when a published version, or a font it pins,
// changes. Change a published template by adding a new version instead.
func TestPublishedImmutable(t *testing.T) {
	published := map[string]map[string]string{}
	readJSON(t, "published.json", &published)
	for _, m := range manifests(t) {
		got := versionFiles(t, m)
		want, ok := published[m.Dir()]
		switch {
		case !m.Published && *update:
			published[m.Dir()] = got
		case m.Published && !ok:
			t.Errorf("%s is published but missing from published.json; run go test ./templates -update before publishing", m.Dir())
		case ok && !maps.Equal(got, want):
			if m.Published {
				t.Errorf("%s is published and its files changed; add a new version instead", m.Dir())
			} else {
				t.Errorf("%s changed; run go test ./templates -update", m.Dir())
			}
		}
	}
	if *update {
		writeJSON(t, "published.json", published)
	}
}

func typstCLI(t *testing.T, want string) string {
	t.Helper()
	bin := os.Getenv("ZAPPLY_TYPST")
	if bin == "" {
		bin, _ = exec.LookPath("typst")
	}
	required := os.Getenv("ZAPPLY_REQUIRE_TYPST") == "1"
	if bin == "" {
		if required {
			t.Fatal("ZAPPLY_REQUIRE_TYPST=1 but no typst CLI (set ZAPPLY_TYPST)")
		}
		t.Skip("no typst CLI; set ZAPPLY_TYPST to run the determinism gate")
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	if !strings.HasPrefix(string(out), "typst "+want+" ") {
		if required {
			t.Fatalf("templates pin typst %s, %s is %s", want, bin, strings.TrimSpace(string(out)))
		}
		t.Skipf("templates pin typst %s, %s is %s", want, bin, strings.TrimSpace(string(out)))
	}
	return bin
}

type fixture struct {
	name    string
	locale  string
	content []byte
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	paths, err := filepath.Glob("fixtures/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []fixture
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var head struct {
			Locale string `json:"locale"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, fixture{strings.TrimSuffix(filepath.Base(p), ".json"), head.Locale, b})
	}
	return out
}

func TestGolden(t *testing.T) {
	golden := map[string]string{}
	readJSON(t, "golden.json", &golden)
	ms := manifests(t)
	for _, m := range ms {
		bin := typstCLI(t, m.Typst)
		for _, f := range fixtures(t) {
			if !slices.Contains(m.Locales, f.locale) {
				continue
			}
			key := m.Dir() + "/" + f.name
			t.Run(key, func(t *testing.T) {
				a, err := Compile(context.Background(), bin, m, f.content)
				if err != nil {
					t.Fatal(err)
				}
				b, err := Compile(context.Background(), bin, m, f.content)
				if err != nil {
					t.Fatal(err)
				}
				if digest(a) != digest(b) {
					t.Fatal("two compiles of the same input differ")
				}
				if *update && !m.Published {
					golden[key] = digest(a)
				} else if golden[key] != digest(a) {
					t.Errorf("hash %s, golden.json has %q", digest(a), golden[key])
				}
			})
		}
	}
	if *update {
		writeJSON(t, "golden.json", golden)
	}
}

// TestTextIsLiteral checks what an ATS reads: user text containing Typst
// markup comes out of the PDF as the same text, in reading order.
func TestTextIsLiteral(t *testing.T) {
	pdftotext, err := exec.LookPath("pdftotext")
	if err != nil {
		if os.Getenv("ZAPPLY_REQUIRE_TYPST") == "1" {
			t.Fatal("ZAPPLY_REQUIRE_TYPST=1 but pdftotext (poppler) is missing")
		}
		t.Skip("no pdftotext")
	}
	var content []byte
	for _, f := range fixtures(t) {
		if f.name == "injection-en" {
			content = f.content
		}
	}
	want := []string{
		`#panic("name") Eve`,
		`]) #set text(red) *bold* _em_`,
		`\escaped // not a comment`,
		`$x^2$ engineer <label> @ref`,
		`• #read("/content.json") literal`,
		`• = Not a heading`,
		"• ```raw``` and `code`",
		`#eval("1+1")`,
		`2020`,
	}
	for _, m := range manifests(t) {
		pdf, err := Compile(context.Background(), typstCLI(t, m.Typst), m, content)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(pdftotext, "-", "-")
		cmd.Stdin = strings.NewReader(string(pdf))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		text := string(out)
		at := 0
		for _, w := range want {
			i := strings.Index(text[at:], w)
			if i < 0 {
				t.Errorf("%s: %q missing or out of order in:\n%s", m.Dir(), w, text)
				break
			}
			at += i + len(w)
		}
	}
}
