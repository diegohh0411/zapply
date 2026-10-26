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

- Lists return summaries without `description` and `notes`; a board of 200 postings would otherwise be megabytes.
- Errors, ETags (`version`), `If-Match` (`428`/`412`) and validation (`422` with `errors[]`) work exactly as in part 2.
- **Events** (outbox, delivered by part 3):
  - `job.{created,updated,moved,archived,unarchived,deleted}`: `{id, version}`
  - `job_status.{created,updated,moved,deleted}`: `{id}`. A delete with `move_to` also emits `job.moved` for each job it moved
  - `job_activity.{created,updated,deleted}`: `{id, job_id}`
  - `job_view.{created,updated,deleted}`: `{id}`

## Out of scope

- Fetching and parsing a posting from its URL (needs the worker, part 5; the extension in part 11 will send the text it sees).
- Linking jobs to a global `company` (part 8 adds `company_id`; `company` stays as the user's own spelling).
- Contacts and reminders. Notes cover them until there is demand.
- Full-text search over descriptions.

## Delivered

See the bottom of this file after the build.
