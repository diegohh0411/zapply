# Part 4: Jobs and application tracking

Status: built (see "Delivered" at the end).

The library (part 2) holds what a person can claim. Part 4 holds where they want to claim it: the postings they save, the applications they send, and how each one goes. Matching (part 6) reads a job's description, rendering (part 7) makes a CV for it, and the mail watcher (part 10) moves applications along. All of them hang off the `job` row built here.

## Concepts

- **Job**: one posting and the user's application to it, in the same row. A posting nobody applies to is still a job, sitting in a "saved" status. Splitting posting and application adds a join to every screen for no gain at this scale. cvd used the same model.
- **Status**: a column on the board. Statuses are **per user**: people name their pipeline differently ("Phone screen", "Take-home", "Ghosted"). Each status has a fixed **stage**, so code never has to interpret a user's label.
- **Activity**: the timeline of a job. Status changes are recorded automatically; the user adds notes (an interview, a contact, a follow-up), each with the date it happened.
- **View**: a saved way of looking at jobs: board or table, a filter and a sort.

## Stages

| Stage | Meaning | Defaults (seeded) |
|---|---|---|
| `saved` | Not applied yet | Bookmarked |
| `active` | Applied, in process | Applied, Interviewing |
| `offer` | An offer is on the table | Offer |
| `closed` | Finished, any outcome | Dropped, Rejected, Posting closed |

- Stages let later parts act without reading labels. The mail watcher maps "we regret to inform you" to the first `closed` status; stats count applications as anything past `saved`.
- **Default statuses are seeded on first use**, the first time a user lists statuses or creates a job, in that transaction. The seven defaults follow cvd's board.
- **A user always has at least one status.** Deleting the last one fails with `409`. So "no statuses" means "never seeded", and seeding never resurrects statuses the user deleted.
- `applied_on` is set to the current date the first time a job enters an `active` or `offer` stage, unless the user already set it. Moving a saved posting straight to `closed` (the posting expired) doesn't count as applying.

## Schema (migration `00004_jobs.sql`)

```
job_status    id · user_id · name · normalized_name (unique per user) · color · stage
              position · created_at · updated_at

job           id · user_id · status_id · title · company · url · normalized_url
              location · work_mode · employment_type
              salary_min · salary_max · salary_currency · salary_period
              source · description · notes · applied_on? · position
              version · content_hash · status_changed_at · archived_at?
              created_at · updated_at

job_activity  id · user_id · job_id · type (created, status_changed, note)
              from_status · to_status · body · occurred_at · created_at

job_view      id · user_id · name · layout (board, table) · filters jsonb · sort
              position · created_at · updated_at
```

- **Tenancy as in part 2:** `user_id` on every row, forced RLS with the `tenant` policy, and composite `(user_id, id)` foreign keys.
- **Positions** are the fractional keys from part 2. Statuses are ordered among the user's statuses; jobs within their status (one board column). New jobs, and jobs moved to another column without a neighbour, go **first** in the column: the newest card is the one the user is looking for.
- Allocation is serialized per user for statuses (an advisory lock) and per column for jobs (locking the status row), so concurrent writes never share a key.
- **`normalized_url`** is the posting URL lower-cased in scheme and host, without the fragment, common tracking parameters (`utm_*`, `gclid`, `fbclid`, `trk`, `refId`, `trackingId`) or a trailing slash. It is unique per user when not empty, so saving the same posting twice returns `409` with the existing job's id. This is the natural idempotency key for a "save this posting" button or browser extension.
- **`content_hash`** is SHA-256 over canonical JSON of what matching reads: title, company and description. Part 6 caches scores by `(evidence hash, job hash, model, prompt version)`.
- **Activity rows snapshot status names** (`from_status`, `to_status`) instead of referencing statuses. History stays readable after a status is renamed or deleted.
- `color` is one of `gray, blue, yellow, orange, green, purple, pink, red, brown` (a fixed palette the UI themes for light and dark).
- `salary_period` is `year`, `month` or `hour`. Amounts are integers in the currency's major unit; `salary_min <= salary_max`.

