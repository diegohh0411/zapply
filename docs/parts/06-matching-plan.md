# Part 6 implementation plan

Status: implementation proposal based on the [agreed scope](06-matching.md).
This is a build plan, not a record of delivered functionality. Defaults below
resolve implementation choices and can change without expanding the scope.

## Outcome and boundaries

Deliver match runs end to end: requirement extraction and evidence/skill scoring
through the part 5 gateway, content-hash caches, deterministic finalize, user
corrections, automatic runs on job saves, the API, events and the Match tab.

No evaluation tooling, embeddings, highlight rewriting, multi-job ranking, or
model choice. No new infrastructure: Postgres, River and the existing gateway
carry everything.

## Existing integration points

- `internal/worker/worker.go`: `Runtime.Process` is the gateway's state machine
  (quote → `platform.reserve` → `Generate` → `platform.settle` / `uncertain`).
  It is written for one River kind (`llm_diagnostic`) and one fixed prompt
  (`openrouter.DiagnosticPrompt`). Matching generalizes it; it does not fork it.
- `internal/llm/provider.go`: `Provider.Quote(ctx)` and
  `Generate(ctx, key, quote)`. `internal/llm/openrouter` hard-codes 128 output
  tokens and reserves the endpoint's full context.
- `internal/store/migrations/00007_platform.sql`: `llm_operation` (result ≤
  16,000 characters; app may only `SELECT, INSERT`), `platform.reserve` moves
  an operation `pending|blocked → running`, `platform.settle` moves it to
  `completed` (non-empty result) or `failed`, `platform.pending_operations`
  feeds recovery.
- `internal/jobs/jobs.go`: `CreateJob` / `UpdateJob` compute `content_hash` and
  emit outbox events inside one `InTenant` transaction. The auto-run hook goes
  there.
- `internal/worker.Queue`: the API's insert-only River client, used with
  `InsertTx` inside tenant transactions.
- Library tables use a role-less `tenant` policy (`00002`); jobs tables scope
  theirs `TO zapply_app` (`00004`). The worker has no grants on either today.
- `api/openapi.yaml` `EventTopic`, the generated server in `internal/api/gen`,
  and `web/src/app/jobs/job-sheet.*`, `jobs.store.ts`, `job-board.ts`,
  `job-table.ts` on the web side.

New code: `internal/match` (domain: prompts, parsing, hashing, batching,
`Compute`, service). Handlers go in
`internal/api/match.go`, queries in `internal/store/queries/match.sql`, the web
feature in `web/src/app/jobs/match/`.

## 1. Generalize the gateway

Matching needs requests with real content, larger outputs and a hook when a
call ends. Change the gateway once, keep the diagnostic as its first client.

**Provider interface.**

```go
type Request struct {
    System, User string
    Schema       json.RawMessage // JSON Schema for the answer; nil for plain text
    MaxOutput    int
}
Quote(ctx, Request) (Quote, error)          // reservation covers this request
Generate(ctx, key string, Request, Quote) (Result, error)
```

- The diagnostic becomes `Request{User: DiagnosticPrompt, MaxOutput: 128}`.
- `Generate` sends `system` and `user` messages and `max_tokens: MaxOutput`.
  When `Schema` is set and the endpoint lists `structured_outputs` or
  `response_format` in `supported_parameters`, it sends
  `response_format: {type: "json_schema", strict: true}`; otherwise the prompt
  asks for JSON only and parsing is strict either way.
- `MaxOutput` is capped by the endpoint's `max_completion_tokens`. A request
  whose `MaxOutput` the endpoint can't serve is `request_rejected`, uncharged.

**Tighter reservations.** Today the quote reserves the endpoint's full context,
which for a 1M-token model makes every matching call reserve dollars. With the
request known at quote time, input is bounded by **UTF-8 byte length plus a
fixed per-message overhead** (byte-level BPE and byte-fallback tokenizers never
produce more tokens than bytes). The reservation becomes
`min(context, bytes + overhead) × input rate + MaxOutput × output rate +
request rate`. The diagnostic keeps working unchanged; the part 5 note about
full-context reservations is updated. A unit test pins the bound for ASCII,
CJK and emoji inputs, and the settlement path already records any overshoot.

