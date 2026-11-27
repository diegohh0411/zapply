# Part 13: Job alerts by preferences

Status: built. This is steps 3 and 4 of the job-alerts redesign together.
Members don't watch boards or searches anymore. The platform finds postings
once, labels them (part 11), and alerts every member whose job preferences
(part 12) each posting fits.

## Flow

1. **Supply** (`alerts.Supply`, River job `posting_supply`). Each supplier runs
   every 30 minutes (`alerts.SupplyInterval`). The recovery tick moves a
   supplier's next run in the same transaction that queues its job. A run reads
   one page, then queues what it found as one `posting_ingest` job, in the
   transaction that moves the supplier on. A run is attempted once: a retry
   would pay again.
2. **Ingest** (`alerts.Ingest`, `posting_ingest`). It adds the sightings to the
   catalog (`catalog.Add`), which queues classification, then routes whatever
   is ready.
3. **Route** (`alerts.Route`, `alert_route`). It runs after ingest, when a
   posting's classification stage finishes (`catalog.Labeled`, in the same
   transaction), and on every recovery tick. In one transaction it locks a
   batch of ready postings, files each fitting member's alerts in that
   member's tenant (`set_config('app.user_id')`), and marks the postings
   `routed_at`. It needs one connection however many members match.

Alerts and the routed mark commit together. `job_alert`'s unique
`(user_id, posting_id)` files nothing twice if a posting is routed again.

## Suppliers

Both suppliers use TheirStack, with the owner's key and page size (Admin →
Providers → Job search). The platform pays; no member's ledger is charged.
`platform.supplier` keeps each supplier's cursor, failures and
`credits_total`.

| Supplier | Asks for |
|---|---|
| `theirstack:countries` | Postings in any country an active member accepts (`platform.supply_scope()`) |
| `theirstack:remote` | Remote postings anywhere, when some member has remote OK |

The suppliers don't overlap. While the remote supplier runs, the countries
supplier asks only for on-site and hybrid postings (`workplace_types_or`).
Postings TheirStack has no workplace type for are then missed.

**Paging.** A run reads the window from the cursor to a second already past
when the window opens (`discovered_at_gte`/`_lte`), one page (`offset`) at a
time. A full page keeps the window and the next page runs on the next recovery
tick. A partial page closes the window, and the next one starts one second
later. So postings that share a discovery second across a page boundary are
neither skipped nor bought twice.

**One run at a time.** A run takes a 10-minute lease (`running_until`). A
duplicate or late job finds the supplier held and does nothing. A run that
dies after TheirStack charged but before it committed is bought again once the
lease expires; TheirStack has no idempotency key.

When no member wants what a supplier covers, it buys nothing and moves its
cursor to now. A provider failure backs off (doubling, at most a day) and keeps
the cursor and window.

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
`posting.country`) when there is no sure label. A sure remote label whose
extraction named no country is remote from anywhere; the source's country
doesn't narrow it.

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
  `platform.supply_scope()`.
- **Also dropped:** plans' `max_search_watchers` and the search settings'
  credit price. The platform pays for searches, so neither applies.

The migration doesn't reverse.

## Removed

- `/v1/job-watches` and the Job alerts page. Alerts arrive as notifications.
- The board poller and `internal/ats`.
- `zapply catalog-backfill`.

## Not done yet

- There's no ATS board supplier.
