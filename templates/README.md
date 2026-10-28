# CV templates

Typst templates for part 7 ([docs/parts/07-rendering.md](../docs/parts/07-rendering.md)).

```
<name>/<version>/
  main.typ        the layout
  strings.json    headings, month names and labels for each locale
  manifest.json   pinned Typst version, fonts bundle, locales, published flag
fonts/<family>/<version>/   bundled fonts and their license
fixtures/*.json             sample content, compiled by the gate tests
golden.json                 SHA-256 of each fixture compiled by each version
published.json              hashes of every file each version compiles with
```

## Rules

- **Published versions never change.** Exports refer to a version and must
  recompile to the same bytes. To change a published template, copy it to a new
  version. `TestPublishedImmutable` fails otherwise.
- **The pinned Typst version must be the one `typst.ts` ships**
  (`web/tests/typst-golden.cjs` knows the mapping), or browsers can't compile it.
- **User text is data.** Read it from `/content.json` and place it as text.
  Never `eval` it or build markup from it.
- **No system or embedded fonts.** Compiles use only the version's bundled fonts.

## The determinism gate

```sh
ZAPPLY_TYPST=/path/to/typst-0.14.2 go test ./templates          # native CLI
cd web && npm test                                              # WASM (typst.ts)
ZAPPLY_TYPST=/path/to/typst-0.14.2 go test ./templates -update  # after changing an unpublished version
```

Both check `golden.json`, so passing both means the browser and the server
produce identical PDFs. CI runs the native half with `ZAPPLY_REQUIRE_TYPST=1`,
so it can't skip.

## Content (schema 1)

The server builds this JSON from the library (see "Building content" in the
part 7 doc). Optional fields may be missing or `null`.

```jsonc
{
  "schema": 1,
  "locale": "en",              // a key of strings.json
  "paper": "a4",               // optional; defaults per locale
  "date": "2026-10-28",        // the export date, used as the PDF date
  "profile": {
    "name": "…", "headline": "…", "email": "…", "phone": "…", "location": "…",
    "links": [{ "label": "…", "url": "https://…" }],
    "summary": "…"
  },
  "sections": [
    {
      "key": "experience",     // experience, education, unpaid, credentials, projects,
                               // publications, presentations, patents, creative, awards, funding
      "entries": [
        {
          "id": "…", "kind": "job", "form": "engagement",
          "title": "…", "organization": "…", "location": "…", "url": "…",
          "start": { "date": "2021-03", "precision": "month" },
          "end": { "date": "2023", "precision": "year" },
          "current": false,
          "summary": "…",
          "details": { },          // the kind's details (part 2)
          "highlights": [{ "id": "…", "text": "…" }],
          "children": [ ]          // nested entries, same shape
        }
      ]
    }
  ],
  "skills": [{ "name": "…", "type": "hard | tool | soft | language", "proficiency": "…" }]
}
```

Sections print in the order given, and entries print in the order given
within each section.
