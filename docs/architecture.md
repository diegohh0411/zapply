# zapply architecture

zapply is the hosted, multi-tenant version of the cvd idea. It keeps one library of a person's real experience, ranks it against a job description, and exports a truthful, ATS-friendly CV. It also tracks applications and reads mail for status updates.

The concept is the same as cvd. The implementation is redesigned for a service with many users. This document records the decisions. Each part of the build has its own notes in `docs/parts/`.

## Principles

1. **Stateless request path.** API processes keep no per-user state in memory, so any replica can serve any request. Anything slow or long-lived runs in a worker.
2. **One binary, many roles.** `zapply api`, `zapply worker`, `zapply relay` and `zapply migrate` ship in the same image. Each role scales on its own.
3. **Postgres is the source of truth**, and it is also the first job queue and the event outbox. A new piece of infrastructure is added only when Postgres stops being enough, and NATS (part 3) is the only one planned.
4. **Tenant isolation is enforced twice:** queries filter on `user_id` explicitly, and forced RLS rejects anything they miss.
5. **Shared facts are shared.** Data that is not personal, such as company profiles and model catalogs, is stored once for everyone instead of once per user.
6. **Contract first.** The HTTP API is described in OpenAPI 3.0.3, the version oapi-codegen fully supports. The Go server interfaces and the Angular client are generated from it.

## Topology

```
                ┌──────────────┐
 Angular SPA ──▶│  CDN / edge  │
                └──────┬───────┘
                       │ HTTPS (REST + SSE)
              ┌────────▼────────┐      ┌───────────────┐
              │ zapply api  ×N  │◀────▶│  NATS (part 3)│◀──┐
              └───┬─────────┬───┘      └───────────────┘   │
                  │ tx      │ enqueue                      │ publish
              ┌───▼─────────▼───┐      ┌───────────────┐   │
              │   Postgres      │◀────▶│ zapply relay  │───┘
              │ data · outbox · │      └───────────────┘
              │ River jobs      │      ┌───────────────┐
              │                 │◀────▶│ zapply worker │──▶ LLM providers, Exa,
              └─────────────────┘      │      ×M       │    Typst, object storage
                                       └───────────────┘
```

## What changes from cvd, and why

| cvd (desktop) | zapply (service) | Reason |
|---|---|---|
| Tauri `invoke` mirrored as `POST /api/rpc/:command` | Resource-oriented REST described in OpenAPI 3.1, with cursor pagination and `Idempotency-Key` on POSTs | Cacheable, versionable and generated on both ends; retries are safe |
| Change feed from statement-level triggers + `pg_notify` on one `LISTEN` connection | Transactional **outbox** table → `relay` → NATS subject `user.<id>` → SSE on whichever API replica holds the stream | `LISTEN` breaks behind poolers, the 8 KB payload limit applies, and one node becomes a bottleneck. The outbox gives at-least-once delivery with ordering per user |
| Schema coupled to Supabase `auth.users` | Own `app_user` table (UUIDv7) plus an `identity(issuer, subject)` mapping, resolved on first request by a `SECURITY DEFINER` function; JWTs verified against JWKS | The auth provider can be swapped; tests need no Supabase stub |
| Serial integer IDs plus composite tenant FKs | UUIDv7 primary keys; `user_id` on every tenant row; composite FKs `(user_id, id)` | IDs can be created anywhere and stay time-ordered for indexes, with no cross-tenant FK leaks |
| Match runs in-process in Tauri | Durable **River** jobs (one per pipeline stage) with retries, cancellation and resume | Survives deploys and crashes; workers scale with load |
| Scores cached per run | Scores cached by **content hash**: `(entry_version_hash, jd_hash, model, prompt_version)` | Re-matching, or matching a near-duplicate JD, costs no new calls |
| Company profiles stored per user | A **global** `company` table, with user-only fields kept separate. Research is deduplicated by a unique job per domain | One research run serves every user who applies to that company |
| Secrets in a `secrets` table | Envelope encryption: a data key per user, wrapped by a KMS (or a local master key in dev) | Database dumps do not leak provider keys or IMAP passwords |
| Local llama.cpp on loopback | An **LLM gateway** module: provider registry, per-user budgets and rate limits, platform keys or BYOK, usage metering | Hosted inference is shared and costs money |
| Typst compiled in-process in a greedy loop | A render worker with the Typst CLI in the image; PDFs stored in S3-compatible storage behind signed URLs | Isolates CPU-heavy work; PDFs leave the database |
| IMAP IDLE task per desktop app | A **mail watcher** fleet: per-account leases in Postgres, sharded, with reconnect and backoff (part 10) | Long-lived connections need owners, not stateless workers |
| `cvd-mcp` over stdio | A remote MCP server (streamable HTTP + OAuth) on the API (part 11) | Works for hosted users |
| Auto-apply via a Tauri browser window | A browser extension that calls the API (later) | A server cannot drive the user's browser |