**Requests are stored, not rebuilt.** A prompt depends on library and job
content that can change after queuing. A new tenant table holds it:

```
llm_request   user_id · operation_id (PK, FK llm_operation) · system · user
              schema jsonb? · max_output · created_at
```

Forced RLS, `INSERT, SELECT` for `zapply_app` and `zapply_worker`. `Process`
loads it (falling back to the diagnostic prompt when absent), so the request
hash stays stable across retries and the exact prompt is auditable. The
operation's `request_hash` covers the stored request.

**Larger results.** Migration raises `llm_operation.result` to 65,536
characters. Extract uses `MaxOutput` 3,000 tokens and score batches 6,000; the
batch packer (section 4) keeps answers inside both limits.

**Operation kinds and completion hooks.** `llm_operation.kind` gains a check
constraint listing `diagnostic`, `match_extract`, `match_score`. The River job
keeps kind `llm_diagnostic` (queued rows stay valid) but `Process` now reads
the operation's kind. `Runtime` gets a registry:

```go
OnTerminal map[string]func(ctx context.Context, tx pgx.Tx, op Operation) error
```

called in the same transaction that makes an operation `completed`, `failed`
or `cancelled` (after `settle`, after the final uncharged failure, after the
`needs_review → failed` path). Matching registers a hook that inserts a
`match_advance` River job for the operation's run. Recovery (section 6) covers
a hook lost to a crash.

**Timeouts.** Matching calls can take longer than the diagnostic: per-kind call
timeouts (diagnostic 90 s, match 180 s), the reservation deadline at call
timeout + 60 s, and River `JobTimeout` raised to match.

**Cancellation.** The API can't update `llm_operation`. Add
`platform.cancel_operation(p_id)`: it cancels only `pending` or `blocked`
operations of the caller, writes `llm_operation.changed`, and returns whether
it did. `platform.reserve` already refuses anything not `pending|blocked`, so a
cancel racing a dispatch is safe: one of them wins.

## 2. Schema (migration `00010_matching.sql`)

The tables are as in the spec, with these refinements:

- `match_run`: adds `library_fingerprint` (see section 7), `rerun_requested
  boolean`, `cost` is not stored (summed from the ledger on read).
  Partial unique index `(user_id, job_id) WHERE status IN ('pending','running',
  'blocked','needs_review')` enforces one active run per job.
- `match_run_item`: adds `facts jsonb`, a snapshot of the structured fields
  finalize uses (kind, category, dates and precision, `is_current`,
  `degree_type`/`status`, `auto_include`, linked skill names, highlight
  hashes in order). Finalize computes from the snapshot, so a run's result is
  stable even after the library changes; staleness tells the user to re-run.
- `match_run_operation`: adds `try` (0 or 1) and `ingested_at`.
- Cache answers are stored **by hash, not by id**. Highlights are sent as local
  refs (`h1…hn`) and stored as highlight content hashes; skills as normalized
  names. Part 2 can give unchanged highlights new ids on `PUT`, and a cached
  answer must still map onto them. Ids are resolved at read time.
- `profile.auto_match boolean NOT NULL DEFAULT true`.
- `platform.model_quote(model, endpoint, quote jsonb, fetched_at)`: the worker
  upserts every quote it fetches; the API reads it for estimates (section 8).
- RLS: every new tenant table gets the `tenant` policy for `zapply_app` and
  `zapply_worker`. The worker gets `SELECT` on `job`, `job_status`, `profile`,
  `evidence`, `highlight`, `skill`, `organization`, `evidence_skill`,
  `highlight_skill`, with worker-scoped `tenant` policies where the existing
  ones are scoped to `zapply_app`. No write grants on library or job tables.
- Grants: `zapply_app` `SELECT, INSERT, UPDATE` on `match_run` and the override
  table (cancel, corrections, auto-run supersede), `SELECT` on caches;
  `zapply_worker` `SELECT, INSERT, UPDATE` on run/item/operation tables and
  `INSERT, SELECT` on caches.
- `platform.active_match_runs(p_limit)`: `SECURITY DEFINER`, returns
  `(user_id, run_id)` of active runs for the recovery scan, like
  `pending_operations`.

