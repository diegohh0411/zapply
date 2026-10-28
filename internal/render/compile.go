package render

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/membership"
	"github.com/diegohh0411/zapply/templates"
)

// Compiler runs the Typst CLI for clients without a browser (remote MCP, the
// extension). Browsers compile themselves; nothing compiled here is stored.
type Compiler struct {
	// Dir holds one CLI per pinned version, Dir/<version>/typst, as the
	// image installs them. Empty uses typst on PATH, if its version matches.
	Dir     string
	Timeout time.Duration
	slots   chan struct{}
}

// NewCompiler allows two compiles at once per process: each takes tens of
// milliseconds but a few hundred MB at worst.
func NewCompiler(dir string) *Compiler {
	return &Compiler{Dir: dir, Timeout: 20 * time.Second, slots: make(chan struct{}, 2)}
}

var errNoCompiler = &membership.Error{Status: 503, Code: "compiler_unavailable", Detail: "Server-side rendering isn't available for this template version."}

func (c *Compiler) binary(version string) (string, error) {
	if c == nil {
		return "", errNoCompiler
	}
	if c.Dir != "" {
		bin := filepath.Join(c.Dir, version, "typst")
		if _, err := os.Stat(bin); err != nil {
			return "", errNoCompiler
		}
		return bin, nil
	}
	bin, err := exec.LookPath("typst")
	if err != nil {
		return "", errNoCompiler
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil || !strings.HasPrefix(string(out), "typst "+version+" ") {
		return "", errNoCompiler
	}
	return bin, nil
}

// Compile compiles content with template version m.
func (c *Compiler) Compile(ctx context.Context, m templates.Manifest, content []byte) ([]byte, error) {
	bin, err := c.binary(m.Typst)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, &membership.Error{Status: 503, Code: "compiler_busy", Detail: "The server is busy rendering. Try again shortly."}
	}
	return templates.Compile(ctx, bin, m, content)
}

// PDF compiles an export's snapshot. The name is "<person> - <company>.pdf".
func (s *Service) PDF(ctx context.Context, u, id uuid.UUID) ([]byte, string, error) {
	var x Export
	var company string
	e := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var e error
		if x, e = scanExport(tx.QueryRow(ctx, `SELECT `+exportColumns+`,content FROM cv_export WHERE user_id=$1 AND id=$2`, u, id), true); e != nil {
			return e
		}
		return tx.QueryRow(ctx, `SELECT company FROM job WHERE user_id=$1 AND id=$2`, u, x.JobID).Scan(&company)
	})
	if e != nil {
		return nil, "", translate(e)
	}
	m, ok := manifest(x.Template, &x.TemplateVersion)
	if !ok {
		return nil, "", errors.New("export names a template version this build lacks")
	}
	// Recompile what was stored, normalized the way it was hashed.
	var c Content
	if e = json.Unmarshal(x.Content, &c); e != nil {
		return nil, "", e
	}
	raw, _ := json.Marshal(c)
	pdf, e := s.Typst.Compile(ctx, m, raw)
	return pdf, Filename(c.Profile.Name, company), e
}

// Filename is "<person> - <company>.pdf", without characters file systems
// reject.
func Filename(person, company string) string {
	parts := []string{}
	for _, p := range []string{person, company} {
		p = strings.Map(func(r rune) rune {
			if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
				return -1
			}
			return r
		}, strings.TrimSpace(p))
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "CV.pdf"
	}
	return strings.Join(parts, " - ") + ".pdf"
}