## Data and tenancy

- Every tenant table has `user_id uuid not null` and `FORCE ROW LEVEL SECURITY` with the policy `user_id = current_setting('app.user_id')::uuid`.
- The API connects as `zapply_app`, a role that owns nothing and has neither `BYPASSRLS` nor superuser. Migrations run as the owner role in a separate step (`zapply migrate`).
- `store.InTenant(ctx, userID, fn)` opens a transaction, runs `set_config('app.user_id', $1, true)` and calls `fn`. The setting is transaction-local, so it works with transaction-mode poolers (PgBouncer, Supavisor).
- Global tables (`company`, model catalog) have no RLS. Only workers write to them.

## Async work

- **River** (a job queue in Postgres) is used for matching stages, renders, company research and mail scans. Jobs are enqueued in the same transaction as the row that triggers them, so nothing is lost and nothing is enqueued for a rolled-back write.
- Per-user fairness: queue concurrency limits plus a per-user in-flight cap enforced by the LLM gateway.

## Events

- Domain writes insert an `outbox` row `(id, user_id, topic, payload, created_at)` in the same transaction.
- `zapply relay` claims rows with `FOR UPDATE SKIP LOCKED`, publishes them to NATS `user.<user_id>.<topic>`, and deletes or marks them.
- API replicas subscribe on demand for each connected SSE stream. Clients reconnect with `Last-Event-ID`, so missed events are replayed from the outbox's retention window.

## Observability and operations

- `log/slog` JSON logs carrying a request ID and user ID, and OpenTelemetry traces across api → job → provider.
- `/healthz` reports liveness. `/readyz` checks dependencies and fails during drain. SIGTERM flips readiness, waits for the drain period, then shuts down gracefully.
- Configuration comes only from the environment (12-factor).

## Build plan

| Part | Scope |
|---|---|
| 1 | Foundation: module, config, logging, HTTP server with stdlib `ServeMux`, health and drain, Postgres pool, embedded migrations, roles + RLS, JWT auth, tenant transaction, `GET /v1/me`, compose, CI |
| 2 | Library: evidence model (education, paid and unpaid × engagement, credential, output, recognition), organizations, highlights, skills. OpenAPI + generated server, sqlc, outbox writes. See `docs/parts/02-library.md` |
| 3 | Events: outbox relay → NATS → SSE gateway. See `docs/parts/03-events.md` |
| 4 | Jobs and application tracking: postings, statuses, views |
| 5 | Worker runtime + LLM gateway: River, provider registry, encrypted secrets, budgets |
| 6 | Matching pipeline: staged jobs, score cache keyed by content hash, explainability |
| 7 | Rendering: Typst templates, page fitting, object storage, exports |
| 8 | Company research: global cache, one research run per company, agent budget |
| 9 | Angular + ZardUI web app, using the generated client (can start in parallel after part 2) |
| 10 | Mail watcher fleet and inbox |
| 11 | Remote MCP, browser-extension autofill |