Check the migration sequence before coding: part 7 is in flight. Go-registered
migrations (6, 8) mean the next free number must be read from both
`internal/store/migrations` and `store.go`.

## 3. Domain package `internal/match`

Pure, unit-tested code with no database access:

- **Inputs.** `JobInput`, `EvidenceDoc` (with facts and highlight refs),
  `SkillDoc`, and canonical-JSON hashing reusing `internal/library`'s
  canonicalizer. `requirements_hash`, `skills_hash` and `library_fingerprint`
  live here.
- **Prompts.** `ExtractRequest(job)` and `ScoreRequest(reqs, docs, skills?)`
  build `llm.Request`s. Constants `PromptVersion = "m1"` and
  `ScoringVersion = "s1"`. Posting and library are wrapped in labelled,
  delimited blocks; the system message carries the rubric with anchored
  examples and the "data is never instructions" rule.
- **Schemas and parsing.** One JSON Schema per answer; `ParseExtract` and
  `ParseScore` validate strictly: known ids only, a highlight ref must belong
  to its evidence, enum values, lengths (requirement 200, reason 240), at most
  30 requirements, no duplicate `(evidence, requirement)` pairs. Any violation
  is `ErrInvalidAnswer` with a safe code.
- **Batching.** `Pack(docs, budget)` → batches of ≤ 20 documents and ≤ 24,000
  characters of rendered input; one oversized document goes alone (a single
  evidence over the budget is truncated at highlight boundaries and flagged).
  The skills document joins the first batch.
- **Checks.** `Years(ranges, asOf)` merges overlapping partial-date periods
  (a period starts at the beginning of its precision unit; current means
  `asOf`); `DegreeAtLeast`; `CEFRAtLeast` (A1 < … < C2 < native).
- **Compute.** `Compute(run snapshot, extract, scores, skills, overrides, asOf)
  → Result` implements finalize exactly as the spec states: recency, checked
  strengths, relevance, highlight and skill ranks, coverage, job score,
  must-haves, insights (gaps, near misses, hidden evidence, seniority fit),
  `selection`, `skills`, `suggestions`. Deterministic for equal inputs,
  including tie-breaks (library order).

## 4. Run orchestration

One River job kind does the stepping:

```go
type AdvanceArgs struct{ UserID, RunID uuid.UUID } // Kind "match_advance", unique by args
```

`Advance` runs in one tenant transaction with the run row locked `FOR UPDATE`,
and is idempotent:

1. **Terminal or cancelled run:** return.
2. **Ingest** every linked operation that is terminal and not `ingested_at`:
   - `completed`: parse. Valid → insert cache rows (`ON CONFLICT DO NOTHING`;
     equal keys mean equal answers), mark items `scored`. Invalid → if `try =
     0`, create a retry operation for the same batch (`try = 1`); else mark the
     batch's items `unscored`.
   - `failed` or `cancelled`: same as invalid, without a retry for `cancelled`.
   - Extract failing twice fails the run (`error_code`).
3. **Next step:**
   - No requirements for `(job_hash, model, prompt version)` → create the
     extract operation (unless one is in flight).
   - Requirements known → set `requirements_hash`, resolve cache hits for every
     pending item and the skills document, then create **one** score operation
     for the next batch of uncached documents. One at a time matches the
     gateway's one-call-in-flight rule and keeps cancellation cheap.
   - Nothing left → finalize: run `Compute`, store `score`, `must_met`,
     `must_total`, counters, `finished_at`; status `completed`, or `partial`
     when anything is `unscored`. If `rerun_requested`, create a new run in the
     same transaction.
4. **Mirror status:** a run whose current operation is `blocked` or
   `needs_review` shows the same status and the operation's reason and
   `next_eligible_at`; otherwise `running`.
5. Emit `match_run.changed` whenever status, stage or counters change.

Creating an operation means: insert `llm_operation` (kind, idempotency key
`match:{run}:{stage}:{batch}:{try}`, request hash), its `llm_request`, the
`match_run_operation` link, and `InsertTx` of the gateway River job, all in the
same transaction. The operation's model is the configured `LLM_MODEL`; the run
records it and the prompt version at creation, so cache keys are stable.

