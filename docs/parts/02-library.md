# Part 2: Library (evidence model)

Status: built (see "Delivered" at the end).

The library holds everything a person can truthfully claim. Matching (part 6) ranks it against a job, and rendering (part 7) lays out the selected subset. Neither ever writes content into it. The model has to fit any career: a nurse, an academic, a freelance designer, an electrician, a career changer. So it is built on **evidence**, not on "jobs and projects".

## Two axes

Every evidence has a **category** and a **form**.

**Category** decides the CV section and the paid/unpaid framing:

| Category | Meaning |
|---|---|
| `education` | Learning, academic work, and credentials earned by study or exam |
| `paid` | Compensated professional work |
| `unpaid` | Uncompensated work: volunteering, community roles, unpaid internships, personal projects, career breaks |

**Form** decides the fields an evidence has and how it nests:

| Form | What it is |
|---|---|
| `engagement` | A period of involvement, usually with an organization |
| `credential` | Something issued that can be verified |
| `output` | A discrete thing produced |
| `recognition` | An honor or funding someone else granted |

## Kinds (fixed for the MVP)

**Everything in the library is publishable.** There are no private fields: the library is the material a person sells from. Choosing what to show is a per-CV selection (`auto_include`, plus picks for each CV), not a privacy setting. Data used only when applying (work authorization, sponsorship, salary expectations) is not career evidence and belongs with autofill (part 11).

The kinds fix the *data* each evidence carries, so the renderer can count on its fields. *Layout* (how each kind is set as a CV line, and section headings for each locale) is code in the part 7 templates, keyed by kind, not data in this table.

A `kind` belongs to exactly one form and allows only some categories. The list is a static reference table seeded by migration and served by `GET /v1/kinds`, so the UI can build its forms from it. Adding a kind is a new migration plus a details schema.

| Kind | Form | Categories | `details` (all optional unless noted) |
|---|---|---|---|
| `degree_program` | engagement | education | `degree_type`* (secondary, associate, bachelor, master, doctorate, diploma, other), `field_of_study`, `status`* (completed, in_progress, incomplete), `grade`, `honors` |
| `course` | engagement | education | `format` (course, bootcamp, workshop, mooc, training), `provider_url`, `hours` |
| `job` | engagement | paid | `employment_type` (full_time, part_time, temporary, seasonal), `work_mode` (onsite, remote, hybrid) |
| `freelance` | engagement | paid | `work_mode` |
| `business` | engagement | paid | `role` (founder, co_founder, owner, partner), `stage`, `team_size` |
| `internship` | engagement | paid, unpaid, education | `work_mode` |
| `apprenticeship` | engagement | paid, education | `trade` |
| `military_service` | engagement | paid | `branch`, `rank` |
| `volunteering` | engagement | unpaid | `cause` |
| `membership` | engagement | unpaid, education | `role` (member, officer, board, chair) |
| `career_break` | engagement | unpaid | `label` (e.g. "Family caregiving", "Sabbatical") |
| `assignment` | engagement | *inherits from parent* | `client` (child-only: a client within a freelance practice, or a rotation or secondment within a job, residency or degree) |
| `license` | credential | education | `issuer`*, `number`, `jurisdiction` |
| `certification` | credential | education | `issuer`*, `credential_id`, `verify_url`, `score` |
| `clearance` | credential | education | `level`*, `issuer` |
| `project` | output | any | `role`, `repo_url` |
| `publication` | output | any | `publication_type`* (journal, conference, book, chapter, preprint, thesis, article, report), `venue`, `authors` (list), `doi` |
| `presentation` | output | any | `event`*, `format` (talk, poster, workshop, panel) |
| `patent` | output | any | `number`, `office`, `status` (filed, granted) |
| `creative_work` | output | any | `medium` (exhibition, performance, film, recording, portfolio, other), `venue` |
| `award` | recognition | any | `issuer`* |
| `funding` | recognition | any | `funding_type`* (grant, scholarship, fellowship), `funder`*, `amount`, `currency` |

`*` marks a required field. Every evidence also has a top-level `url` (a link to the paper, repo, portfolio or credential page), so no kind repeats it in `details`. Language test scores (IELTS, JLPT) are `certification`s, and spoken languages are skills (see below).

### Career breaks

Career breaks are evidence. An unexplained gap costs candidates more than a stated one, and career changers and caregivers need a truthful way to show their timeline. They follow these rules:

- **Excluded from CVs by default** (`auto_include = false`). Matching never adds one; the user includes it explicitly.
- Only a free-text `label` the user writes. There is no "reason" field: the app never needs to know why, so it doesn't collect it.
- No organization is needed. Highlights and skills are allowed ("Managed household finances").

## Rules (enforced in the database, validated in the API)

