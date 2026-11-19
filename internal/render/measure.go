package render

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/templates"
)

// The summary sits on its own rows: fitting reserves exactly SummaryRows
// lines for it, so a summary has to wrap to that many lines on the page.
// These measurements compile a page with only a name and the summary, and
// count the summary's lines in the SVG Typst writes.

// MinSummaryRows and MaxSummaryRows bound the summary row setting.
const (
	MinSummaryRows     = 1
	MaxSummaryRows     = 4
	DefaultSummaryRows = 2
)

// calibration is ordinary prose for estimating characters per line. Its words
// have typical English lengths, so the estimate holds for most summaries.
const calibration = "Computer science student and researcher who builds full-stack platforms, data pipelines and tools for small teams, " +
	"with shipped features in web frameworks, relational databases and background jobs, and a record of measurable results for real users. " +
	"Previously led workshops, organized events and wrote software that people still use every day across several countries and teams. " +
	"Enjoys turning vague requirements into working systems with tests, documentation and careful attention to the details that matter most."

// textY matches the vertical offset of each text run in Typst's SVG output.
var textY = regexp.MustCompile(`class="typst-text" transform="matrix\(1 0 0 -1 [-0-9.e]+ ([-0-9.e]+)\)"`)

type lineKey struct {
	template string
	version  int
	locale   string
}

var perLine sync.Map // lineKey -> float64

// lineCount compiles content holding only a name and summary and returns how
// many distinct text rows the page has.
func (c *Compiler) lineCount(ctx context.Context, m templates.Manifest, locale, summary string) (int, error) {
	bin, err := c.binary(m.Typst)
	if err != nil {
		return 0, err
	}
	content, _ := json.Marshal(map[string]any{
		"schema": 1, "locale": locale, "date": "2000-01-01",
		"profile":  map[string]any{"name": "X", "summary": summary},
		"sections": []any{}, "skills": []any{},
	})
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	svg, err := templates.CompileSVG(ctx, bin, m, content)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for _, y := range textY.FindAllSubmatch(svg, -1) {
		seen[string(y[1])] = true
	}
	return len(seen), nil
}

// SummaryLines is how many lines summary wraps to in template version
// (name, version) for locale. Whitespace runs count as one space, as the
// summary agent's output is normalized.
func (c *Compiler) SummaryLines(ctx context.Context, name string, version int, locale, summary string) (int, error) {
	m, ok := manifest(name, &version)
	if !ok {
		return 0, errors.New("render: unknown template version")
	}
	summary = strings.Join(strings.Fields(summary), " ")
	if summary == "" {
		return 0, nil
	}
	base, err := c.lineCount(ctx, m, locale, "X")
	if err != nil {
		return 0, err
	}
	n, err := c.lineCount(ctx, m, locale, summary)
	if err != nil {
		return 0, err
	}
	return n - base + 1, nil
}

// CharactersPerLine estimates how many characters of prose fill one summary
// line in a template version and locale. It is measured once per process.
func (c *Compiler) CharactersPerLine(ctx context.Context, name string, version int, locale string) (float64, error) {
	k := lineKey{name, version, locale}
	if v, ok := perLine.Load(k); ok {
		return v.(float64), nil
	}
	lines, err := c.SummaryLines(ctx, name, version, locale, calibration)
	if err != nil {
		return 0, err
	}
	if lines < 2 {
		return 0, errors.New("render: calibration text did not wrap")
	}
	// The last line is partly filled; count it as half.
	v := float64(utf8.RuneCountInString(calibration)) / (float64(lines) - 0.5)
	perLine.Store(k, v)
	return v, nil
}

// SummaryRange is the character range a summary aims for to wrap to rows
// lines: past the start of the last row and short of its end.
func SummaryRange(perLine float64, rows int) (min, max int) {
	return int(perLine * (float64(rows) - 0.75)), int(perLine * (float64(rows) - 0.1))
}
