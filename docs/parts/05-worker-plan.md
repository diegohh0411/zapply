# Part 5 implementation plan

Status: implementation proposal based on the [agreed MVP scope](05-worker.md).
This is a build plan, not a record of delivered functionality. Defaults below
resolve implementation choices and can change without expanding the MVP scope.

## Outcome and boundaries

Deliver `zapply worker`, an OpenRouter gateway, owner-managed plans and system
keys, and member usage screens. Prove the complete path with a small, fixed-prompt
owner diagnostic task. The owner must have an active membership to run that task;
owner status never bypasses metering. Matching remains in part 6.

No BYOK, subscription billing, global spending ceiling, arbitrary chat endpoint,
or generic workflow engine. Start with non-streaming text requests and a small
server-configured set of explicit model IDs. Do not add model selection to plans
or build a model management UI in this part.

## Existing integration points

- `cmd/zapply/main.go` selects API, relay, and migration roles; add the worker
  and owner provisioning commands here.
- `internal/store/store.go` owns pgx pools, `InTenant`, and Goose migrations.
  Migration 6 is already registered in Go for URL backfilling: start new
  application migrations at **7**, checking the sequence again before coding.
- `internal/auth/auth.go` resolves verified identities to stable `app_user` IDs.
  Authentication remains unchanged; platform owner authorization is new.
- `api/openapi.yaml` generates the strict Go server and TypeScript types.
  Reuse problem responses, ETags, `If-Match`, and creation idempotency conventions.
- The Angular app already has generated-client access in `core/api.ts`, identity
  handling in `core/auth.service.ts`, and SSE invalidation in `core/events.service.ts`.
- Reuse the outbox/relay for tenant notifications; do not introduce a second
  stream for administrative data.

Add `internal/membership` for owner/member services, `internal/model` for accounting
and gateway contracts, `internal/model/openrouter` for the adapter, `internal/secrets`
for encryption, and `internal/worker` for River wiring. Keep SQL and generated
code in the existing store layout; add API handlers in `internal/api` and Angular
features in `web/src/app/admin` and `web/src/app/usage`.

## 1. Authorization and database boundaries

Store platform owners in a protected `platform_owner(user_id)` relation.
Provision the first owner with `zapply owner grant --user-id <existing-id>`, using
`MIGRATE_DATABASE_URL`; provide a matching revoke command. It only grants platform
permissions, not SQL privileges or membership. No first-signup promotion, email
heuristics, or caller-editable JWT claim grants ownership. Keep owner grants out
of the browser MVP; log provisioning changes without credentials.

`GET /v1/me` returns `platform_role: member | owner`. Check ownership from the
database on every administrative request, without using the identity LRU as a
permission cache. The owner UI guard is a convenience; backend checks are required.

Add protected platform tables in a `platform` schema, with no privileges for
`PUBLIC`, Supabase browser roles, or direct `zapply_app` table access. Use narrow
`SECURITY DEFINER` functions for owner administration and member self-service
reads. Functions derive the actor from transaction-local `app_user_id()`, check
ownership where needed, set a safe search path, fully qualify objects, and revoke
default `PUBLIC` execution. Do not expose an arbitrary SQL or tenant-switching
function. The API calls them inside the existing authenticated tenant transaction.

Give `zapply_worker` its own restricted connection and no superuser/BYPASSRLS
privileges. Grant worker-only platform functions for membership lookup,
reservation, settlement, and credential loading. Tenant task/result tables use
forced RLS and explicit policies for `zapply_app` and `zapply_worker`; both use a
transaction-local tenant setting. Grant only the tenant domain access needed by
the diagnostic now; later parts extend it. The worker receives tenant IDs from
durable queue arguments, never directly from an unauthenticated client.
Explicitly grant `app_user_id()` execution to the worker and scope applicable
RLS policies to the roles that can execute it.

Revoke application default table grants on new protected objects explicitly;
the current migration history grants new public tables to `zapply_app` by default.
Also test explicit/default grants for hosted browser roles. Owner access to member
spending does not grant access to their evidence, prompts, or results.

## 2. Data and money

Use UUIDv7 IDs and timestamps, composite tenant foreign keys, and resource versions
where applicable. Proposed relations:

- `platform.plan`: name, `cap_7d`, `cap_30d`, version, created/updated timestamps.
  No plan deletion while assigned; omitting deletion entirely is sufficient here.
- `platform.budget_account`: one stable row per member, including users with no
  assigned plan. It supplies the lock for accounting and membership mutations.
  Backfill existing users and initialize it idempotently during identity creation.
- `platform.membership`: member ID, plan ID, active/suspended status, effective
  timestamp, version. No membership means no new model work; ordinary app use
  continues. The owner explicitly assigns plans; there is no automatic free tier.
- `platform.membership_history` and `platform.admin_audit`: preserve assignments,
  status changes, plan edits, key changes, and manual accounting resolutions with
  actor/time. Store metadata, never keys or request/response bodies.
- `platform.system_key_version`: OpenRouter label, encrypted secret, encryption
  key version, fingerprint, and timestamps. A protected provider configuration
  selects one current version; retain older versions for pending reconciliation.
- Tenant `model_operation`: kind, status, request/idempotency hash, next eligibility
  time/reason, bounded result, timestamps, and version. Queue payloads contain
  only tenant/operation IDs and trace context, not prompts or secrets.
- `platform.model_attempt`: tenant/operation IDs, attempt number, request hash,
  model, credential version, pricing snapshot, reservation amount, dispatch time,
  generation ID, lifecycle state, and recorded provider error class. Unique
  `(user_id, operation_id, attempt_number)` and a partial unique constraint prevent
  multiple unresolved attempts for one operation. Enforce uniqueness of known
  `(provider, generation_id)` across attempts, including manual reconciliation.
- `platform.model_usage`: append-only charge/reconciliation entries linked to an
  attempt, with provider-reported usage, amount, accounting time, source, and
  deduplication key. Never rewrite history to reset a plan. A correction is a
  linked delta; repeating the same settlement cannot add the charge twice.

Operation states are `pending`, `blocked`, `running`, `completed`, `failed`,
`needs_review`, and `cancelled`. Attempt states are `prepared` (no hold/no send),
`dispatching` (hold acquired), `uncertain`, `settled`, and `uncharged`. Enforce
transitions conditionally; a duplicate worker or late response cannot overwrite
a terminal decision. Late billing evidence follows the audited correction path.
Scope operation idempotency to `(user_id, kind, key)` and
compare a canonical request hash, including the diagnostic template version.

Store USD as signed 64-bit integer **picodollars** (one trillion units per USD),
with checked bounds and decimal-string API fields. Parse provider decimals
without float64, rounding a positive charge/reservation upward to the nearest
unit. Plan caps must be positive and fit this representation. Keep provider
decimal values for audit. Do not use the existing nullable numeric-to-float sqlc
override for money. Buying OpenRouter credits, taxes, and funding fees do not
consume member allowances; provider-reported model request charges do.

Use dispatch time for both rolling windows, including late settlements and
corrections. Capture one database time `T` after acquiring locks. A settled charge
is included when `T - window < dispatched_at <= T`; windows are exactly 168 and
720 hours, independent of timezone and daylight saving time. Index usage by
`(user_id, accounted_at)` and unresolved attempts by user. Retain ledger/audit
history; the initial implementation does not prune financial records.

## 3. Reservation and settlement transactions

Calculate a conservative quote before opening the transaction. Then:

1. Lock the member's `budget_account` row `FOR UPDATE`.
2. Read membership and lock its plan `FOR SHARE`. Reject missing/suspended
   memberships; obtain `T` after the locks. Membership changes use the same
   account lock. Plan edits lock the plan row and never acquire member account
   locks, preventing the reverse lock order.
3. Sum settled usage in each window and **all unresolved reservations**, regardless
   of reservation age. Check the proposed reservation against both caps and the
   per-user in-flight limit (default one dispatched/uncertain call per member).
4. Claim a prepared attempt with a conditional state update; set its immutable
   dispatch time, pricing/credential snapshots, reservation, and `dispatching`
   state. Write a usage-change outbox event and commit before any provider call.

Never hold a database transaction during HTTP. Only the worker that committed
the dispatch transition may send that attempt. A replay seeing `dispatching` or
`uncertain` reconciles instead of resending. Do not release its hold because a
lease expired. A crash immediately before the send can therefore require manual
resolution; the system cannot prove whether an external side effect happened.

