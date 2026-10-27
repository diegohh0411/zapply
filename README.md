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