- `form` must match the kind's form, through a composite FK to `evidence_kind(kind, form)`.
- `category` must be one of the kind's allowed categories.
- **Nesting:** at most 2 levels.
  - A child's category always equals its parent's.
  - `assignment` must have a parent, and the parent must be an engagement.
  - Outputs and recognitions may nest under engagements; credentials don't nest.
  - Examples: a job's projects, a PhD's thesis and awards, a freelance practice's client assignments.
- **Dates** have an explicit precision. A degree can say `2019`, a job `2021-03`, a license `2023-06-14`; nothing is faked as `2019-01-01`. Each form maps them to its own labels:

  | Form | `start` | `end` |
  |---|---|---|
  | engagement | start | end, or `is_current` |
  | credential | issued | expires (nullable) |
  | output | published / released | — |
  | recognition | granted | — |

  `end >= start`; `is_current` implies `end` is null.
- **Tenancy:** every row carries `user_id`, and FKs between tenant rows are composite `(user_id, id)`, so nothing can point across tenants.

## Schema

```
profile           user_id PK · full_name · headline · email · phone · location
                  links jsonb [{label,url}] · summary · version · updated_at

organization      id · user_id · name · normalized_name (unique per user) · website · location
                  type (company, education, nonprofit, government, client, other)

evidence_kind     kind PK · form · categories[] · child_only · details_schema jsonb   (global, read-only)

evidence          id · user_id · parent_id? · organization_id? · category · form · kind
                  title · summary · location · url
                  start_date · start_precision · end_date? · end_precision? · is_current
                  details jsonb · auto_include · position · version · content_hash
                  archived_at? · created_at · updated_at

highlight         id · user_id · evidence_id · text · metric_value? · metric_unit?
                  auto_include · position · content_hash

skill             id · user_id · name · normalized_name (unique per user)
                  type (hard, tool, soft, language) · proficiency?
                  (language: CEFR A1–C2 or native; others: beginner–expert)

evidence_skill    user_id · evidence_id · skill_id
highlight_skill   user_id · highlight_id · skill_id

outbox            id · user_id · topic · payload jsonb · created_at     (insert-only for the API)
idempotency_key   user_id · key · request_hash · status · body · created_at
```

- `title` holds whatever is primary for the kind: the role title, degree name, credential name, output title or award name.
- `position` is a fractional sort key (base-62 text, `COLLATE "C"`) within the parent, or within the category for top-level evidence. Moving an item rewrites one row.
- **`content_hash`** is SHA-256 over canonical JSON of what matching reads. It is computed in Go on write.
  - **Highlight:** text, metric, skill names.
  - **Evidence:** kind, category, title, summary, dates, details, organization name, skill names, and the ordered hashes of its highlights.
  - Renaming a skill or organization recomputes the hashes of everything linked to it, in the same transaction.
  - Part 6 caches scores by this hash.
- `version` increments on every write and is returned as the `ETag`.

## API (OpenAPI 3.0.3, `/v1`)

| Endpoint | Notes |
|---|---|
| `GET /kinds` | The catalog: form, categories, child_only and the details JSON Schema for each kind |
| `GET, PUT /profile` | `PUT` requires `If-Match` |
| `GET, POST /organizations`, `GET, PATCH, DELETE /organizations/{id}` | `DELETE` is rejected while evidence references the organization |
| `GET /evidences` | Filters: `category`, `form`, `kind`, `parent_id` (or `top_level=true`), `include_archived`. Cursor pagination |
| `POST /evidences` | Creates one **document**: the evidence, its `highlights[]` and `skills[]` (by name; missing ones are created). `Idempotency-Key` supported |
| `GET /evidences/{id}` | The document plus summaries of its children |
| `PUT /evidences/{id}` | Replaces the whole document atomically. Requires `If-Match`; a stale version returns `412` |
| `POST /evidences/{id}/move` | `{parent_id?, after_id?, before_id?}`. Re-parenting re-checks the rules |
| `POST /evidences/{id}/archive`, `/unarchive` | |
| `DELETE /evidences/{id}` | Returns `409` if it has children, unless `?cascade=true` |
| `GET, POST /skills`, `PATCH, DELETE /skills/{id}`, `POST /skills/{id}/merge` | `merge` takes `{into}` and re-links everything to the target, then deletes the source |

- Errors are `problem+json`. Validation errors list the failing fields: `errors: [{field, message}]`.
- Every write inserts an `outbox` row in its transaction. Topics: `profile.updated`, `organization.{created,updated,deleted}`, `evidence.{created,updated,moved,archived,deleted}`, `skill.{created,updated,merged,deleted}`. The payload is `{id, version}`, not the full row.

## Tooling

