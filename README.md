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
