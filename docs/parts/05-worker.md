# Part 5: Worker runtime and LLM gateway

Status: implemented. The [implementation plan](05-worker-plan.md) records the
design; this document records the delivered behavior and setup. Provider
credentials, model configuration, and production deployment are still operational
setup steps, not bundled credentials or an automatic deployment.

This part introduces River workers and a platform-funded LLM gateway through
OpenRouter. It includes the platform owner role, owner administration screens,
and member usage screens. Matching logic remains in part 6.

## Agreed MVP scope

- The platform owner manages system-owned OpenRouter keys through the UI.
  Keys are encrypted at rest, never exposed to members, and replaceable without
  changing memberships. BYOK is deferred until after the MVP succeeds.
- Memberships are assigned by the owner. Paid subscription billing is outside
  the MVP scope.
- Each plan has a name, a USD cost allowance for the past 7 days, and a USD cost
  allowance for the past 30 days. The owner can create and edit plans in the UI.
- Each membership records its member, assigned plan, and active or suspended
  status. Plan changes take effect immediately and preserve usage history.
- Both allowances use rolling windows. There are no calendar resets or rollover
  balances; capacity returns as older usage leaves each window.
- There is no application-wide spending ceiling in the MVP. The platform relies
  on OpenRouter's balance enforcement for that limit.

## Owner and member experience

The platform owner can create and edit plans, assign memberships, suspend access
to model work, manage OpenRouter keys, and view member spending. These are
platform permissions, distinct from ordinary tenant access, and must be enforced
by the backend as well as the UI.

Members see usage against both allowances, labelled **Past 7 days** and
**Past 30 days**, and when capacity will next become available. There is no
single reset time. Active reservations also reduce available capacity.

When either allowance cannot cover a new model call, model work pauses with a
clear explanation. Members retain access to their library, jobs, and existing
results. Suspended memberships cannot start new model calls.

## Budget enforcement

Each model call belongs to a member and passes through the gateway:

1. Check the membership and its current plan.
2. Atomically check both rolling allowances and reserve an estimated maximum
   cost before sending the request. Concurrent calls must see existing
   reservations and cannot independently spend the same remaining allowance.
3. Record the call's provider/model, token usage, pricing snapshot, and actual
   cost in a usage ledger. Reconcile the reservation and release its unused
   portion.

Outstanding reservations count against both allowances while a call remains
unresolved, even if the reservation is older than a window. Retries consume
allowance whenever the provider charges for them. Changing a plan does not
reset the ledger or forgive previous usage. A lower allowance can therefore
pause new work immediately.

Costs use the dispatch timestamp, including late settlements/corrections. Windows
are exactly 168 and 720 hours. Money is stored as integer picodollars and exposed
as USD decimal strings with 12 fractional digits. An unknown provider outcome
keeps its reservation and cannot automatically trigger another paid call.

## Delivered implementation

1. **API and database:** `internal/membership` implements synchronous owner
   administration and member reads. Platform tables are private; narrow functions
   run under a non-login `zapply_platform` role with explicit permissions and RLS
   policies. Tenant operations/results retain forced RLS. OpenAPI is 0.5.0.
2. **Worker:** `zapply worker` runs pinned River jobs under `zapply_worker`.
   Creating a diagnostic and enqueueing it share one transaction. Rescued/retried
   jobs reuse persisted state, and a periodic scan recovers abandoned operations.
   Worker concurrency defaults to four, with one unresolved call per member.
3. **Gateway and accounting:** `internal/llm` and `internal/llm/openrouter`
   quote, reserve, invoke once, settle, and reconcile. The ledger supports
   idempotent settlements and append-only corrections. Ambiguous calls keep their
   holds; a known generation ID permits lookup using the original credential.
4. **Keys:** `internal/secrets` uses versioned AES-256-GCM credentials bound to
   their record/provider. The UI replaces active provider keys; retained versions
   support pending accounting. A deployment command re-encrypts existing records
   when rotating the master key.
5. **Web:** `/admin/plans`, `/admin/members`, `/admin/system-keys`, and
   `/admin/usage` provide the owner controls. `/usage` shows member allowances,
   reservations, charges, and upcoming expiry. Identity changes invalidate cached
   data. New outbox topics invalidate member views through the existing SSE feed.
6. **Operations:** worker health/readiness, bounded drain, and optional OTLP
   tracing connect API enqueue, worker execution, and provider calls. Dockerfile
   targets supply exec-form API/worker/relay commands for Heroku's distroless
   deployment. No deployment is performed by implementing this part.