Settlement locks account then attempt, inserts deduplicated usage entries,
replaces the hold with the actual charge, saves any bounded result, updates the
operation, and writes outbox events in one transaction. A later worker retry
returns the persisted result/status. If saving a provider result fails, retry
the database write while it is available; recovery must not repeat the paid call.
Cancellation releases only work proven not dispatched; cancelling an HTTP request
does not prove that the provider performed no billable work.

Plan edits apply to all assigned members immediately. Assignment, suspension,
and plan downgrades preserve usage and pending holds. Calls authorized before a
change may finish; subsequent dispatch transactions use the new limits. If
recorded cost ever exceeds the quote, record the full real charge, stop further
member dispatches when over cap, and report the quote failure. Never clamp the
ledger to the allowance or promise a provider billing guarantee.

## 4. OpenRouter adapter and uncertain outcomes

Define a small internal provider interface for quoting, generating once, and
looking up an existing generation; include a deterministic fake. Use a bounded
Go HTTP client with timeouts and no automatic POST retries. Keep the base URL
fixed to OpenRouter in production; tests inject a local transport/server.

Start with explicit, text-only model/endpoint configurations and a finite output
token limit. Disable plugins, server tools, multimodal inputs, model fallback,
and provider fallback. Bound input using the selected model's tokenizer plus
tested message overhead, or use the full context allowance conservatively until
that tokenizer is verified. Include reasoning tokens, request fees, and any
cache-write rate in the upper bound. Do not admit a model with unbounded or
unknown charges. Refresh price/capability metadata on a bounded schedule; reject
new dispatches when its configured freshness limit expires. Store the quote's
rates/version on the attempt and use `provider.only`, `allow_fallbacks: false`,
`require_parameters: true`, and `max_price` constraints to keep routing within
the quote. Check price units: catalog rates and routing ceilings use different
units. These constraints are supported by [OpenRouter provider routing](https://openrouter.ai/docs/guides/routing/provider-selection).

Settle from `usage.cost` when present; do not substitute the upstream provider's
inference cost. Persist the generation ID and token details. OpenRouter documents
the billed cost in [usage accounting](https://openrouter.ai/docs/cookbook/administration/usage-accounting).
If cost is absent but the generation ID is known, retain the reservation and
schedule a lookup using the original key version; its metadata includes
`total_cost`. The [generation lookup API](https://openrouter.ai/docs/api/api-reference/generations/get-request-&-usage-metadata-for-a-generation)
requires that generation ID. Lookup availability errors or a temporary 404 do
not prove zero cost. Audit corrections use the original dispatch time.

Classify outcomes by evidence, not only HTTP status:

- **Not sent or confirmed uncharged:** release the hold; validation/key errors
  fail or pause as appropriate. Rate-limited uncharged requests can retry with
  bounded backoff and `Retry-After`, using a new metered attempt if dispatched.
- **Known charge:** settle even if output is unusable. Another model invocation
  requires a new attempt, fresh reservation, and remaining budget.
- **Potentially charged:** timeout, crash after dispatch, malformed/incomplete
  response, or ambiguous server error retains the hold as `uncertain`; never
  automatically resend. A captured ID permits lookup without regeneration.

Without a durable generation ID, the public lookup contract does not provide
recovery by our local request ID. Do not assume provider idempotency. Surface the
attempt to the owner for reconciliation against provider evidence. Provide a
narrow owner-only resolution action with generation ID or explicit confirmed
amount (including zero), reason, and audit entry. It settles accounting once;
it cannot reconstruct a lost result or automatically rerun the operation.
Allow this only for an uncertain attempt after its local request deadline and
worker ownership have expired; reject resolution while a worker is still active.

## 5. Encryption and key replacement

Use AES-256-GCM with a fresh random nonce and authenticated metadata containing
the provider, record ID, and encryption-key version. Supply a versioned master-key
ring and active version through deployment secrets to API and worker processes.
The database stores ciphertext and key version, never the master key. Support
adding a new master-key version and re-encrypting records before retiring the old
one. KMS integration and per-user data keys can follow later requirements.

The owner submits an OpenRouter key through a write-only API field. Responses
return label, fingerprint, status, and timestamps only. Load/decrypt keys only
for the credential operation or gateway call; never place them in River args,
SSE payloads, logs, traces, browser storage, or errors. Disable redirects on
authenticated provider calls.

Replacing the active OpenRouter key creates a new version and changes the pointer
atomically. New dispatches use the new version; attempts already authorized use
their recorded version. Retain the older ciphertext for reconciliation while it
is referenced. Deactivating a local key prevents new dispatches; revoking it at
OpenRouter may also prevent pending lookups, which must remain visible to the
owner. An absent/invalid key pauses model work without breaking ordinary API use.

## 6. River runtime and paused work

Pin River and its runtime pgx and migration database/sql drivers together. Use
a dedicated `river` schema with its own migration history. Register a Goose Go
migration after the application schema/role prerequisites and before queue
grants. That migration uses River's database/sql migrator on Goose's owner
connection, configured for the `river` schema and an explicit target version,
following the documented Goose integration. Future River upgrades get a new
application migration with a new fixed target; never change an applied migration
to silently migrate to the latest version. Do not put River SQL in sqlc's
application schema input or migrate on API/worker startup. Upgrade under the
migration role only. This follows River's
[migration](https://riverqueue.com/docs/migrations) and
[alternate-schema](https://riverqueue.com/docs/alternate-schema) support.

The API uses an insert-only client: create an operation and `InsertTx` in the
same tenant transaction, rolling both back on error. River documents this
[transactional enqueueing contract](https://riverqueue.com/docs/transactional-enqueueing).
Grant API insertion and worker execution only the River objects required by the
pinned version. River queue rows are trusted service infrastructure, not tenant
resources: expose no generic queue REST API, no secrets, and no prompt bodies.
Do not broaden tenant table permissions to make queue access work.

Run `zapply worker` with a direct/session-capable `WORKER_DATABASE_URL`, because
its queue listener must not depend on a transaction-pooled session. Retain
transaction-pooler compatibility for API tenant transactions. Add worker health
and readiness on a separate admin port, finite concurrency, per-call deadlines,
and drain behavior: stop fetching, allow bounded completion, persist uncertain
dispatches, then close connections. Provider balance outages do not make the
ordinary API unready. Add request/operation/attempt trace context across enqueue,
worker, and provider calls, without recording prompts or keys.

Budget-blocked operations hold no reservation. Calculate the earliest expiry
time at which **both** caps can admit their quote, assuming no new spending.
Distinguish this from the first time any capacity returns. If the quote exceeds
a full cap, membership is missing/suspended, or unresolved calls occupy the
in-flight limit, expose that reason rather than inventing a reset time.

Persist `blocked_reason` and `next_eligible_at`; use bounded rescheduling with
[River snoozing](https://riverqueue.com/docs/snoozing-jobs) for eligibility checks
without consuming paid attempts. Wake eligible operations after membership/plan
changes and settlement, with a periodic scan as recovery. Rechecks always repeat
the atomic budget gate. An `uncertain` operation is reconciled, not regenerated.
Use bounded retries for metadata/lookup outages and expose provider/key/balance
failures separately from member allowance exhaustion.

## 7. HTTP contracts, screens, and events

Define these in OpenAPI before handlers; regenerate the Go server and web types:

- Extend `GET /v1/me` with role and membership summary.
- `GET /v1/usage`: own 7/30-day caps, spent, reserved, available, server time,
  earliest capacity-return time, and block reason. No false single reset date.
- `GET /v1/usage/entries`: cursor-paginated own charge metadata, without raw
  provider payloads or credential identifiers.
- `/v1/admin/plans`: list/create; `/{id}` read/update with resource versions.
- `/v1/admin/members`: paginated registered-user list with identity display
  metadata and membership/spending summaries; `/{id}/membership` read/assign/update.
  Expose only identity information needed to identify a registered member, never
  tokens or arbitrary identity-table access.
- `/v1/admin/system-keys`: list/create versions and an explicit activation/
  deactivation action; updates never return the submitted secret.
- `/v1/admin/model-attempts`: filter uncertain/failed accounting; `/{id}/resolve`
  performs audited, version-checked resolution and rejects conflicting repeats.
- `POST /v1/admin/gateway-tests`: enqueue a fixed-prompt diagnostic for the actor;
  `GET /v1/admin/gateway-tests/{id}` reads that actor's operation. Require
  `Idempotency-Key`; same key/body returns the same operation, changed body is 409.

Use 403 for owner-only actions and missing/suspended model access, 429 for member
budget/concurrency denial (with `Retry-After` only when meaningful), 503 for
provider/key/balance unavailability, 422 for invalid input, and 412 for stale
`If-Match`. Extend `Problem` with stable reason codes and safe usage/retry fields.
An asynchronous diagnostic normally returns 202, then reports blocked state in
its operation; do not turn a delayed worker failure into a fictitious HTTP error.

Owner screens: Plans, Members, System keys, and a small unresolved-usage view.
Member screen: Usage, with two allowance indicators, spent versus reserved,
recent charges, and explicit missing-plan/suspended/blocked messages. Keep
remaining amounts visually at zero when overspent, while preserving true spent
amounts. Warn before saving a reduction that may block members immediately.

Use `/usage` and guarded `/admin/...` routes with the existing Angular/ZardUI
components and generated client. Clear role, membership, and usage state on
identity changes; stale requests must not restore another member's data.

Publish tenant `membership.changed`, `usage.changed`, and `model_operation.changed`
events as invalidations with IDs/versions only. Plan edits notify affected
members in the same transaction; for the MVP this can insert one outbox row per
assigned member without taking their account locks. Owner lists refresh after
mutations/on focus; do not publish other members' usage into an owner's tenant
stream. Members refetch on SSE invalidation/reconnect and at the next known
expiry while the page is visible, since time passing emits no database event.

## 8. Build order and acceptance gates

1. **Schema and access:** implement new migrations, owner provisioning,
   platform functions, worker role, sqlc queries, and membership/plan services.
   Prove missing/non-owner/owner access, browser-role grants, cross-tenant
   rejection, safe function paths, and owner revocation with an existing token.
2. **Budget core:** implement decimal conversion, indexed rolling sums, attempt
   transitions, reservations, and deduplicated settlement against a fake clock
   and real Postgres. Test simultaneous calls from multiple pools, both windows
   independently, exact boundaries, late settlement, holds older than 30 days,
   corrected charges, duplicate settlement, overflow, and immediate plan changes.
3. **Worker runtime:** add pinned River migrations, insertion, worker command,
   bounded scheduling, health, drain, and tracing. Prove enqueue rollback,
   committed execution, completed-task replay, and recovery with two workers.
4. **Provider and credentials:** implement encryption, quote validation,
   generation/lookup, and classification using a fake HTTP server. Inject crashes
   before dispatch, after the dispatch commit, after provider acceptance, and
   before/after settlement. Verify uncertain calls never auto-resend, real costs
   survive unusable output, rotations preserve lookups, and secrets stay redacted.
5. **API and UI:** deliver the contracts/screens above. Verify owner plan creation
   and assignment, key replacement, member usage, suspension, limit messaging,
   manual resolution, stale edits, and identity changes. Use meaningful UI state
   tests plus a browser walkthrough; reuse the existing Node test harness.
6. **End-to-end and deployment:** run the fixed diagnostic through transaction,
   River, gateway, ledger, outbox, SSE, and UI. Test with a fake provider in CI;
   perform an explicit live owner diagnostic only after deploying configured
   credentials and assigning that owner a plan. Never make CI spend real credits.

Update `.env.example`, `Makefile` (`make worker`), `deploy/postgres/init.sql`,
`internal/testdb`, CI role setup, README, and deployment notes. Add a worker
process definition compatible with the current distroless image and Heroku's
command behavior; preserve the API's working startup. Production rollout is:
provision restricted worker credentials and encryption keys, migrate, deploy
API/worker, grant owner, configure OpenRouter keys/plans, assign memberships,
then run the diagnostic. Provider funding remains managed in OpenRouter.

Run `make generate`, generated-file drift checks, `make vet`, the integration
suite with `-race`, web tests/typecheck/build, and the image build. Existing
library/jobs behavior must pass with no membership or provider key configured.
Document delivered behavior, operational recovery, and any remaining limitations
in `05-worker.md` when implementation is complete.
