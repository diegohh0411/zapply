# Part 3: Events (outbox relay → NATS → SSE)

Status: planned.

Part 2 writes an `outbox` row in the same transaction as every library change. Part 3 delivers those rows to the user's open browser tabs as Server-Sent Events, so the web app (part 9) updates live and later parts (matching progress, renders, mail) can push to the same stream.

## Flow

```
API write tx ──▶ outbox row ──▶ zapply relay ──▶ NATS user.<uid>.<topic> ──▶ API replica holding the stream ──▶ SSE
                  (Postgres)     assigns seq       (core pub/sub)              replays gaps from Postgres
```

- **Why an outbox and a relay:** publishing to NATS from the API after commit is a dual write. A crash between commit and publish loses the event, and publishing before commit announces data that may roll back. The outbox makes the event part of the data change, and the relay moves it out afterwards. `LISTEN/NOTIFY` is ruled out (see `docs/architecture.md`).
- **Why NATS:** the relay doesn't know which API replica holds a user's stream. A replica subscribes to `user.<uid>.>` while it holds a stream for that user, and NATS routes each event only to those replicas.
- **Why core NATS, not JetStream:** Postgres is already the durable log. NATS is only a live router; anything it drops is replayed from the outbox when the client reconnects.

## Ordering: `seq` is assigned at publish time

Outbox ids are UUIDv7s created in Go at insert time, so they don't follow commit order: two transactions can commit in the opposite order of their ids. Resuming with `id > Last-Event-ID` could skip an event that committed late.

The relay assigns `seq bigint` from a sequence when it publishes a row. `seq` is the SSE `id`, and resuming is `seq > Last-Event-ID`.

Options considered:

| Approach | Ordering | Throughput | Cost |
|---|---|---|---|
| **Polling publisher, one serialized relay pass** (chosen) | Global, publish order | Thousands of events/s | None beyond the relay |
| Hash partitions, one relay loop per partition | Per user | Scales with partitions | A lock per partition |
| CDC from the WAL (logical replication, Debezium) | Commit order | Highest | Replication slot (WAL bloat if the consumer stalls), a direct non-pooled connection, failover handling |

Clients only rely on each user's `seq` increasing. A user always maps to one partition, so moving to hash partitions later keeps a shared sequence monotonic per user and doesn't change the client contract. CDC is reserved for when polling itself becomes the bottleneck.

## Schema (migration `00003_events.sql`)

```
outbox   + seq bigint UNIQUE (null until published)
         + published_at timestamptz
         index on (created_at, id) WHERE published_at IS NULL
         index on (user_id, seq) WHERE seq IS NOT NULL
outbox_seq   sequence
```

- New role **`zapply_relay`**: `SELECT, UPDATE, DELETE` on `outbox`, usage on `outbox_seq`, and a policy that applies only to it: `CREATE POLICY relay ON outbox FOR ALL TO zapply_relay USING (true)`. It needs no `BYPASSRLS` and can't read any other table.
- **`zapply_app` gets `SELECT` on `outbox` back**, for replay. The existing tenant policy limits it to the user's own rows, read inside `InTenant`. `UPDATE` and `DELETE` stay revoked.

## Relay (`zapply relay`)

Each iteration is one transaction:

1. `pg_try_advisory_xact_lock(<relay key>)`. If another replica holds it, sleep and retry. A transaction-scoped lock works behind transaction-mode poolers, unlike a session lock.
2. Claim up to `RELAY_BATCH` unpublished rows, ordered by `(created_at, id)`, `FOR UPDATE`.
3. Assign `seq = nextval('outbox_seq')` in that order and set `published_at`.
4. Publish each row to `user.<user_id>.<topic>` and flush the NATS connection.
5. Commit.

