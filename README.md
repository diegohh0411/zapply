<img src="assets/icon.svg" width="64" height="64" alt="">

# zapply

A hosted service that tailors your CV to each job. It ranks your real experience against a job description, exports an ATS-friendly PDF, and tracks your applications. It is the multi-tenant successor to [cvd](https://github.com/diegohh0411/cvd).

- Architecture and build plan: [`docs/architecture.md`](docs/architecture.md)
- Notes for each part: [`docs/parts/`](docs/parts)

## Development

Prerequisites: Go 1.27 and Podman (or Docker).

```bash
cp .env.example .env
make db-up            # Postgres 17 on 127.0.0.1:54329, NATS on 127.0.0.1:4222
make migrate          # apply migrations as the schema owner
make run              # API on 127.0.0.1:8080
make relay            # outbox relay → NATS (second terminal)
curl -N -H "Authorization: Bearer $(make -s token SUB=alice)" localhost:8080/v1/events   # live changes
curl -H "Authorization: Bearer $(make -s token SUB=alice)" localhost:8080/v1/me
```

The HTTP API is defined in [`api/openapi.yaml`](api/openapi.yaml). Edit it, or the SQL in `internal/store/queries`, then run `make generate` (oapi-codegen and sqlc, pinned with `go tool`).

```bash
make test              # unit tests (database tests are skipped)
make test-integration  # full suite against a throwaway Postgres container
```

With Podman, `make db-up` goes through `podman compose`. If it cannot reach the socket, set `DOCKER_HOST` to the Podman machine's socket.

## Database roles

- `zapply migrate` connects as the schema owner (`MIGRATE_DATABASE_URL`).
- The API connects as `zapply_app` (`DATABASE_URL`). That role must not be superuser or `BYPASSRLS`, and the API refuses to start if it is.
- `zapply relay` connects as `zapply_relay` (`RELAY_DATABASE_URL`). It can read and update the outbox and nothing else, and has the same superuser and `BYPASSRLS` check.
- Every tenant query runs inside `store.InTenant`, which sets `app.user_id` for that transaction only. Forced row-level security enforces the isolation.