Cached answers are matched by the model and prompt version the run recorded.
Changing `LLM_MODEL` therefore starts fresh caches for new runs without
touching old ones.

**Library reads.** At run creation the service snapshots eligible evidence
(non-archived, not `career_break`, ≤ 300) into `match_run_item` with hashes and
facts, and records `skills_hash` and `library_fingerprint`. Requests are built
from the live rows only when their hash still equals the snapshot; an item
edited in between is marked `skipped` (the run will be stale anyway).

## 5. Automatic runs and supersede

`jobs.Service` gets an optional dependency:

```go
type Matcher interface {
    JobSaved(ctx context.Context, tx pgx.Tx, uid uuid.UUID, job db.Job, hashChanged bool) error
}
```

`CreateJob` and `UpdateJob` call it inside their transaction after the write.
`match.Service.JobSaved` does nothing unless the hash changed (or it's a
create), the description or title is non-empty, the status's stage is not
`closed`, and `profile.auto_match` is true. Then:

- No active run → create one (`trigger = auto`) and `InsertTx` its
  `match_advance`.
- An active run with no operation that left `pending|blocked` → cancel its
  open operations through `platform.cancel_operation`, mark it `cancelled`,
  create the new run.
- An active run that has dispatched → set `rerun_requested`; finalize starts
  the follow-up run.

Moves between columns and archive changes don't touch `content_hash`, so they
never trigger runs. Library edits never call the matcher. Tests run the jobs
service with and without a matcher, so part 4 behavior is unchanged when
matching is disabled.

## 6. Recovery

- The worker's existing periodic scan also calls
  `platform.active_match_runs(1000)` and inserts `match_advance` for each.
  River uniqueness by args makes duplicate inserts harmless.
- `Advance` repairs every lost-hook case by ingesting terminal operations it
  finds, so the hook is an optimization, not a correctness requirement.
- A crash between creating an operation and its River job can't happen: they
  share a transaction.

## 7. Staleness and the board

- `library_fingerprint` = SHA-256 over sorted `(evidence id, content_hash)` of
  eligible evidence plus `skills_hash`. Computing it is one aggregate query per
  user.
- `GET /jobs` computes the fingerprint once per request and joins each job's
  latest run (`DISTINCT ON (job_id)` over an index on `(user_id, job_id,
  created_at DESC)`): `stale = run.job_hash <> job.content_hash OR
  run.library_fingerprint <> current`.
- `GET /match-runs/{id}` additionally counts changed, added and removed items
  by comparing the run's items with current hashes.

## 8. API

As listed in the spec. Implementation notes:

- **Estimate** reads `platform.model_quote` (written by the worker), rebuilds
  the requests that would be sent for cache misses, and sums their per-request
  reservations with the same function the gateway uses. No quote, or a quote
  older than 15 minutes → `estimate_unavailable` with the reason; starting a
  run is still allowed. Available allowance comes from `platform.usage()`.
- **Start** is `POST` with `Idempotency-Key`, stored on the run
  (`UNIQUE (user_id, idempotency_key)`); replay returns the same run.