- `api/openapi.yaml` is the source of truth. **oapi-codegen** generates a strict `net/http` server interface into `internal/api/gen`.
- **sqlc** generates typed queries from `internal/store/queries/*.sql`.
- Both are pinned with `go tool` in `go.mod`. `make generate` runs them, and CI fails if the generated code differs from what's committed.

## Out of scope

Custom kinds, attachments and files (part 7, object storage), cvd import, public profile pages, full-text search.

## Delivered

- **Migration `00002_library.sql`:**
  - The tables, the enums, and the `evidence_kind` seed.
  - Composite tenant FKs.
  - Forced RLS on every tenant table.
  - `outbox` is insert-only for the API.
- **Database backstops:**
  - The `evidence_check` trigger enforces the category and nesting rules, using SQLSTATE `ZA001`, which the API maps to 422.
  - A deferred constraint trigger keeps children in their parent's category.
- **`internal/library`** holds the domain:
  - The kinds catalog, including the details schemas served by `/v1/kinds`.
  - Partial dates.
  - Fractional positions: appends stay two characters long for about 1,900 items.
  - Validation, content hashing and the service.
  - Each write is one tenant transaction plus its outbox event.
- **Generated code, pinned with `go tool`:**
  - `api/openapi.yaml` → oapi-codegen strict `net/http` server (`internal/api/gen`).
  - `internal/store/queries` → sqlc (`internal/store/db`).
  - Run `make generate` to regenerate. CI fails if the committed generated code is stale.
- **HTTP behavior:**

  | Situation | Response |
  |---|---|
  | Validation failure | `422` with an `errors[]` entry for each field |
  | `If-Match` missing on `PUT` | `428` |
  | Stale `If-Match` | `412` |
  | Conflicts | `409` |
  | Malformed JSON, bad parameters | `400` |
  | Body over 1 MiB | `413` |

- **Store:** pgx exec mode now also registers `uuid` and `uuid[]`, which it can't infer from Go types.

### Tests

- **Unit:**
  - Partial dates: parsing, precision, and an end date overlapping the start's period.
  - Positions: 2,000 random inserts stay strictly ordered, repeated prepends work, and invalid keys are rejected.
- **Service (Postgres):**
  - Lifecycle: kept highlight IDs keep their hashes, a stale version is rejected, and renaming an organization rehashes without bumping the version.
  - A move adopts the parent's category, and a cascade delete removes the children.
  - Validation, including the nesting rules.
  - The kinds table and the Go catalog agree.
  - The outbox records committed writes only.
  - Eight concurrent retries with the same idempotency key create one evidence.
- **HTTP:**
  - Profile ETags (`428`, `412`).
  - Idempotency: a replay returns the same evidence, and the same key with a different body gets `422`.
  - Validation errors list the failing fields.
  - Children, highlight IDs kept across `PUT`, cross-tenant `404`/`422`, archive filtering, and cascade.
  - Cursor pagination, and moves before or after a sibling.
  - Request errors.

### Review fixes (independent review, round 1)

| Finding | Fix | Regression test |
|---|---|---|
| A rehash (organization or skill rename) could overwrite a concurrent `PUT`'s hash with one computed from stale content | `rehash` locks the rows (`FOR UPDATE`, in id order) before reading them | `TestRehashWaitsForConcurrentWrite` |
| Oversized lists were validated item by item: 20k highlights produced a 1 MB error response | Oversized lists fail at once, and responses carry at most 50 field errors | `TestOversizedListsFailFast`, `TestBodyLimits` |
| Equal positions made a move silently land in the wrong place. They came from a `PUT` that changed category, or from concurrent appends | A category change appends in the new category. Top-level allocation is serialized with an advisory lock for each category, and children by locking their parent. Moves repair legacy ties | `TestCategoryChangeGetsAFreshPosition`, `TestMoveRepairsTiedPositions` |
| An idempotent retry returned the current document, not the original result | The created document is stored with the key and replayed verbatim, even after edits or deletion | `TestIdempotentReplayReturnsOriginal` |
| Implicit changes emitted no events: auto-created skills, and children recategorized with their parent | `skill.created` and `evidence.updated` are emitted for them. Renaming, merging or deleting a skill also bumps the version of linked documents and emits `evidence.updated`, since their `skills` list changed | `TestImplicitChangesEmitEvents` |
| `-0` hashed differently from the `0` Postgres stores | Numbers are canonicalized before hashing and storage | `TestNegativeZeroHashesStably` |
| Bytes after the first JSON value were ignored, including 2 MiB of whitespace | The body is read in full (≤ 1 MiB) and must be a single JSON value | `TestBodyLimits` |
| An invalid `category` or `form` filter, or cursor category, caused a `500` | They are validated and return `422` with a field error | `TestListRejectsBadFilters` |

Idempotency keys are kept forever for now. A cleanup job (24 h retention) arrives with the worker in part 5.
