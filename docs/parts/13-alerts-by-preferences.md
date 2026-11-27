# Part 13: Job alerts by preferences

Status: built. This is steps 3 and 4 of the job-alerts redesign together.
Members don't watch boards or searches anymore. The platform finds postings
once, labels them (part 11), and alerts every member whose job preferences
(part 12) each posting fits.

## Flow

1. **Supply** (`alerts.Supply`, River job `posting_supply`). Each supplier runs
   every 30 minutes (`alerts.SupplyInterval`) from the worker's recovery tick.
   It asks for what members accept, then queues what it found as one
   `posting_ingest` job. That job is inserted in the transaction that moves the
   supplier's cursor, so a run's postings are never lost and never bought
   twice. A run is attempted once: a retry would pay again.
2. **Ingest** (`alerts.Ingest`, `posting_ingest`). It adds the sightings to the
   catalog (`catalog.Add`), which queues classification, then routes whatever
   is ready.
3. **Route** (`alerts.Route`, `alert_route`). It runs after ingest, when a
   posting's classification stage finishes (`catalog.Labeled`, in the same
   transaction), and on every recovery tick. It files an alert per fitting
   member and marks the posting `routed_at`.

The alerts are filed before the posting is marked routed, so a crash routes it
again. `job_alert`'s unique `(user_id, posting_id)` files nothing twice, which
makes delivery at-least-once and idempotent.

## Suppliers

Both suppliers use TheirStack, with the owner's key and page size (Admin →
Providers → Job search). The platform pays; no member's ledger is charged.
`platform.supplier` keeps each supplier's cursor, failures and
`credits_total`.

| Supplier | Asks for |
|---|---|
| `theirstack:countries` | Postings in any country an active member accepts (`platform.supply_scope()`) |
| `theirstack:remote` | Remote postings anywhere, when some member has remote OK |

When no member wants what a supplier covers, it buys nothing and moves its
cursor to now. A provider failure backs off (doubling, at most a day) and keeps
the cursor.

## When a posting is ready

A posting routes once each classification stage that has a model has
finished, answered or failed. A stage with no model doesn't hold anything. A
posting still waiting after **10 minutes** (`alerts.Hold`) routes on whatever
is known, because a late alert costs a member more than a loose one.

## Fit (`alerts.Fits`)

The audience is every active member with a `job_preference` row, except the
catalog's member (`platform.alert_audience()`). A posting fits when all of
these hold:

- **Seniority** is within the member's range.
- **Areas:** one of the posting's areas is among the member's.
- **Employment type** isn't one the member excludes.
- **Location:** the posting's countries overlap the member's, or it is remote
  with no countries (anywhere) and the member has remote OK.

Anything unknown passes:

- A missing label, or one under **0.6** confidence (`alerts.MinConfidence`), is
  ignored.
- An empty preference group sets no limit, so a member with no preferences set
  gets every posting.
- A posting in no known place passes the location rule.

Remote and countries fall back to what the source said (`posting.remote`,
`posting.country`) when there is no sure label.

A posting whose link is already one of the member's jobs isn't alerted. There
is no cap per member.

## Schema (migration 00048)

- **Dropped:** `job_watch`, `job_alert_key`, and the board poller's and search
  watchers' platform tables and functions. `platform.search_charge` stays, as
  members' usage history.
- **`job_alert`** is recreated: `posting_id` replaces `watch_id` and
  `posting_key`, with unique `(user_id, posting_id)`. Earlier alerts and their
  notifications are deleted.
- **`platform.posting.routed_at`:** postings already in the catalog count as
  routed.
- **Added** `platform.supplier`, `platform.alert_audience()` and
  `platform.supply_scope()`. `platform.search_config()` no longer needs a
  credit price.

The migration doesn't reverse.

## Removed

- `/v1/job-watches` and the Job alerts page. Alerts arrive as notifications.
- The board poller and `internal/ats`.
- `zapply catalog-backfill`.

## Not done yet

- Plans still carry `max_search_watchers`, and the search settings still carry
  a credit price. Neither is used.
- No ATS board supplier.
