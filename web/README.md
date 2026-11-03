# zapply web

Angular 22 + ZardUI. Design notes: [`../docs/parts/09-web.md`](../docs/parts/09-web.md).

```bash
npm ci
npm start          # localhost:4200, proxies /v1 to the API on :8080 (make run)
npm run gen:api    # regenerate src/app/api/schema.d.ts from ../api/openapi.yaml
npm run typecheck
npm run build      # `make web` copies the output into internal/webui/dist for embedding
```

Components in `src/app/shared` come from ZardUI and are ours to edit.

`npm run build` also generates Brotli and gzip copies of the Typst WASM
compiler. The Go static handler negotiates these copies and caches the
versioned compiler URL for a year. Use the npm build command for deploys so
this compression step runs; `ng serve` serves the original asset in development.