- **Run detail** loads run, items, cache rows and overrides, calls `Compute`
  with `asOf = finished_at` (so recency doesn't drift on every read), maps
  hashes to current ids, and adds the settled cost from a new
  `platform.operation_costs(p_ids uuid[])` (sum of the caller's own ledger
  entries, no other member data).
- **Corrections** `PUT/DELETE /jobs/{id}/match-requirements/{requirement_id}`
  validate the id against the latest completed run's requirements, upsert the
  override, recompute that run's `score` and `must_*` with `Compute`, bump its
  version and emit `match_run.changed`.
- **Profile** `auto_match` joins the existing `If-Match` flow.
- OpenAPI goes to 0.6.0: new schemas `MatchRun`, `MatchRunSummary`,
  `MatchEstimate`, `Requirement`, `Coverage`, `EvidenceMatch`, `Insight`,
  `Selection`; `JobSummary.match`; `Profile.auto_match`; `EventTopic` gains
  `match_run.changed`. `make generate` regenerates server and client.

## 9. Web

- `web/src/app/jobs/match/`: `match.store.ts` (signals; per-job latest run,
  estimate, corrections; cleared on identity change like the jobs store;
  generation-guarded loads as in the part 4 fixes), `match-tab.ts/.html`, and
  small presentational components for the score ring, requirement row and
  evidence row.
- The job sheet gets a Match tab. SSE `match_run.changed` for the open job
  refetches the run; other jobs' events update board badges through the jobs
  store.
- Board cards and the table get a score badge (dimmed when stale); the table a
  sortable score column, sorted client-side within the loaded page.
- Settings toggle for `auto_match` where the profile is edited.
- Requirement text and reasons render as plain text only.

## 10. Tests and acceptance gates

1. **Gateway generalization.** Fake HTTP: system/user messages, `max_tokens`,
   `response_format` only when supported, byte-bound reservations (ASCII, CJK,
   emoji), `MaxOutput` above the endpoint limit rejected uncharged. Existing
   part 5 tests pass unchanged; the diagnostic still works end to end.
   `cancel_operation` racing `reserve` in two transactions: exactly one wins.
2. **Domain (unit).** Parsers reject each invalid shape; packing respects both
   limits; `Years` across overlapping, partial and current ranges; degree and
   CEFR ordering; `Compute` golden tests on recorded answers covering recency,
   checks capping strength, overrides (`ignore` removes a requirement from the
   denominator), floor, suggestions, career breaks excluded, deterministic
   ties.
3. **Orchestration (Postgres + fake provider).**
   - Cold run: one extract, N score batches, one at a time, completed.
   - Warm re-run: zero calls; editing one highlight: one batch.
   - Same content with new highlight ids still maps citations.
   - Invalid answer → one retry → `unscored` and `partial`; invalid extract
     twice → `failed`.
   - Allowance exhausted mid-run → `blocked` with reason and resume; no
     membership → `blocked` and proceeds after a plan is assigned.
   - Uncertain attempt → `needs_review`, owner resolution → `unscored`, run
     continues; no call is ever resent.
   - Cancel before dispatch makes no call; cancel after dispatch settles and
     still caches.
   - Lost hook (kill between settle and hook) recovered by the scan.
   - Two workers advancing the same run never create duplicate operations.
4. **Auto-run.** Create with description → run; title-only edit → run; move,
   archive, notes edit → none; closed stage → none; `auto_match` off → none;
   rapid saves → supersede before dispatch, `rerun_requested` after; jobs
   rolled back → no run and no River job.
5. **API.** Estimate with and without a quote; start idempotency and `409`
   with `existing_id`; tenant isolation on every endpoint; corrections update
   score and emit; stale on board after job and library edits; cost visible
   only to its owner.
6. **Web.** Store tests in the existing harness: identity change clears runs,
   late responses don't overwrite newer runs, SSE refetch, correction
   round-trip. Typecheck and production build. Browser walkthrough with the
   fake provider: save a job, watch the run progress, read gaps, correct a
   requirement, edit evidence, see stale, re-run.
7. **Live check.** One manual run against OpenRouter after deploy, by the
   owner with a plan. CI never spends credits.

Run `make generate`, generated-file drift, `make vet`, the integration suite
with `-race`, web tests, typecheck and build, and the image build.

## 11. Build order

1. Gateway generalization (section 1) with part 5 regression tests green. This
   is mergeable on its own and unblocks parts 7, 8 and 10.
2. Migration, sqlc queries, `internal/match` domain package and its unit tests.
3. Orchestration, recovery and the worker grants.
4. API, OpenAPI and the auto-run hook.
5. Web.
6. Docs: record delivered behavior in `06-matching.md`, update the part 5 note
   on reservations, `.env.example` (match timeouts if configurable), README.

## Deploying

Apply the new migrations with `make migrate` before rolling out API and worker.
Existing diagnostic operations keep working across the deploy: the River kind
is unchanged and a missing `llm_request` row means the diagnostic prompt.
`LLM_MODEL` must name an endpoint that supports `max_tokens` (already required)
and, preferably, structured outputs.
