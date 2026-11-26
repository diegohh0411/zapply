<img src="assets/icon.svg" width="64" height="64" alt="">

# zapply

A hosted service that tailors your CV to each job. It ranks your real experience against a job description, exports an ATS-friendly PDF, and tracks your applications. It is the multi-tenant successor to [cvd](https://github.com/diegohh0411/cvd).

- Architecture and build plan: [`docs/architecture.md`](docs/architecture.md)
- Notes for each part: [`docs/parts/`](docs/parts)

## Development

Prerequisites: Go 1.27, Node 24.15+ (pinned with Volta in `web/package.json`) and Podman (or Docker).

```bash
cp .env.example .env
make db-up            # Postgres 17 on 127.0.0.1:54329, NATS on 127.0.0.1:4222
make migrate          # apply migrations as the schema owner
make run              # API on 127.0.0.1:8080
make relay            # outbox relay → NATS (second terminal)
make worker           # River model worker (third terminal; model config below)
# or, instead of the three above: make run-all   (one process, embedded NATS)
curl -N -H "Authorization: Bearer $(make -s token SUB=alice)" localhost:8080/v1/events   # live changes
curl -H "Authorization: Bearer $(make -s token SUB=alice)" localhost:8080/v1/me
```

### Web app

The Angular + ZardUI app in [`web/`](web) is embedded into the binary and served by `zapply api` (same origin, no CORS). Notes: [`docs/parts/09-web.md`](docs/parts/09-web.md).

```bash
make web        # build web/ into internal/webui/dist; the next `make run` serves it at localhost:8080
make web-dev    # or: Angular dev server with live reload on localhost:4200, proxying /v1 to `make run`
```

In development the login page offers "Development sign-in" (tokens minted with `AUTH_DEV_SECRET`). In production it uses Supabase Auth: set `SUPABASE_URL` and `SUPABASE_ANON_KEY` on the API.

The HTTP API is defined in [`api/openapi.yaml`](api/openapi.yaml). Edit it, or the SQL in `internal/store/queries`, then run `make generate` (oapi-codegen and sqlc, pinned with `go tool`, plus the web client's types via openapi-typescript).

```bash
make test              # unit tests (database tests are skipped)
make test-integration  # full suite against a throwaway Postgres container
```

With Podman, `make db-up` goes through `podman compose`. If it cannot reach the socket, set `DOCKER_HOST` to the Podman machine's socket.

### Running the app on this machine

`compose.yaml` runs the production image and Firecrawl Local against the hosted
database, in place of a dyno. Put the production settings in `.env` (the
Heroku config vars, `PUBLIC_URL=http://localhost:8080` and a `FIRECRAWL_TOKEN`
of your choice), then:

```sh
podman compose up -d --build   # http://localhost:8080
podman compose down
```

### Scraping posting links

Posting links in the new job dialog are read by scrapers that a platform owner
manages in the web app (Administration, Scraping): an ordered list of
providers, where the next one is tried when one fails, and per-site rules that
choose other providers for a host and its subdomains. Provider keys are stored
as encrypted system keys (Administration, Models & keys) and need
`SYSTEM_KEY_RING`.

`SCRAPER_PROVIDER`, `FIRECRAWL_URL` and `FIRECRAWL_API_KEY` (or
`FIRECRAWL_TOKEN`) only seed a deployment that has no saved scraper settings:
the API imports them as the first provider the first time it boots, storing the
key as a system key. After that the environment is ignored, and edits happen in
the admin UI.

#### Crawl4AI as a fallback

[Crawl4AI](https://github.com/unclecode/crawl4ai) renders pages in a stealth
Chromium, which gets past some sites that turn Firecrawl away. It has no hosted
API: run its Docker server (`make crawl4ai` locally on `127.0.0.1:11235`; the
`crawl4ai` service in `compose.yaml` is reachable by the app as
`http://crawl4ai:11235`; both pin `unclecode/crawl4ai:0.9.5`, and Chromium
wants `shm_size: 1g`). Then, as a platform owner:

1. If the server has a token (`CRAWL4AI_API_TOKEN`, set from `CRAWL4AI_TOKEN`
   in the compose files), add it under Administration, Models & keys, with
   provider Crawl4AI. Without a token the server is open to whoever can reach
   it, so keep it off public networks.
2. Under Administration, Scraping, choose Add Crawl4AI provider, enter its
   address, and pick the token. Place it after Firecrawl to use it as the
   fallback, or first to prefer it. A provider can only use a key of its own
   kind.
3. For a site Firecrawl cannot read, add a site rule, for example
   `linkedin.com` with Crawl4AI first and Firecrawl second.

The server's SSRF guard refuses private addresses, and it rejects request
fields such as `magic`, `simulate_user` and `override_navigator` from API
callers, so Zapply relies on `enable_stealth` and a random user agent. LinkedIn
may still block a datacenter IP; reading it reliably needs a residential
address, which edge workers are meant to provide.

#### Edge workers

LinkedIn blocks datacenter addresses, so a deployment on a server cannot read
its postings. An **edge worker** is a small program (`cmd/zapply-edge`, shipped
in a container with its own headless Chromium) that you run on a machine with a
residential connection, such as your laptop. It pulls scrape jobs from the API
over HTTPS, reads each page, and posts it back. It only makes outbound requests:
it opens no port, and NATS and Postgres are not exposed to it. Jobs wait in a
Postgres table, so any API replica can queue a job and any other can serve the
worker.

How a page is read: for a LinkedIn posting the worker first asks LinkedIn's
logged-out endpoints with ordinary browser headers (the guest posting fragment
and the public page), with no browser at all. If LinkedIn answers with a
login wall, a throttle (HTTP 429 or 999) or a redirect to its authwall, or for
any other site, it renders the page in headless Chrome, in a new browser
context per job (no cookies or storage carry over) and never signs in. A pause
of 2 to 6 seconds separates jobs.

Setup, as a platform owner and then on the machine (this is how it is run on a
Mac with Podman; Docker works the same):

1. Administration, Scraping, Edge workers: create a worker. Give it the sites
   it may open (for example `linkedin.com`; a host also covers its subdomains,
   and an empty list allows nothing) and its limits. The token is shown once;
   only its hash is stored. Rotate it to replace a lost or leaked one.
2. Under Providers, add an Edge workers provider, and choose where it sits in
   the order, or add a site rule that puts it first for `linkedin.com`. If no
   worker is online for a site the provider fails at once and the next one is
   tried; a job that no worker takes within 45 seconds fails the same way.
3. On the machine:

   ```sh
   cd deploy/edge
   cp .env.example .env     # ZAPPLY_URL and ZAPPLY_EDGE_TOKEN
   podman compose up -d
   podman logs -f zapply-edge-edge-1
   ```

   The image pins Go 1.27 and Debian 12.12 and takes Chromium from Debian;
   rebuild with `podman compose build --pull` now and then for its patches.
   `restart: unless-stopped` brings it back after the Podman machine restarts.
   To bring the Podman machine and the worker up at login on a Mac, load
   `deploy/edge/com.zapply.edge.plist` with `launchctl bootstrap` (edit its paths
   first); it is optional.

Safety limits. The worker runs other users' links on your home network, so:

- The owner decides which hosts a worker may open (`allowed_hosts`, or `*` alone
  for any host), its jobs
  per hour and its jobs at once. The database enforces them when a job is
  claimed, and only the worker that claimed a job can answer it.
- The worker refuses anything but `http`/`https` on ports 80 and 443, and any
  address that is not public: loopback, private (RFC 1918), link-local
  (including cloud metadata at `169.254.169.254`), carrier-grade NAT, and so
  on. Names are resolved and every answer is checked; plain HTTP connections
  re-check the address that is really dialled, and every request the browser
  makes (redirects and sub-resources too) is checked before it leaves.
- The container runs as a non-root user with a read-only root filesystem,
  every capability dropped but the one Chromium's sandbox needs, no extra
  privileges, memory, CPU and process limits, and tmpfs for the browser
  profile. It is not signed in to anything.
- Results are kept only until the API reads them (once), and never more than an
  hour; finished job rows, with the posting URL, are deleted after 25 hours.
  Worker administration is audited.

A residual risk of the browser path: the browser does its own DNS lookups, so a
hostile name that changes its answer between the check and the connection
(DNS rebinding) could reach a private address; the plain-HTTP path does not
have this gap. Run the worker on a network you are comfortable with that risk
on, or put it on a guest VLAN.

LinkedIn's terms of use prohibit automated scraping and the worker's traffic
comes from your own address. It reads only the pages anyone can open without
signing in, never logs in, and is meant for the handful of postings a person
pastes into the new job dialog. Keep the volume low (the default limit is 20 jobs
an hour); whether that is acceptable for you and your users is your call, and
LinkedIn may still block or throttle the address.

Gmail push needs a public URL, so leave the `GOOGLE_PUBSUB_*` variables out and
set `MAIL_RECONCILE_INTERVAL` (e.g. `5m`) for mail to sync while it runs. Google
sign-in for mail needs `http://localhost:8080/oauth/google/callback` on the
OAuth client, and Supabase sign-in needs `http://localhost:8080` among the
project's redirect URLs. Migrations: `podman compose run --rm app migrate`.

### Local Supabase (development only)

A copy of the hosted project on this machine: Postgres 17, Auth, Studio and
Mailpit, run by the Supabase CLI (through `npx`; Podman works, the Makefile
points the CLI at its socket). Production keeps using hosted Supabase.

```sh
make supa-up        # first run creates supabase/signing_keys.json (ES256)
make supa-pull      # dumps the hosted project from MIGRATE_DATABASE_URL in .env
make supa-restore   # loads supabase/backup into the local database
cp .env.supabase.example .env.supabase   # add the printed publishable key
make web && make run-all ENV_FILE=.env.supabase   # http://localhost:8080
```

Sign in with the email link; it arrives in Mailpit at http://127.0.0.1:54324.
Your account and its ID come over in `auth.users`, so your data stays yours.
`supabase/backup` holds personal data and is never committed. Google sign-in
isn't configured locally. `make supa-down` stops the stack and keeps its data.

## Database roles

- `zapply migrate` connects as the schema owner (`MIGRATE_DATABASE_URL`).
- The API connects as `zapply_app` (`DATABASE_URL`). That role must not be superuser or `BYPASSRLS`, and the API refuses to start if it is.
- `zapply relay` connects as `zapply_relay` (`RELAY_DATABASE_URL`). It can read and update the outbox and nothing else, and has the same superuser and `BYPASSRLS` check.
- `zapply worker` connects as `zapply_worker` (`WORKER_DATABASE_URL`), using a direct or session-pooled connection for River. It has explicit queue/tenant permissions and the same superuser and `BYPASSRLS` check.
- Private platform functions run under `zapply_platform`, a non-login role. Owner API actions are checked in the database; owner status grants no general access to member evidence or results.
- Every tenant query runs inside `store.InTenant`, which sets `app.user_id` for that transaction only. Forced row-level security enforces the isolation.

## Model access and memberships

Part 5 adds owner-managed plans with rolling **past 7 days** and **past 30 days**
USD allowances, encrypted system OpenRouter keys, a usage ledger, and the worker.
There is no BYOK, paid billing, or application-wide spending ceiling in the MVP.
Library and jobs remain usable without model configuration or a membership.

After signing in, use the app UUID from `GET /v1/me` to provision the owner:

```bash
set -a; . ./.env; set +a
bin/zapply owner grant --user-id USER_UUID
```

Configure `SYSTEM_KEY_RING` and `SYSTEM_KEY_ACTIVE_VERSION` on API and worker
processes, then use Administration to create plans, assign memberships, and
save/activate an OpenRouter key. Configure `LLM_MODEL` and `LLM_ENDPOINT` on the
worker and start `make worker`. The owner also needs a membership to run the
fixed-prompt model diagnostic. Key inputs never return in API responses.

All spending lands in one ledger: each billable provider call is a
`platform.charge` that holds its most expensive outcome against the plan until it
settles into append-only `platform.ledger_entry` rows. What the call was is kept
as evidence by kind: `model_charge` (model calls), `scrape_charge` (posting pages
read through a priced scraper; set a provider's **price per credit** under
Scraping, or leave it empty for a free one) and `search_charge` (search
watchers, below).

**Search watchers** run a member's saved job search (titles, places, country,
remote) across every company through TheirStack, at the interval they choose,
and file new postings as job alerts. Add a TheirStack system key, then set it and
its price per API credit under Scraping → Job search. Each plan sets how many
search watchers its members may keep (none by default, and none without an
active membership); each poll is paid from the member's allowances at the
credits TheirStack charged, one per posting returned. A posting alerts once per
member however many watches find it.

Setup, accounting rules, conservative reservations, and recovery commands are in
[`docs/parts/05-worker.md`](docs/parts/05-worker.md). Optional tracing uses standard
`OTEL_EXPORTER_OTLP_*` configuration and exports only when an endpoint is set.

## Mail

Part 10 connects a Gmail account, finds the mail about your applications (received,
assessment, interview, rejection, offer) and links it to the matching job. It uses
`gmail.readonly` through Google OAuth, Pub/Sub push notifications and a periodic
reconcile scan. Mail not about an application is discarded after classification;
only its identifier is kept. Setup, environment variables and design:
[`docs/parts/10-mail-plan.md`](docs/parts/10-mail-plan.md). Set the Google variables in
`.env.example` to enable it; without them the Mail page says it is not configured.

## Matching

Part 6 adds automatic matching when jobs are created, reusable decision-answer
caching, deterministic coverage and CV selections, corrections, run history and
staleness. Open a saved job's **Match & CV** tab; automatic matching can be turned off
in Profile. Apply migrations through 00012 before deploying the updated API and worker.

Choose the decision model under **Administration → System keys**. It uses the
same active OpenRouter key; no decision-model environment variable is required.
Runs can queue without a model or membership and wait for configuration or
allowance. Live dispatch is gated on documented model billing/input bounds;
the initial production adapter registry is empty pending verification. Fake
providers cover the complete pipeline without spending credits. Details and
verification prerequisites: [Part 6](docs/parts/06-matching-plan.md).

Shared gateway code is under `internal/model`. Migration 00012 preserves existing
accounting data while renaming shared gateway objects to `model_*`; administration
uses `/v1/admin/model-attempts` and `model_operation.changed` events. Text diagnostic
configuration retains the LLM-specific `LLM_MODEL`/`LLM_ENDPOINT` names.

## CV rendering

Open a job's **Match & CV** tab to preview and export a PDF from your library. Matching
ranks the content when available; all eligible library experience is offered
to the page fitter, including less relevant entries. Check items to always keep
them, leave items out, or use automatic selection. Choose English or Spanish
and a 1–3 page budget, then export. The fitter restores smaller entries and
highlights into spare space without exceeding the budget. The browser compiles
with Typst WASM; Postgres stores immutable content snapshots, and previous
exports download identically after library edits.
Apply migration 00016 before deploying.

The image includes Typst 0.14.2 for `GET /v1/cv-exports/{id}/pdf`, which streams
a PDF without storing it. For local server rendering, put that CLI on PATH or
set `TYPST_DIR` to a directory containing `0.14.2/typst`. Browser rendering
needs no local CLI. The native determinism tests also need `pdftotext`
(poppler); run them with `ZAPPLY_REQUIRE_TYPST=1 make test-integration`.
Details: [Part 7](docs/parts/07-rendering.md).

Heroku deployments can show their source commit in the sidebar by enabling both
`runtime-dyno-metadata` and `runtime-dyno-build-metadata` labs features on the app.
The build time is embedded in the image and shown in the browser’s local time.

## License

Copyright (C) 2026 Diego Hernández Herrera

zapply is free software: you can redistribute it and/or modify it under the
terms of the [GNU Affero General Public License v3.0](LICENSE) as published by
the Free Software Foundation. If you run a modified version as a network
service, you must offer its users the corresponding source code.

The bundled Source Sans 3 fonts in `templates/fonts/` are licensed separately
under the SIL Open Font License 1.1 (see their `LICENSE.md`).
