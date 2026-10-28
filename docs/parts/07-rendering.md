# Part 7: Rendering

Status: in progress. Step 1 (the determinism gate and the `classic` template) is built; see "Implementation record" at the end.

Matching (part 6) decides what a job should see: the selected entries, their
highlights in order, and the ranked skills. Part 7 lays that subset out as an
ATS-friendly PDF.

**Rendering stores no files.** The browser compiles each CV with Typst built to
WebAssembly. The server keeps only a small, immutable **snapshot** in
Postgres: the exact data a CV was compiled from and the template version
that compiled it. Compiling the same snapshot again gives the same PDF, byte
for byte. Object storage is not needed, so NATS remains the only
infrastructure besides Postgres.

## Principles

- **Truthful by construction.** The text in a CV comes only from the library.
  Part 7 never rewrites, summarizes or generates content. The server builds
  each snapshot from library rows, so a client can choose items but cannot
  inject text.
- **Deterministic.** A snapshot pins everything that changes the output:
  content, template version, compiler version, fonts and the document date.
- **Data, not markup.** User text reaches Typst as JSON (`json(...)` on a
  virtual file) and is never interpolated into Typst source. A highlight
  that contains `#` or `]` cannot break or script a template.
- **The library stays the source.** Snapshots copy content; they do not
  reference it. Editing or deleting an entry later does not change a CV that
  was already exported.

## Concepts

- **Draft:** the editable CV settings for one job: template, locale, page
  budget and the user's picks (include or exclude an entry or highlight).
  There is one draft per job.
- **Content:** the JSON a template compiles, built by the server from the
  library, the latest match run and the draft picks (see "Building content").
- **Trim order:** the order in which optional items are removed when the
  content overflows the page budget, least relevant first.
- **Export:** an immutable snapshot of one CV as it was sent: the content
  after fitting, the template version and the date.

## Templates

Templates live in the repository as Typst source:

```
templates/<name>/<version>/
  main.typ          the layout, keyed by evidence kind (part 2)
  strings.json      section headings, month names and labels for each locale
  manifest.json     typst version, fonts bundle, supported locales, published flag
```

- **Versions are immutable once published.** A change to layout, fonts or
  compiler is a new version. Old versions stay forever, because exports
  refer to them. CI enforces this with a committed hash manifest of every
  published version.
- **The template manifest pins the compiler and fonts.** Upgrading Typst
  means publishing a new template version. Old snapshots keep compiling with
  the compiler they were made with, and the set of compilers stays small.
- **Fonts are bundled** as versioned, openly licensed files. Typst must never
  fall back to system fonts, which differ between machines.
- The Go binary embeds the templates and fonts (`go:embed`), and the API serves
  them as immutable static assets, the same way as the web app (part 9).
- **ATS rules:** single column, real text (no text in images or shapes),
  standard section headings for each locale, embedded fonts, no headers or
  footers holding content, and links as visible text. Output is tagged PDF
  wherever the pinned Typst version supports it.
- MVP: one template (`classic`) in English and Spanish.

## Building content

`internal/render` builds the content for a draft:

1. **Selection.** Start from the latest completed match run for the job:
   `selection`, with highlights in the run's order, and ranked `skills`.
   Without one, fall back to `auto_include` items in library order, which is
   how cvd behaved. If the library changed after the run (its
   `library_fingerprint` differs), the draft says so and offers a new run.
2. **Picks.** Apply the user's includes and excludes. An included item is
   pinned and is never trimmed. Career breaks appear only when included
   (part 2).
3. **Sections.** Group items by category and kind into the template's
   sections, and sort entries by date within each section.
4. **Trim order.** List optional items from least to most relevant:
   - highlights first, always keeping at least one on any entry that remains
   - then whole entries, last of all those that are the supporting evidence
     for a met must-have
   - then skills beyond the top N

The content is canonical JSON with a `content_hash` (SHA-256), like
`job.content_hash`.

