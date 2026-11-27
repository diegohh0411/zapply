# Part 14: ATS board supplier

Status: built. A third supplier for job alerts (part 13). It reads public
Greenhouse, Lever and Ashby job boards. That's free and fast: a posting shows
up on its board the moment it goes live, before aggregators notice. It also
keeps alerts coming if TheirStack credits run out. Board postings go through
the same catalog → classify → route pipeline. Members don't set anything up:
the owner chooses which boards are read, and routing by preferences is
unchanged.

## Reading boards (`internal/ats`)

`ats.Client.Fetch(system, board)` reads a board's public JSON API with no
credentials. Responses are capped at 32 MB.

| ATS | Endpoint | Description |
|---|---|---|
| Greenhouse | `boards-api.greenhouse.io/v1/boards/<b>/jobs?content=true` (+ `/v1/boards/<b>` for the name) | HTML, turned into text |
| Lever | `api.lever.co/v0/postings/<b>?mode=json` | `descriptionPlain` + `additionalPlain` |
| Ashby | `api.ashbyhq.com/posting-api/job-board/<b>` (unlisted jobs skipped) | `descriptionPlain` |

`ats.Parse` recognises board links and posting links, including embedded and
EU Greenhouse boards.

## Boards (`platform.ats_board`, migration 00049)

Each board has a status:

- **pending:** discovered and waiting for the owner.
- **active:** read on schedule.
- **disabled:** rejected, turned off, or retired automatically. The reason is
  in `last_error`.

Slugs match without case.

- **Seeds:** the owner pastes links in Admin → Job sources. They're active at
  once.
- **Discovery:** `alerts.Ingest` parses every TheirStack sighting's URL. A
  board it names is added as `pending`, and `sightings` counts how many search
  postings pointed at it. A board already known only counts the sighting, so a
  rejected board is never offered again.
- **Cap:** at most 2,000 active boards (`platform.assert_board_room`).

## Polling (`alerts.Poll`, River job `ats_board_poll`)

- **Scheduling:** the recovery tick queues up to 200 due active boards. It
  moves each board's `next_poll_at` forward by the board interval (+0–10%
  jitter) in the same transaction. At most 4 reads run at once per ATS.
- **What gets queued:** a read looks up which of the board's
  `ats:<ats>:<board>:<posting>` identities the catalog already holds. It queues
  only the rest, as `posting_ingest` jobs of up to 50 sightings, in the
  transaction that records the read.
- **Baseline:** a board's first good read, or one more than 72 h
  (`alerts.BaselineGap`) after the last good one, is a baseline. Those
  sightings are `Quiet`: `catalog.Add` creates them with `routed_at` already
  set, so they never alert. A posting the catalog already had keeps its own
  state.
- **Failures:** a failed read doubles the wait, up to a day. A board not found
  for a week is disabled.
- **Retiring discovered boards:** a discovered board with no new postings for
  60 days (counted from approval) is disabled.

## Intervals

`platform.supplier.interval_minutes` replaces `alerts.SupplyInterval`. The
allowed values are 5, 10, 15, 30 and 60 minutes, 6 h and 24 h. The defaults
are 30 for the TheirStack suppliers and 15 for `ats:boards`, which applies to
every board. Saving a shorter interval moves runs that are already scheduled
earlier.

## Admin (Admin → Job sources)

- **Suppliers table:** interval pickers, last good run, credits spent and
  health.
- **Boards:** Pending, Active and Disabled lists.
  - Pending boards can be approved or rejected.
  - Active boards can be polled now or disabled.
  - Disabled boards can be enabled again.
- **Add boards:** paste links; ones that name no board are returned.

Endpoints (owner only):

- `GET /v1/admin/job-sources`
- `PUT /v1/admin/job-sources/interval`
- `GET|POST /v1/admin/ats-boards`
- `PUT /v1/admin/ats-boards/{ats}/{board}/status`
- `POST /v1/admin/ats-boards/{ats}/{board}/poll`

## Not done

- Postings that close on a board aren't marked closed in the catalog.
- Discovery covers only new TheirStack sightings. Postings already in the
  catalog aren't scanned for boards.
