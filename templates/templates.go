// Package templates holds the CV templates (part 7) and the fonts they pin.
//
// Each template version lives in <name>/<version>/ and is immutable once its
// manifest says it is published: exports refer to it, and recompiling an
// export must give the same PDF byte for byte. See docs/parts/07-rendering.md.
package templates

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
)

//go:embed classic fonts
var files embed.FS

// FS is every template and font, rooted at this directory.
func FS() fs.FS { return files }

// Manifest describes one template version.
type Manifest struct {
	Name    string   `json:"name"`
	Version int      `json:"version"`
	Typst   string   `json:"typst"`
	Fonts   string   `json:"fonts"`
	Locales []string `json:"locales"`
	// Published versions are frozen: CI checks their files against
	// published.json.
	Published bool `json:"published"`
}

// Dir is the version's directory, "classic/1".
func (m Manifest) Dir() string { return m.Name + "/" + strconv.Itoa(m.Version) }

// FontDir is the directory of the fonts the version pins.
func (m Manifest) FontDir() string { return "fonts/" + m.Fonts }

// Manifests lists every template version, ordered by name and version.
func Manifests() ([]Manifest, error) {
	paths, err := fs.Glob(files, "*/*/manifest.json")
	if err != nil {
		return nil, err
	}
	out := make([]Manifest, 0, len(paths))
	for _, p := range paths {
		b, err := files.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var m Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if m.Dir() != path.Dir(p) {
			return nil, fmt.Errorf("%s: name and version say %s", p, m.Dir())
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Manifest) int {
		if a.Name != b.Name {
			if a.Name < b.Name {
				return -1
			}
			return 1
		}
		return a.Version - b.Version
	})
	return out, nil
}

// Sources are the files a version compiles from, besides content.json.
var Sources = []string{"main.typ", "strings.json"}

// Compile runs the Typst CLI at typst on content for template version m.
//
// It compiles in a fresh directory holding only the template, its fonts and
// content.json, with system and embedded fonts disabled, so the output
// depends on nothing else on the machine.
func Compile(ctx context.Context, typst string, m Manifest, content []byte) ([]byte, error) {
	return compile(ctx, typst, m, content, "pdf")
}

// CompileSVG is Compile with SVG output, for content that fits one page.
// Measurements read it; nothing compiled this way is stored or shown.
func CompileSVG(ctx context.Context, typst string, m Manifest, content []byte) ([]byte, error) {
	return compile(ctx, typst, m, content, "svg")
}

func compile(ctx context.Context, typst string, m Manifest, content []byte, format string) ([]byte, error) {
	if len(content) > 256<<10 {
		return nil, fmt.Errorf("CV content exceeds 256 KB")
	}
	dir, err := os.MkdirTemp("", "zapply-typst-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	for _, name := range Sources {
		b, err := files.ReadFile(m.Dir() + "/" + name)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "content.json"), content, 0o600); err != nil {
		return nil, err
	}
	fonts, err := fs.Sub(files, m.FontDir())
	if err != nil {
		return nil, err
	}
	fontDir := filepath.Join(dir, "fonts")
	if err := os.CopyFS(fontDir, fonts); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "out."+format)
	cmd := exec.CommandContext(ctx, typst, "compile",
		"--root", dir, "--format", format,
		"--ignore-system-fonts", "--ignore-embedded-fonts",
		"--font-path", fontDir,
		filepath.Join(dir, "main.typ"), out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = []string{} // no TYPST_* or SOURCE_DATE_EPOCH from the host
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("typst compile %s: %w: %s", m.Dir(), err, stderr.Bytes())
	}
	stat, err := os.Stat(out)
	if err != nil {
		return nil, err
	}
	if stat.Size() > 8<<20 {
		return nil, fmt.Errorf("compiled CV exceeds 8 MB")
	}
	return os.ReadFile(out)
}