## Rules

- Lengths: title, company, location, source ≤ 300 characters; URL ≤ 2,000 and http(s); description ≤ 100,000 (long postings exist); notes and activity bodies ≤ 20,000.
- A job needs a title. Everything else is optional, so a bare bookmark works.
- `currency` is a three-letter ISO 4217 code (upper-cased), required when a salary is set.
- A status deletion fails with `409` while jobs use it, unless `move_to` names another status. Then the jobs move there first (appended in their order), each with a `status_changed` activity, in the same transaction.
- Only `note` activities can be edited or deleted. Generated ones are the record.
- Views: at most 50 per user. `filters` is `{status_ids?, stages?, q?, include_archived?}` and is validated. Status ids in a saved filter that no longer exist are ignored when applied.

## API (`/v1`)

| Endpoint | Notes |
|---|---|
| `GET /job-statuses` | The board's columns, in order. Seeds defaults on first use |
| `POST /job-statuses`, `PATCH, DELETE /job-statuses/{id}` | `DELETE ?move_to=<id>` reassigns jobs first |
| `POST /job-statuses/{id}/move` | `{after_id?, before_id?}`; neither means last |
| `GET /jobs` | Filters: `status_id` (repeatable), `stage` (repeatable), `q` (title or company, case-insensitive), `include_archived`. `sort`: `-updated_at` (default), `-created_at`, `position`. Cursor pagination, `limit` ≤ 200 |
| `POST /jobs` | `status_id` defaults to the first status. `409` with `existing_id` for a duplicate URL |
| `GET /jobs/{id}` | The job with its description and notes |
| `PUT /jobs/{id}` | Replaces the job. `If-Match` required. Changing `status_id` here is a move to the top of the new column |
| `POST /jobs/{id}/move` | `{status_id?, after_id?, before_id?}`: the board's drag and drop |
| `POST /jobs/{id}/archive`, `/unarchive`, `DELETE /jobs/{id}` | Archived jobs are hidden from lists by default |
| `GET, POST /jobs/{id}/activities`, `PATCH, DELETE /jobs/{id}/activities/{activity_id}` | The timeline, newest first. Only notes are writable |
| `GET, POST /job-views`, `PUT, DELETE /job-views/{id}` | Saved views |
| `POST /postings/read` | `{url}`: reads a posting's page for the new job dialog. Saves nothing. See [Reading posting links](#reading-posting-links) |

- Lists return summaries without `description` and `notes`; a board of 200 postings would otherwise be megabytes.
- Errors, ETags (`version`), `If-Match` (`428`/`412`) and validation (`422` with `errors[]`) work exactly as in part 2.
- **Events** (outbox, delivered by part 3):
  - `job.{created,updated,moved,archived,unarchived,deleted}`: `{id, version}`
  - `job_status.{created,updated,moved,deleted}`: `{id}`. A delete with `move_to` also emits `job.moved` for each job it moved
  - `job_activity.{created,updated,deleted}`: `{id, job_id}`
  - `job_view.{created,updated,deleted}`: `{id}`

## Out of scope

- Reading pages behind a sign-in. The extension in part 11 will send the text it sees.
- Linking jobs to a global `company` (part 8 adds `company_id`; `company` stays as the user's own spelling).
- Contacts and reminders. Notes cover them until there is demand.
- Full-text search over descriptions.

## Reading posting links

The new job dialog starts from the posting link, then shows a review step with the fields it filled marked. `POST /v1/postings/read` scrapes the page through the configured scraper and returns:

- **Fields from the page's schema.org `JobPosting` JSON-LD**, which job boards publish for search engines (Lever, LinkedIn, Workday, most ATSs): title, company, location, remote (`TELECOMMUTE`), employment type and salary. A salary per week or day has no job period and is dropped. Without JSON-LD, the title falls back to `og:title`.
- **The page as markdown**, which becomes the job description when none was pasted.
- **`extraction_id`**, when the owner selected an extraction model: the posting's text (the pasted text, else the page's markdown) was queued for it. `GET /v1/postings/extractions/{id}` is `pending` until it answers, then `completed` with the fields it read, or `failed`.

