package api

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/httpx"
	"github.com/diegohh0411/zapply/internal/render"
	"github.com/diegohh0411/zapply/templates"
)

func (s *server) ListCvTemplates(ctx context.Context, _ gen.ListCvTemplatesRequestObject) (gen.ListCvTemplatesResponseObject, error) {
	ts, e := render.Templates()
	return convertMatch[gen.ListCvTemplates200JSONResponse](ts, e)
}

func (s *server) GetCv(ctx context.Context, req gen.GetCvRequestObject) (gen.GetCvResponseObject, error) {
	p, e := s.cv.Preview(ctx, uid(ctx), req.Id)
	body, e := convertMatch[gen.CvPreview](p, e)
	if e != nil {
		return nil, e
	}
	return gen.GetCv200JSONResponse{Body: body, Headers: gen.GetCv200ResponseHeaders{ETag: etag(p.Draft.Version)}}, nil
}

func (s *server) PutCv(ctx context.Context, req gen.PutCvRequestObject) (gen.PutCvResponseObject, error) {
	version, e := parseIfMatch(req.Params.IfMatch)
	if e != nil {
		return nil, e
	}
	in, e := convertMatch[render.DraftInput](req.Body, nil)
	if e != nil {
		return nil, e
	}
	p, e := s.cv.SaveDraft(ctx, uid(ctx), req.Id, version, in)
	body, e := convertMatch[gen.CvPreview](p, e)
	if e != nil {
		return nil, e
	}
	return gen.PutCv200JSONResponse{Body: body, Headers: gen.PutCv200ResponseHeaders{ETag: etag(p.Draft.Version)}}, nil
}

func (s *server) ListCvExports(ctx context.Context, req gen.ListCvExportsRequestObject) (gen.ListCvExportsResponseObject, error) {
	xs, e := s.cv.Exports(ctx, uid(ctx), req.Id)
	return convertMatch[gen.ListCvExports200JSONResponse](map[string]any{"items": xs}, e)
}

func (s *server) CreateCvExport(ctx context.Context, req gen.CreateCvExportRequestObject) (gen.CreateCvExportResponseObject, error) {
	in, e := convertMatch[render.ExportInput](req.Body, nil)
	if e != nil {
		return nil, e
	}
	key := ""
	if req.Params.IdempotencyKey != nil {
		key = *req.Params.IdempotencyKey
	}
	return convertMatch[gen.CreateCvExport201JSONResponse](s.cv.CreateExport(ctx, uid(ctx), req.Id, key, in))
}

func (s *server) GetCvExport(ctx context.Context, req gen.GetCvExportRequestObject) (gen.GetCvExportResponseObject, error) {
	return convertMatch[gen.GetCvExport200JSONResponse](s.cv.Export(ctx, uid(ctx), req.Id))
}

func (s *server) GetCvExportPdf(ctx context.Context, req gen.GetCvExportPdfRequestObject) (gen.GetCvExportPdfResponseObject, error) {
	pdf, name, e := s.cv.PDF(ctx, uid(ctx), req.Id)
	if e != nil {
		return nil, e
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	return gen.GetCvExportPdf200ApplicationpdfResponse{Body: bytes.NewReader(pdf), ContentLength: int64(len(pdf)),
		Headers: gen.GetCvExportPdf200ResponseHeaders{ContentDisposition: &disposition}}, nil
}

// templateAssets serves templates.FS under render.AssetsPath for browsers to
// compile with. Files of published versions, and the fonts they pin, never
// change, so they cache for a year; unpublished ones revalidate.
func templateAssets() http.Handler {
	frozen := map[string]bool{}
	if ms, err := templates.Manifests(); err == nil {
		for _, m := range ms {
			if m.Published {
				frozen[m.Dir()+"/"], frozen[m.FontDir()+"/"] = true, true
			}
		}
	}
	files := http.FileServerFS(templates.FS())
	return http.StripPrefix(strings.TrimSuffix(render.AssetsPath, "/"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if strings.HasSuffix(r.URL.Path, "/") || !strings.Contains(p, ".") {
			httpx.WriteProblem(w, r, http.StatusNotFound, "")
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-cache")
		for prefix := range frozen {
			if strings.HasPrefix(p, prefix) {
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			}
		}
		h.Set("X-Content-Type-Options", "nosniff")
		switch path.Ext(p) {
		case ".typ", ".md":
			h.Set("Content-Type", "text/plain; charset=utf-8")
		case ".ttf":
			h.Set("Content-Type", "font/ttf")
		case ".otf":
			h.Set("Content-Type", "font/otf")
		}
		files.ServeHTTP(w, r)
	}))
}