## Page fitting

Fitting runs where the CV is compiled, which is normally the browser:

1. Compile the full content. If it fits the page budget, it is done.
2. Otherwise binary-search `k`, the number of trim-order items to drop, for
   the smallest `k` that fits. That takes about `log₂(n)` compiles, each
   tens of milliseconds incremental.
3. If the content still overflows after dropping every optional item, it
   exports anyway with an overflow warning. Pinned items are the user's call.

The export records the dropped item IDs, not `k`. Rendering a snapshot never
fits again: it compiles exactly what was stored.

The page budget defaults to 1 and can be set to 1–3 pages per draft.

## Rendering

**Browser (default).** The web app lazy-loads `typst.ts` (the compiler as WASM,
27 MB, about 7 MB with Brotli, cached long-term) only on the CV screen and compiles in a Web
Worker so the UI stays responsive. Preview, fitting and download all happen
locally. The download is named `<Full name> - <Company>.pdf`.

**Server (fallback).** Clients without a browser, such as remote MCP and possibly
the extension (part 11), call `GET /v1/cv-exports/{id}/pdf`. The API compiles
the snapshot with the Typst CLI at the version the template pins and streams
the result back without storing it.

- The runtime image adds the static (musl) Typst CLI builds that the
  published templates pin, next to `/zapply`. CGO stays disabled.
- Each compile gets a timeout, a size cap and a process-wide concurrency
  limit, and runs in a temporary directory that holds only the template,
  fonts and `content.json`.

**No server-side verification in the MVP.** The CI determinism gate (see
"Tests and acceptance gates") already checks that the WASM and native builds
agree on every template and compiler change, and a snapshot that rebuilds byte
for byte is its own record. The client sends `client_sha256` with each export,
so a mismatch can be found later by compiling on the server. A worker job that
does this for every export can be added if a mismatch ever gets past CI.

## Schema (migration `00016_rendering.sql`)

```
cv_draft    user_id · job_id (PK) · template · template_version? (null = latest)
            locale · page_budget · picks jsonb {include: [ids], exclude: [ids]}
            version · updated_at

cv_export   id · user_id · job_id · match_run_id? · template · template_version
            locale · document_date · page_budget · pages · overflow
            content jsonb · content_hash · dropped jsonb [ids]
            client_sha256? · created_at
```

- Tenancy as in part 2: `user_id` on every row, forced RLS with the `tenant`
  policy, and composite `(user_id, id)` foreign keys. Both tables cascade
  when their job is deleted.
- `cv_export` is **insert-only**.
- `content` is capped at 256 KB. A large library compiles to a few tens of KB.
- `document_date` is the export date. The template sets
  `#set document(date: ...)` from it, so recompiling does not stamp the
  current time.

## API (`/v1`)

| Endpoint | Notes |
|---|---|
| `GET /templates` | Published templates, their versions, locales and asset URLs |
| `GET /jobs/{id}/cv` | The draft, its built content, the trim order and a stale-match flag. The draft creates itself on first read |
| `PUT /jobs/{id}/cv` | Template, locale, page budget and picks. Requires `If-Match` |
| `POST /jobs/{id}/cv/exports` | `{draft_version, dropped, pages, client_sha256?}`. The server rebuilds the content at that draft version, applies `dropped` (only trim-order items), and stores the snapshot. A stale draft returns `412`. `Idempotency-Key` supported |
| `GET /jobs/{id}/cv/exports` | Export history for a job, newest first |
| `GET /cv-exports/{id}` | The snapshot, with the asset URLs needed to compile it |
| `GET /cv-exports/{id}/pdf` | Server-side compile, streamed and not stored |

Outbox topics: `cv_draft.updated`, `cv_export.created`.

## Web

The job sheet gets a **CV** tab next to Match:

