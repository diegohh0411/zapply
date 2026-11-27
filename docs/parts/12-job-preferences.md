# Part 12: Member job preferences

Status: built. This is step 2 of the job-alerts redesign; part 13 routes alerts
by these preferences. Each member gets
preferences in the catalog's terms (part 11). Alerts don't read them yet; step 3
filters alerts with them.

## What a member sets

Settings → Job preferences, in four groups:

| Group | Fields | Values |
|---|---|---|
| `seniority` | `seniority_min`, `seniority_max` | `catalog.Seniorities`, min ≤ max |
| `areas` | `areas` | `catalog.Areas`, except `other` in suggestions |
| `location` | `countries`, `remote_ok` | ISO 3166-1 alpha-2 codes (`catalog.CountryCode`) |
| `employment` | `excluded_employment` | `catalog.EmploymentTypes` to skip |

Each group has a source: `derived` (it follows the library) or `member` (the
member set it). Re-deriving always refreshes the stored suggestion (`derived`),
but it only copies the suggestion into groups that are still `derived`. "Use
suggestion" (the undo icon) and "Reset all to suggestions" set groups back to
`derived`.

## Derivation rules (`preferences.Derive`)

The rules read top-level, unarchived engagements with dates.

- **Work time** is the union of months, so overlapping work counts once.
  Finished work counts through its end month. Current work counts through last
  month.
  - Paid `job`, `freelance`, `business` and `military_service` count fully.
  - `internship` and `apprenticeship`, paid or not, count half. Paid work in a
    month a `degree_program` also covers counts half too. All half-weight time
    together adds at most one year.
  - Volunteering, courses, memberships, career breaks and education add
    nothing.
- **Seniority range** comes from the years: under 1 → intern–entry; under 3 →
  entry–mid; under 7 → mid–senior; 7 or more → senior–staff.
- **Manager:** when the latest paid work has a manager's title (Manager, Head,
  Director, VP, Lead of, Gerente, Jefe, Coordinador, Chief) and there are at
  least 3 years of work, the range tops out at `manager`. "Tech Lead" isn't a
  manager title.
- **Skip internships** from 2 years of work.
- **Countries:** the profile location's country. If the profile has none, the
  location of the latest paid work. Remote OK defaults to true.

Examples (`TestDerive`):

| Library | Years | Range |
|---|---|---|
| Student with two 3-month internships | 0.3 | intern–entry |
| 18 months in a current job | 1.5 | entry–mid |
| 10 years as an engineer, with overlapping freelance work | 10 | senior–staff, skip internships |
| 8 years, now an Engineering Manager | 8 | senior–manager |
| 2 years of work during a degree, then 4.8 years as Tech Lead | 5.8 | mid–senior |

## Areas (one model call, charged to the member)

The question is a `decide` choice over the catalog's areas. It uses the same
option criteria as the posting classifier (`catalog.AreaCriteria`), so a member
and a posting read areas the same way.

- **Input:** the 8 most recent work and internship entries (kind, title,
  organization, months), the latest degree's title, the profile headline, and
  the 20 hard or tool skills used most. No contact details, summaries or
  highlights.
- **Output:** every area with probability ≥ 0.2, most probable first, at most
  3, never `other`.
- **Model:** `posting_decision_model` (Admin → Models → Posting catalog). With
  no model set, the rules still derive, and areas stay empty until the member
  picks them.
- **Cost:** the member pays, through the usual reserve and settle path. The
  input comes from their own library. A posting decision costs about
  $0.0000026.
- **Caching:** the hash of the model and the input is stored
  (`area_input_hash`). An unchanged input asks nothing. A changed one checks
  `decision_answer` first, then queues one operation. `preferences.OnTerminal`
  (in the worker's chain, before mail and matching) writes the answer. A failed
  or cancelled question keeps the earlier areas.

## Re-deriving

A trigger on `outbox` (topics `evidence.*`, `skill.*`, `organization.*`,
`profile.*`) marks the member's preferences `stale`. Two things re-derive:

- the next `GET /v1/job-preferences`, in the request's transaction;
- the worker's recovery (every minute), for members stale for at least 5
  seconds (`platform.stale_job_preferences`).

## API

- `GET /v1/job-preferences`: current values, `sources`, `suggested` (with
  `work_years`, `enrolled`, `manager`), `derived_at`, and `deriving` (an area
  question is open). ETag is the version.
- `PUT /v1/job-preferences` with `If-Match`: the groups present become the
  member's. Invalid values answer 422; a stale version answers 412.
- `POST /v1/job-preferences/reset` with `If-Match` and `{"fields": [...]}`.

## Schema

Migration 00047 adds `job_preference`, one row per member (tenant RLS for app
and worker). It also adds the `invalidate_job_preference` trigger and
`platform.stale_job_preferences(limit)`.
