package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// typstCompiler uses the CLI ZAPPLY_TYPST names (the template gate's
// variable), so these tests run where the gate runs.
func typstCompiler(t *testing.T) *Compiler {
	bin := os.Getenv("ZAPPLY_TYPST")
	if bin == "" {
		if os.Getenv("ZAPPLY_REQUIRE_TYPST") != "" {
			t.Fatal("ZAPPLY_TYPST is required")
		}
		t.Skip("ZAPPLY_TYPST not set")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "0.14.2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(dir, "0.14.2", "typst")); err != nil {
		t.Fatal(err)
	}
	return NewCompiler(dir)
}

func TestSummaryLines(t *testing.T) {
	c := typstCompiler(t)
	ctx := context.Background()
	for _, tc := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"Short summary.", 1},
		{strings.Repeat("word ", 40), 2},
		{strings.Repeat("word ", 60), 3},
	} {
		got, err := c.SummaryLines(ctx, "classic", 1, "en", tc.text)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("SummaryLines(%d chars) = %d, want %d", len(tc.text), got, tc.want)
		}
	}
	per, err := c.CharactersPerLine(ctx, "classic", 1, "en")
	if err != nil {
		t.Fatal(err)
	}
	if per < 80 || per > 160 {
		t.Fatalf("CharactersPerLine = %v", per)
	}
	for rows := MinSummaryRows; rows <= MaxSummaryRows; rows++ {
		lo, hi := SummaryRange(per, rows)
		if lo >= hi {
			t.Fatalf("SummaryRange(%d) = %d..%d", rows, lo, hi)
		}
	}
}