The dialog merges, highest first: the page's fields, then what the URL says (`web/src/app/jobs/posting-link.ts`: board, company and title from the path), then what the description says (work mode, employment type, salary, location). It fills only empty fields, and a new link replaces only values it filled that weren't edited. The dialog is extraction-first: pasting a link starts the read at once, a loader covers the page fetch and the extraction ("Fill it in myself" opens the form early and the answer still lands in it; Cancel returns to the link), and the form opens already filled, each filled field marked. The extraction fills empty fields and corrects ones guessed from the link or text, never the page's own data or edits. A salary is taken whole (amounts, currency and period) or not at all.

Errors: `503` without a scraper and no pasted text (the dialog then uses the link and text alone, silently); `422` on `url` when the page can't be read (blocked, `4xx`, slow) and there is no pasted text, which the review step shows. With pasted text, an unreadable page is reported in `page_error` and the text is extracted instead. A scraper that's down, or out of credits, is a `500`.

### Scrapers

`posting.Scraper` is the scrape API: one method that returns a page's markdown, raw HTML and metadata. `SCRAPER_PROVIDER` picks it (`firecrawl` or `none`; unset, it is `firecrawl` whenever a `FIRECRAWL_*` value is set). Firecrawl is the one provider today:

- **Production** uses [Firecrawl's API](https://docs.firecrawl.dev): set `FIRECRAWL_API_KEY` and leave `FIRECRAWL_URL` unset. Firecrawl then sees posting URLs.
- **Development** can use Firecrawl Local instead: `FIRECRAWL_URL=http://127.0.0.1:3002` with its `FIRECRAWL_TOKEN`.

A new provider implements `Scrape` (returning `*posting.UnreadableError` for pages it couldn't read) and gets a case in `cmd/zapply`'s `scraper`.

### Extraction models

Extraction models read typed fields out of text in one pass and bill input only ([OpenRouter's Extractions API](https://openrouter.ai/docs/guides/community/extractions), alpha). The owner selects one under **Models & keys → Posting extraction**; none selected means no extraction.

- `internal/model/extraction` is the provider-independent contract: a state, named typed fields, and one answer per field. `extraction.ProfileOf` says which models answer typed numbers (Clairic, Schematron, Liquid); the rest (ScaleDown Extract, Mercury Decide, Perplexity Decider) answer text only, so `posting.ExtractionRequest` asks them for salaries as text and `posting.FromExtraction` parses it (`$150,400`, `120k`, `1.2M`, `30 mil`, `€45.000`; a range in one answer gives its lowest amount for the minimum and highest for the maximum; "3+ years" and "40 hours" are not amounts). Switching models changes nothing for callers.
- `model.ExtractionProvider` is the provider side; `openrouter.Client` implements it on `POST /api/alpha/extractions`, with the catalogue from `GET /api/v1/models?endpoints=/api/alpha/extractions`.
- Extractions are `model_operation`s of kind `extraction`, quoted, reserved against the member's allowance and settled by the worker like decisions. Input is reserved once per field, since providers may split fields into passes that each read the whole text. Generations aren't looked up; an uncertain one waits for the owner, as for decision models.
- Answers under 0.5 confidence, nulls, and values that don't fit a job field (a work mode or period the job API has no value for, a bare `$` currency) are left empty for the person.

**Firecrawl Local.** In development, `make firecrawl` runs [Firecrawl Local](https://docs.firecrawl.dev/quickstarts/firecrawl-local) from `deploy/firecrawl/Dockerfile`: one binary built from crates.io at a pinned version, with Chromium for JavaScript pages. It's behind the `firecrawl` compose profile because its first build compiles Rust (several minutes); `make db-up` doesn't start it.

- It serves `POST /v2/scrape` on `127.0.0.1:3002`, requires the token, and refuses localhost and private-network targets, so a posting URL can't reach internal services.
- `FIRECRAWL_CLOUD=off` (the compose default) keeps every fetch on this machine. `auto` sends pages that block it to Firecrawl Cloud, which then sees their URLs. LinkedIn sometimes blocks; pasting the text still works.
- Chromium's sandbox needs `clone`, `setns` and `unshare`, which Podman's default seccomp profile allows. Docker's blocks them, so JavaScript pages come back unrendered with a warning; run with Playwright's seccomp profile there (Firecrawl's docs show how). Docker Compose can't pass that profile to Podman, so the compose file leaves it out.
- The production image doesn't include it (one Heroku dyno); production uses Firecrawl's API.

## Delivered

- **Migration `00004_jobs.sql`:** `job_status`, `job`, `job_activity`, `job_view`, their enums, forced RLS with the `tenant` policy (scoped to `zapply_app`), composite tenant FKs, and a partial unique index on `(user_id, normalized_url)`.
- **`internal/jobs`:** the service. It reuses the library's error types, so `writeError` maps both domains the same way, plus `DuplicateError`, which the API turns into a `409` with `existing_id`.
  - Statuses are seeded inside the first transaction that needs them, behind a per-user advisory lock.
  - Job positions are allocated under a `FOR UPDATE` lock on the destination status row. The lock order is always status rows, then job rows, so moves, `PUT`s and status deletes can't deadlock each other.
  - `content_hash` covers title, company and description.
- **API:** `/v1/job-statuses`, `/v1/jobs` (plus `move`, `archive`, `unarchive`, `activities`), and `/v1/job-views`. OpenAPI is now 0.4.0. Lists load ids in the requested order first, then the summaries by id, so every sort shares one row type and the description and notes columns stay out of lists.
- **Events:** the topics listed above, added to `EventTopic`. `EventData` gains `job_id` for activity events.
- **Web UI:** the board, table, saved views, job sheet and column manager. See `docs/parts/09-web.md`.

### Tests

- **Unit:** URL normalization, covering the scheme and host case, default ports, tracking parameters, trailing slashes and rejected schemes.
- **HTTP (Postgres):**
  - `TestJobStatusesSeedOnce`: 8 concurrent first reads seed one set. Covers create, rename, reorder, duplicate names (`409`), validation, and tenant isolation.
  - `TestJobLifecycle`: duplicate URLs through another link return `409` with `existing_id`, and another user may save the same URL. Covers validation, `applied_on` set on entering an active stage, the activity timeline, `PUT` with `428`/`412`, a status change through `PUT`, notes (create, edit, delete, generated entries read-only), archive filtering and cross-tenant `404`s.
  - `TestJobBoardOrdering`: newest first, reorder within a column, invalid anchors, no activity for moves within a column, closing a saved posting without setting `applied_on`, deleting a status with `move_to`, and keeping the last status.
  - `TestJobConcurrentInsertsKeepDistinctPositions`: 10 concurrent creates in one column get distinct keys.
  - `TestJobListFiltersAndPages`: cursor pages, stage and status filters, `q` treating `%` and `_` literally, invalid filters and cursors, and a cursor from one sort rejected by another.
  - `TestJobViews`: create, list order, validation, update, tenant isolation and delete.

### Notes

- Seeding emits no events. Anyone who reads the statuses next seeds the same set or sees it.
- A `PUT` that also changes status bumps the version twice and emits `job.updated` then `job.moved`. Clients only rely on the latest `ETag`.
- `JobViewFilters` is closed in OpenAPI (`additionalProperties: false`), but the generated decoder ignores unknown keys rather than rejecting them.
- **Deploying:** apply migration 00004 with `zapply migrate` against the production database before (or right after) rolling out this API. Until then, the job endpoints fail with `500`.

### Independent review fixes (GPT 6.1 Sol)

The first review covered the jobs API and Angular UI through `245ac47`. It
confirmed nine issues with Postgres reproductions and controlled frontend
responses. The following fixes include regression coverage:

| Finding | Fix | Regression coverage |
|---|---|---|
| Account changes retained another account's board | Clear jobs, statuses, views and load flags on principal changes; discard responses from an earlier session, including stale 401s | Frontend account switch with an outstanding list |
| Column deletion could pull back a job concurrently moved elsewhere | Recheck source membership after locking each job; skip moved or deleted rows | Postgres destination-lock barrier and concurrent move |
| Late list/move/archive responses replaced newer data | Reconcile changes during pagination, compare versions before removing archived jobs, and keep delete tombstones | Deferred lists/moves, archive versions, deletion during paging, missed-event resync |
| A late sheet response could replace the newly selected job | Guard job loads, activity loads and mutation completions by selection generation | Reverse-order loads, delayed activity, delayed status action |
| Status/archive actions discarded draft edits | Merge server metadata and ETag while preserving locally changed form fields | Draft notes survive status and archive changes |
| Valid Unicode URLs exceeded the unique index's entry limit | Migration `00005_job_url_digest.sql` indexes a fixed-size SHA-256 digest; retain the full normalized URL for exact lookup | Long Unicode URLs, duplicate detection and distinct URLs |
| Escaped and literal path separators shared a duplicate key | Trim trailing literal separators while preserving escaped-path semantics | Encoded slash versus literal slash, including trailing encoded separators |
| Views filtered by deleted statuses became empty | Drop absent IDs before applying column and job filters; an empty selection is unrestricted | Removed-only and mixed status filters |
| NUL text reached Postgres and returned 500 | Reject NUL in the shared jobs text validator with field errors | HTTP `422` for a NUL in the job title |

Frontend regressions run with `cd web && npm test` and are included in CI. They
execute the production class methods using controlled DI/signals and deferred
responses; the original harness does not render Angular templates or schedule Angular effects.
A second harness uses real Angular DI, signals and a controlled root-effect
scheduler to cover mounted account changes and conflict recovery.
Type checking and the production build separately validate templates.

Existing databases must apply **migrations 00004–00006** before the
updated API is deployed. The digest function fixes its encoding to UTF-8 and
uses PostgreSQL's built-in SHA-256, requiring no extension. Rolling back 00005
recreates the original text index and can fail if long URLs have since been
saved.

Validation after the fixes: full Go integration suite with `-race`, `go vet`,
unchanged sqlc output, frontend regressions, TypeScript checks and the production
Angular build passed. The updated local app also passed browser checks for
account switching, draft notes surviving status and archive changes, saving with
the refreshed ETag, and the Zard delete dialog (Cancel kept the fixture; Delete
returned `404`). The disposable browser fixture was removed afterward.

A second independent pass identified four follow-ups, also fixed:

| Finding | Fix and regression |
|---|---|
| A mounted sheet retained the previous principal's private document and drafts | Observe identity, close and clear the sheet, invalidate pending loads; tested with real Angular effects |
| A mounted board stayed empty after a cross-tab principal change | Reload automatically when an authenticated principal changes after the board was requested; tested without remounting or SSE |
| `412` recovery left Save loading | Release saving state before the quiet conflict reload; test preserves the draft and new ETag |
| Existing normalized URLs retained the old escaped-path bug | Registered Go migration **00006** backfills from original URLs using the exact normalizer used for new writes; an upgrade regression verifies encoded-URL duplicate detection and distinct literal paths |

Migration 00006 takes a table lock and repairs keys atomically. It runs through
`zapply migrate` together with the SQL migrations, using the schema-owner
connection. It restores forced RLS before committing and never merges or deletes
user documents. If corrected keys conflict, it rolls back with the affected job
ID. Shared URL normalization lives in `internal/joburl` to keep writes and the
backfill consistent.
