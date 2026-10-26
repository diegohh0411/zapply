# Part 1: Foundation

## Delivered

- **Single binary** `cmd/zapply` with the roles `api`, `migrate` and `devtoken`. It ships as a 15 MB distroless image whose default command is `api`.
- **Config** comes from environment variables only (`internal/config`). Startup fails on unsafe settings: `AUTH_DEV_SECRET` in production, a missing issuer, or a missing database URL.
- **HTTP** (`internal/httpx`) uses `net/http` and the stdlib `ServeMux` with method patterns, with no framework. It provides:
  - request IDs, reusing a well-formed `X-Request-ID`
  - a scoped `slog` logger
  - panic recovery
  - RFC 9457 `problem+json` errors
  - a server that drains on SIGTERM: readiness flips, it waits `DRAIN_DELAY`, then shuts down gracefully
- **Auth** (`internal/auth`) verifies JWTs (RS256, ES256 or EdDSA via JWKS, refreshed in the background) and checks `iss`, `aud` and `exp`. A development HS256 verifier is available outside production only. The middleware resolves the principal to a user.
- **Store** (`internal/store`):
  - `pgxpool` plus goose migrations, embedded and applied by `zapply migrate` with the owner role.
  - `Open` refuses roles that are superuser or have `BYPASSRLS`.
  - `InTenant` sets `app.user_id` with transaction scope, so it works with transaction-mode poolers.
  - `ResolveUser` maps `(issuer, subject)` → UUIDv7 through `resolve_identity()`. It is race-safe and cached in an in-process LRU.
- **Schema:** `app_user` (forced RLS) and `identity` (unreadable by the runtime role).
- **Endpoints:**
  - `GET /healthz`: liveness
  - `GET /readyz`: database ping; fails while draining
  - `GET /v1/me`
- **Tooling:**
  - `deploy/compose.yaml`: Postgres 17 on port 54329
  - `Makefile`
  - `scripts/test-integration.sh`: a throwaway container
  - CI: format, vet, race tests against Postgres, image build

## Tests

- **Unit:** JWT validation (expiry, issuer, audience, wrong secret, `alg: none`, HS256 against JWKS), auth middleware, request ID, panic recovery, graceful drain with a request in flight.
- **Integration** (real Postgres):
  - migrations are idempotent
  - unsafe roles are rejected
  - identity resolution is stable and race-safe (16 concurrent first requests produce one user)
  - RLS hides other tenants and blocks cross-tenant writes
  - nothing is visible without a tenant, and the tenant setting is transaction-local
  - `identity` is not readable
  - end-to-end `/v1/me`

## Deferred on purpose

- OpenAPI codegen arrives in part 2, when there are real resources to describe.
- NATS arrives in part 3, together with the outbox relay that uses it.
- OpenTelemetry tracing arrives with the worker (part 5), where traces start crossing processes.
- The access log line carries `request_id` but not `user_id`, because auth runs inside it. The handler logs do carry `user_id`.