Integration tests cover simultaneous requests, independent weekly/monthly caps,
window expiry, unchanged plan-value round trips, plan edits, suspension,
idempotent queueing and settlement, late corrections, committed-dispatch crashes,
uncertain outcomes, River recovery, provider/master-key rotation, and permissions.
Fake HTTP tests verify routing, pricing, exact billed cost, and no automatic POST
retry. UI regression tests cover identity/request races; a browser walkthrough
checks owner/member administration and secret redaction. Matching, rendering,
paid billing, BYOK, and a global spending ceiling remain outside this part.

## Setup and recovery

- Apply migrations with `make migrate`. New migrations are 7 (platform), 8 (Go
  migration for River's pinned schema target), and 9 (queue grants). Runtime
  processes do not apply migrations. Production must provision restricted worker
  credentials. Migration 7 works with a non-superuser migration owner that has
  `CREATEROLE` (for example Supabase's `postgres`): it creates `zapply_platform`
  and grants itself `SET` on it, without inheritance, to assign function
  ownership. If `zapply_platform` was created by another role, grant the
  migration owner `SET` on it first. That role must stay non-login.
- Configure `WORKER_DATABASE_URL` using a direct/session-capable connection,
  `SYSTEM_KEY_RING`, and `SYSTEM_KEY_ACTIVE_VERSION` on API and worker processes.
  The ring is a JSON map of version names to base64-encoded random 32-byte keys.
  Keep it in deployment secrets. `.env.example` shows shell-safe quoting.
- Sign in first, obtain the app user UUID from `GET /v1/me`, then run
  `zapply owner grant --user-id USER_UUID` with `MIGRATE_DATABASE_URL` loaded.
  `zapply owner revoke` removes that permission; no sign-in token replacement is
  needed. Owner status does not create a membership or bypass usage limits.
- In the owner UI, create plans, assign memberships, and save/activate an
  OpenRouter key. Configure an explicit `LLM_MODEL` and `LLM_ENDPOINT` on the
  worker. Run API, relay, and `make worker` separately. The system-key screen can
  enqueue one fixed-prompt diagnostic, billed to the owner's membership.
- An uncertain attempt with a known generation ID is looked up automatically.
  Without an ID, the owner confirms its actual charge against provider evidence
  in Unresolved usage. Resolution requires the local request deadline to pass,
  records the evidence, and never automatically regenerates lost output.
  Reconciliation passes do not change an unresolved attempt, so the owner's
  resolution stays current and the original error class is kept.
- A provider-reported charge is saved with retries for up to a minute. If it
  still cannot be saved, the attempt becomes uncertain with its generation ID
  (error `settlement_failed`) and the automatic lookup recovers the cost.
- If a stored key cannot be decrypted, the operation retries up to four times
  (one uncharged attempt each) and then fails.
- To rotate master encryption keys, first deploy a ring containing both versions
  to all API/worker processes, with the new active version. Run
  `zapply secrets reencrypt` using migration credentials and that ring. Retire the
  old master only after all stored credentials have been re-encrypted. Replacing
  a provider key is a separate action; revoking an old key at OpenRouter can
  prevent pending lookups even while its ciphertext remains available.

## MVP limits

- Only non-streaming text diagnostics are exposed in this part. Future matching
  stages use the same gateway; there is no arbitrary chat API.
- Reservations conservatively cover the selected endpoint's full context plus
  bounded output until model-specific input tokenization is added. A request can
  therefore pause even when its eventual actual charge would fit the allowance.
  Quotes expire after 15 minutes; routing is restricted by endpoint and prices.
- Paused work is rechecked at least once per minute (provider backoff can be
  longer). Usage expiry is a hint, not a promise that a particular request fits.
- A provider charge exceeding its quote is recorded in full and reported in the
  audit/logs. The application cannot guarantee provider billing; subsequent
  dispatches still enforce both allowances against the real ledger.
- Provider integration is verified with fake HTTP responses and documented live
  API shapes. No paid OpenRouter call is made without operational configuration
  and an explicit diagnostic or later domain task.

## Implementation design

The [implementation plan](05-worker-plan.md) proposes owner provisioning and
database boundaries, exact cost accounting, OpenRouter reconciliation, worker
recovery and scheduling, key management, UI/API contracts, and acceptance gates.
Its implementation defaults are distinguished from the agreed product scope.
