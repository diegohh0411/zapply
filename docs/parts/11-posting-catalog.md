# Part 11: Posting catalog and classifier

Status: built. This is step 1 of the job-alerts redesign: it collects and
labels postings. Nothing members see changes: alerts are still matched by
title keywords and delivered exactly as before.

## Why

Alerts match a watch's keywords against a posting's title. That misses
"Practicante de ventas" for someone watching "sales intern", and can't answer
"senior roles I can do remotely from Mexico". The redesign matches on what a
posting *is*. Step 1 builds the inputs for that: every posting the platform
sees, stored once, with labels a model reads off it.

## The catalog

Migration 00043, package `internal/catalog`. All tables live in `platform`,
which no member can read.

| Table | Holds |
|---|---|
| `platform.posting` | One row per opening: company, title, location, URL, remote flag, country (from the source or the location), description and the source's seniority when given. |
| `platform.posting_identity` | `key` → posting, unique on `key`. |
| `platform.posting_label` | The labels, per-field confidence, the decision and extraction models used, and `version` (`catalog.LabelVersion`). |
| `platform.posting_stage` | The two classification stages per posting: open operation, attempts, done/failed. |
| `platform.posting_backfill` | Per-member cursor for the alert backfill. |

### Identities

The same rules as `job_alert_key`, with the same folding (`catalog.Fold`,
`catalog.Company`, now shared by `internal/alerts/keys.go`). `job_alert_key`
itself is untouched.

- `ats:<ats>:<board>:<posting>`: the ATS's own id.
- `url:<normalized URL>` via `joburl.Normalize`.
- `fp:<company|title|country>`. Country, not city, so an aggregator's "Remote
  (US)" sighting meets the board's "Brooklyn, NY". Without a known country the
  third part is the folded location, so two places in an unknown country stay
  apart. `catalog.Country` reads countries from locations ("Austin, TX",
  "Toronto, ON, CA", "Remote - Spain", English and Spanish names).

Resolution takes the strongest key that matches, in that order, except:

- **The `from_ats` rule:** a fingerprint never joins two ATS board postings.
  Two board postings are the same opening only by their ATS key.
- A fingerprint lapses 60 days after its last sighting
  (`catalog.FingerprintTTL`, shared with alerts). The next posting with it is
  a new opening and takes the key over.

A key another posting already holds stays with it. Each sighting's keys are
locked (advisory locks, in sorted order) before lookup, so concurrent polls
can't create the same posting twice. Later sightings fill empty fields
(country, description, seniority hint) but never overwrite them.

## Ingest

- **Board poll** (`alerts.Poll`): after alerts are filed and marked delivered,
  it catalogs every board posting not yet cataloged
  (`platform.ats_posting.cataloged`). Baseline postings count too, so existing
  boards fill the catalog on their next poll.
- **Search poll** (`alerts.PollSearch`): after filing, it catalogs every usable
  posting the search paid for, matched or not, with TheirStack's
  `country_code`, `description` and `seniority` (as a hint).

Cataloging runs after delivery and can't fail a poll: errors are logged, and
uncataloged board postings are retried on the next poll.
`TestCatalogLeavesDeliveryUnchanged` runs the same board history with no
catalog, a working one and a broken one, and gets identical alerts.

## Classification

OpenRouter only serves d1.2's decision side (`noul`, `choice`, `score`, `box`).
Typed `text` and `number` questions are rejected with 400, and `choice` with
`multiple` is silently dropped (both tested live, 2026-11-25). So each posting
gets **two operations**. Each stage has its own owner-selected model:

| Stage | Setting | Labels |
|---|---|---|
| decision (`model_operation.kind='decision'`) | `posting_decision_model` | `seniority`, `areas`, `employment_type`, `remote` (`noul`), `min_years` (choice of buckets: unstated, 0, 1, 2, 3, 4, 5, 6, 8, 10) |
| extraction (`kind='extraction'`) | `posting_extraction_model` | `specialty` (text), `countries` (list → ISO 3166-1 alpha-2), `languages` (list → ISO 639-1) |

Both settings are off by default, and a stage whose model isn't set waits.
The owner sets them through `PUT /v1/admin/model-settings`
(`posting_decision_model`, `posting_extraction_model`; empty turns a stage
off).

**`areas`:** a decision answers one choice but gives a probability for every
option, so the runner-up becomes the second area when it has ≥ 0.2 and
neither is `other`. In the live test a Solutions Engineer came back
`engineering_software` 0.65 and `sales` 0.32.

**Prompt:** it handles English and Spanish titles (Practicante, Becario,
Recién egresado, Gerente, Jr/Sr, …). It also spells out the boundary cases:
Solutions/Sales Engineers are `engineering_software` first and `sales`
second, and AI-trainer gig work (Mercor, Alignerr, Outlier, …) is
`ai_training`. TheirStack's seniority goes in as `posting.board_seniority`,
which the instructions call a hint.

**Confidence:**

- A choice stores the model's own `confidence`.
- `remote` stores max(p, 1 − p).
- Extraction fields store the model's confidence when it reports one.
  Answers under 0.5 are dropped.

**Flow:** the sweep (`catalog.Sweep`) runs on every recovery tick and when a
new posting or a finished operation wakes it (`posting_classify` job). It
starts at most 6 operations at a time, newest postings first, staying under
the ledger's 8 concurrent calls per member. Operations go through
`worker.Runtime` like every other call: quoted, reserved, settled, and
reconciled by generation ID when uncertain. Decisions also hit
`decision_answer`, so an identical posting reuses the cached answers without
a call. `catalog.OnTerminal` runs first in the worker's terminal chain and
claims every operation of the catalog's member. It validates the answers
(`decide.Ingest`, `extraction.Answers`) and writes the labels. A stage whose
answers don't validate is retried once and then marked `failed`.

## Who pays

Classification is charged to a **dedicated internal member**,
`catalog.SystemMember` (`00000000-0000-7000-8000-0000000ca7a1`), created by
the migration with an active membership on the plan "Internal: posting
catalog": $10 per 7 days and $30 per 30 days.

- **Not members:** a posting is shared by everyone who might see it, so no
  member's allowance should pay for it.
- **Not the owner's account:** catalog spend would mix with the owner's own
  usage and hit their caps.
- **Kept on the ledger:** the existing reserve and settle path, caps, usage
  history and uncertain-charge handling apply unchanged. The spend shows in
  the admin pages like any member's, and the owner can raise the caps there.

At the observed $0.0000026 per decision call, the caps are far above the
expected spend. When they're hit, operations block with `usage_limit` and
resume when the window allows.

## Backfill

`zapply catalog-backfill` (worker database URL) adds every posting members
were already alerted about. It walks members who watch anything
(`platform.watching_members`), reads each member's `job_alert` rows in id
order in their tenant, and resolves them with ATS keys for board watches. The
member's cursor moves in the same transaction as each batch, so the backfill
can be stopped at any point and re-run safely: a second run reads nothing,
and a resumed one starts at the cursor (`TestBackfillIsIdempotent`). New
postings wake the classifier.

## Not in step 1

- Matching alerts on labels, and anything members see.
- Re-labeling when `LabelVersion` changes. Labels record their version, so a
  later step can select stale ones.
- An admin page control for the two posting models. They're set through the
  API.