- **At-least-once:** publishing happens before the commit. A crash in between leaves the rows unpublished. They are sent again with new `seq` values. Payloads are `{id, version}`, so a duplicate only costs the client a refetch.
- **Wake-up:** poll every `RELAY_INTERVAL` (default 200 ms). A full batch triggers another poll immediately.
- **Pruning:** in the same loop, delete published rows older than `EVENT_RETENTION` (default 24 h), in bounded batches. Part 5's worker can take this over.
- Any number of relay replicas is safe. The lock serializes batches, which keeps the order global.
- `/healthz` and `/readyz` on a small admin listener: ready while both Postgres and NATS are connected.

## SSE gateway (`GET /v1/events`)

Wire format:

```
id: 1842
event: evidence.updated
data: {"id":"0192…","version":4}

: ping
```

The stream starts in this order, so an event can't fall between replay and live:

1. Subscribe to `user.<uid>.>` (one NATS subscription per user per replica, reference-counted across tabs) and buffer.
2. In `InTenant`, replay `seq > Last-Event-ID`, ordered by `seq`.
3. Flush the buffer and stream live, dropping any event with `seq <=` the last one sent.

Rules:

- **Expired cursor:** if `Last-Event-ID` is older than the oldest retained `seq` for the user, send `event: reset` and stream from now. The client then refetches its data.
- **No `Last-Event-ID`:** stream from now. The client has just loaded fresh data.
- **Heartbeat:** a `: ping` comment every 20 s, so proxies keep the connection open.
- **Token expiry:** the stream closes at the JWT's `exp`. The client reconnects with a fresh token and its `Last-Event-ID`.
- **Drain:** when the server starts draining, every stream closes with `retry: 1000`, so clients reconnect to another replica. Without this, open streams would hold graceful shutdown until its timeout.
- **Slow clients:** each connection has a bounded buffer (`SSE_BUFFER`, default 256 events). On overflow the stream closes, and the client replays on reconnect.
- **Limits:** at most `SSE_MAX_STREAMS_PER_USER` (default 8) per replica. Past that, `429`.
- **NATS down:** `/v1/events` returns `503` with `Retry-After`. `/readyz` doesn't depend on NATS, so REST keeps working.
- **Auth:** the usual `Authorization: Bearer` header. The browser's `EventSource` can't send headers, so the Angular client uses a fetch-based SSE client. A short-lived `?ticket=` endpoint can be added if a client ever needs `EventSource`.
- The handler is written by hand on the mux (oapi-codegen's strict server can't stream). `api/openapi.yaml` still documents the endpoint as `text/event-stream` and defines an `Event` schema, so the generated client gets the types.

## Config

| Variable | Used by | Default |
|---|---|---|
| `NATS_URL` | api, relay | `nats://127.0.0.1:4222` |
| `RELAY_DATABASE_URL` | relay (as `zapply_relay`) | required for `relay` |
| `RELAY_INTERVAL` | relay | `200ms` |
| `RELAY_BATCH` | relay | `500` |
| `EVENT_RETENTION` | relay | `24h` |
| `SSE_BUFFER` | api | `256` |
| `SSE_MAX_STREAMS_PER_USER` | api | `8` |

`deploy/compose.yaml` gains a `nats` service, `deploy/postgres/init.sql` creates `zapply_relay`, and the Makefile gains `make relay`.

## Tests

Tests run an embedded `nats-server` in the same process, so CI needs no new service.

- **Relay:**
  - Transactions that commit out of id order are each delivered exactly once, in `seq` order.
  - Two relay replicas never interleave or reuse `seq`.
  - A crash after publishing but before committing redelivers, and loses nothing.
  - Rolled-back writes are never published.
  - Pruning removes only published rows past retention.
  - `zapply_relay` can't read any table other than `outbox`.
- **SSE:**
  - An event published during replay is delivered once.
  - Resuming with `Last-Event-ID` replays exactly the missed events.
  - An expired cursor produces `reset`.
  - User A never receives user B's events, through NATS or through replay.
  - Heartbeats are sent. The stream closes at token expiry and on drain, and drain finishes quickly with streams open.
  - A slow client is disconnected. The per-user stream limit returns `429`. NATS being down returns `503`.

## Out of scope

Hash partitioning of the relay, CDC, JetStream, `?ticket=` auth, and a global (cross-replica) stream limit.