- A live preview with a page count, the page budget, the template and locale.
- An item list grouped by section: what is in, what was trimmed to fit, and
  include and exclude toggles. Trimmed items explain why ("dropped to fit 1
  page").
- **Export** downloads the PDF and records the snapshot. The export history
  re-downloads any past CV, compiled again locally.

## Tests and acceptance gates

- **Determinism gate (before building on it):** compile fixture snapshots
  with `typst.ts` in Node and with the native CLI at the pinned version,
  twice each, and require identical SHA-256s. CI runs it again for every new
  template or compiler version. If WASM and native differ, the server
  fallback's PDF is not the same file the user downloaded, and the plan has
  to change before step 5.
- Golden tests: fixture content → PDF hash for each published template
  version, so an accidental change to a published version fails CI.
- Text extraction: `pdftotext` on fixtures returns every highlight in reading
  order, which is the ATS proxy.
- Injection: highlights holding Typst syntax compile as literal text.
- Content builder: selection fallback, picks, pinning, trim order rules and
  the snapshot surviving a deleted library entry.
- Fitting: the smallest fitting `k` is found, pinned items are never dropped,
  and overflow is flagged.

## Build order

1. Determinism gate and the `classic` template with fixtures.
2. `internal/render`: content builder and trim order, with unit tests.
3. Migration, store queries, API and outbox topics.
4. Web: `typst.ts` in a Web Worker, fitting, preview, export and history.
5. Server compile endpoint and the Typst CLI in the image.

## Out of scope

- Cover letters, DOCX export and any LLM-written or tailored text.
- CVs that are not tied to a job (a general CV). The draft could later key on
  an optional job.
- User-made templates.
- File attachments. Part 2 deferred them to object storage in part 7. They
  are still deferred, and are the first feature that would bring object
  storage back.

## Open questions

- Can `typst.ts` stay current with Typst releases? Pinning per template
  version makes lag acceptable, but the browser cannot use a version that
  `typst.ts` does not ship.
- Page count is assumed to fall as `k` rises. A dropped item can, in
  principle, shift a break the wrong way. A linear fallback over the last
  few `k` handles it if the fixtures show it happening.

## Implementation record

### Step 1: determinism gate and `classic` v1

- **Versions.** `typst.ts` 0.7.0, its latest stable release, compiles with
  Typst 0.14.2, so `classic/1` pins 0.14.2. Typst 0.15 is out, but the browser
  can't use it until `typst.ts` ships it, which is the lag the open question
  expected.
- **The gate passes.** The native CLI and the WASM compiler produce identical
  PDFs for every fixture. `templates/golden.json` holds the hashes;
  `templates/gate_test.go` checks them against the CLI (CI installs it and sets
  `ZAPPLY_REQUIRE_TYPST=1`, so the check can't skip) and
  `web/tests/typst-golden.cjs` checks them against `typst.ts`.
- **Immutability.** `templates/published.json` hashes every file a version
  compiles with, fonts included. Once `published` is true in a manifest, a
  change fails CI. `classic/1` stays unpublished until part 7 ships.
- **Fonts.** Source Sans 3 (3.052, SIL Open Font License): regular, italic,
  semibold and semibold italic, 1.4 MB. It covers Latin scripts only, so text
  in other scripts prints as missing glyphs; a Noto fallback would be a new
  template version.
- **Dates lead the line under the title instead of sitting right-aligned.**
  `pdftotext` read right-aligned dates as a separate column and printed them
  after the entry's highlights, which is how a simple ATS would see them too.
  `TestTextIsLiteral` checks the reading order.
- **Injection.** User text containing Typst markup (`#panic(...)`, `]`, `$`,
  `<label>`, `@ref`, raw blocks) prints literally, checked through
  `pdftotext` by `TestTextIsLiteral`.
- **`templates.Compile`** runs the CLI in a scratch directory holding only the
  template, its fonts and `content.json`, with an empty environment. Step 5
  builds the server endpoint on it.
- The content schema is in [`templates/README.md`](../../templates/README.md).

