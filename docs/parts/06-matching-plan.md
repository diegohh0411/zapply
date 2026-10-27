# Part 6 implementation plan

Status: implementation proposal based on the [agreed scope](06-matching.md).
This is a build plan, not a record of delivered functionality. Defaults below
resolve implementation choices and can change without expanding the scope.

## Outcome and boundaries

Deliver match runs end to end on System One decision models: a System One
transport in the part 5 gateway, a generic per-question decision cache, posting
reading, evidence/highlight/skill scoring, deterministic finalize, user
corrections, automatic runs on job saves, the API, events and the Match tab.

No chat-model calls, generated text, evaluation tooling, embeddings,
multi-job ranking or model choice. No new infrastructure: Postgres, River and
the existing gateway carry everything.

## Existing integration points

- `internal/worker/worker.go`: `Runtime.Process` is the gateway's state machine
  (quote → `platform.reserve` → `Generate` → `platform.settle` / `uncertain`).
  It serves one River kind (`llm_diagnostic`) and one chat prompt
  (`openrouter.DiagnosticPrompt`). Matching adds a second request type to the
  same state machine; it does not fork it.
- `internal/llm/provider.go`: `Provider.Quote(ctx)`, `Generate(ctx, key,
  quote)`, `Lookup(ctx, key, generationID)`. `internal/llm/openrouter` talks to
  `/chat/completions`, reads endpoint metadata from
  `/models/{id}/endpoints`, requires `max_tokens` support, and reserves the
  endpoint's full context.
- `internal/store/migrations/00007_platform.sql`: `llm_operation` (result ≤
  16,000 characters; the app may only `SELECT, INSERT`). `platform.reserve`
  moves an operation `pending|blocked → running`; `platform.settle` moves it to
  `completed` (non-empty result) or `failed`; `platform.pending_operations`
  feeds recovery.
- `internal/jobs/jobs.go`: `CreateJob` / `UpdateJob` compute `content_hash` and
  emit outbox events inside one `InTenant` transaction. The auto-run hook goes
  there.
- `internal/worker.Queue`: the API's insert-only River client, used with
  `InsertTx` inside tenant transactions.
- Library tables use a role-less `tenant` policy (`00002`); jobs tables scope
  theirs `TO zapply_app` (`00004`). The worker has no grants on either today.
- cvd's working System One client and matcher, for reference:
  `cvd/src-tauri/src/engine/{wire,http}.rs` and `cvd/src-tauri/src/matcher/`.

New code: `internal/llm/systemone` (wire types, transport), `internal/decide`
(the decision cache and request packing, reusable by parts 8 and 10),
`internal/match` (posting reading, question building, `Compute`, service).
Handlers in `internal/api/match.go`, queries in
`internal/store/queries/{decide,match}.sql`, the web feature in
`web/src/app/jobs/match/`.

## 0. Verify the API before coding

The API reference for `POST https://openrouter.ai/api/v1/systemone` shows the
request but not every limit. Before step 1, one owner-run probe with a
low-allowance key records, in this document:

- The response shape: `answers`, `usage.input_tokens`, `usage.output_tokens`,
  `usage.cost`, `id`, and `confidence` on answers.
- Whether `GET /api/v1/generation?id=` finds a System One request's `id`.
- The per-request question limit and input context for the configured model
  (`/api/v1/models?output_modalities=decisions` gives `context_length` and
  pricing).
- The status codes for an oversized request (`413`?), an unknown model and a
  bad key.

Each item has a safe default below, so a missing answer narrows behavior
rather than blocking the part.

## 1. System One in the gateway

**Wire types** (`internal/llm/systemone`), mirroring cvd's `wire.rs`:

```go
type Question struct {
    Type         string          // noul | choice | score
    Instructions json.RawMessage // a string, or an object carrying data
    Criteria     json.RawMessage // choice: {option: description}; score: [levels...]; noul: optional
}
type Request struct {
    Model     string
    State     json.RawMessage // string or object
    Questions map[string]Question
}
type Answer struct {
    Type          string
    Noul          *float64
    Choice        string
    Score         *float64
    Probabilities map[string]float64
    Confidence    *float64
}
```

**Provider interface.** Add alongside the chat methods:

```go
QuoteDecision(ctx, systemone.Request) (Quote, error)
Decide(ctx, key string, systemone.Request, Quote) (DecisionResult, error)
```

- The URL is fixed: `https://openrouter.ai/api/v1/systemone`. Not the alpha
  `/api/alpha/decisions`, and not `/v1/systemone` without `/api`, which returns
  `200` with an HTML page. A non-JSON `200` is classified `response_uncertain`,
  never parsed as answers.
- `DecisionResult` carries the parsed answers, `usage`, `usage.cost` as the
  billed amount (parsed without floats, as part 5 does), and `id` as the
  generation ID.
- Errors follow part 5's classification: `400/401/404` uncharged
  `request_rejected`, `402` `provider_balance`, `429` `rate_limited`, `413`
  uncharged `request_too_large` (the packer halves the request and retries),
  anything else after sending `uncertain`.
- **Lookup:** if step 0 shows `/generation` finds System One ids, uncertain
  attempts reconcile automatically as in part 5. Otherwise they go to the
  owner's unresolved-usage view; amounts are fractions of a cent.

**Configuration and quotes.** `DECISION_MODEL` (for example
`typesafe/jev-1.14`) is configured separately from part 5's `LLM_MODEL`.
`QuoteDecision` reads the model's entry from
`/api/v1/models?output_modalities=decisions`, caches it for 15 minutes like the
chat quote, and rejects pricing fields other than `prompt`, `completion` and
`request` (part 5's rule).

**Reservations bounded by the request.** The reservation is
`(bytes + overhead) × prompt rate + questions × 32 × completion rate +
request rate`, where `bytes` is the UTF-8 length of the serialized request.
Byte-level tokenizers never produce more tokens than bytes, so this bounds the
input; 32 output tokens per question bounds answers (observed: 1–21 for a whole
request). A request whose bound exceeds the model's context is split before
quoting. The settlement path already records any overshoot. Part 5's chat
diagnostic is unchanged.

**Stored requests.** A new tenant table keeps the exact request:

```
llm_request   user_id · operation_id (PK, FK llm_operation) · body jsonb · created_at
```

Forced RLS; `INSERT, SELECT` for `zapply_app` and `zapply_worker`. `Process`
loads it, so retries send the same bytes and the operation's `request_hash`
covers them. Operations without a row are diagnostics.

**Operation kinds.** `llm_operation.kind` gets a check constraint:
`diagnostic` or `decision`. `Process` reads the kind and calls `Generate` or
`Decide`; quoting, reservation, settlement, uncertainty and recovery are
shared. The River kind stays `llm_diagnostic`, so queued jobs remain valid.

**Results.** `platform.settle` stores the raw answers JSON in
`llm_operation.result`. A migration raises its limit to 262,144 characters
(128 answers with full distributions fit comfortably).

**Completion hooks.** `Runtime` gets

```go
OnTerminal func(ctx context.Context, tx pgx.Tx, op Operation) error
```

called in the transaction that makes an operation `completed`, `failed` or
`cancelled`. `internal/decide` registers one that ingests answers (section 2)
and notifies the operation's owner; matching's owner inserts a `match_advance`
River job. Recovery (section 7) covers a hook lost to a crash.

**Cancellation.** The API can't update `llm_operation`. Add
`platform.cancel_operation(p_id)`: it cancels only `pending` or `blocked`
operations of the caller, writes `llm_operation.changed`, and returns whether
it did. `platform.reserve` already refuses anything not `pending|blocked`, so a
cancel racing a dispatch is safe: one of them wins.

## 2. The decision cache (`internal/decide`)

A small generic layer between features and the gateway:

```go
type Ask struct {
    State     json.RawMessage
    Questions map[string]systemone.Question // caller's keys
}
// Lookup returns cached answers and the questions still missing.
Lookup(ctx, tx, user, model string, asks []Ask) (Answers, []Pending, error)
// Enqueue packs pending questions into requests and creates one operation
// for the first request, or all of them (the caller chooses).
Enqueue(ctx, tx, user, model string, pending []Pending, owner Owner, opts) ([]uuid.UUID, error)
```

- **Keys.** `state_hash` = SHA-256 of the canonical JSON state; `question_hash`
  = SHA-256 of the canonical JSON question (type, instructions, criteria). The
  wording is part of the hash, so changing a question's text invalidates its
  answers without a version constant.
- **Packing.** Requests group questions sharing a state, up to the model's
  question limit (default 128) and a byte budget derived from its context.
  Request question ids are short (`q1…`); the mapping to cache keys is stored
  with the operation (`decision_request_item(operation_id, request_key,
  state_hash, question_hash)`).
- **Ingestion** (from the hook or recovery): parse the operation's result, and
  for every answer validate it against its question: type matches, `choice`
  names a known option, `score` probabilities cover the legend, probabilities
  are finite, in [0, 1] and sum to 1 ± 0.01, `noul` in [0, 1]. Valid answers go
  into `decision_answer` (`ON CONFLICT DO NOTHING`); invalid or missing ones are
  reported to the owner as discarded.
- Parts 8 and 10 reuse this package unchanged.

## 3. Schema (migration `00010_matching.sql`)

The tables are as in the spec, plus `llm_request`, `decision_request_item`, and:

- `match_run`: partial unique index `(user_id, job_id) WHERE status IN
  ('pending','running','blocked','needs_review')` enforces one active run per
  job; `idempotency_key` with `UNIQUE (user_id, idempotency_key)`.
- `match_run_question`: `user_id · run_id · stage · subject (requirement,
  evidence, highlight, skill ids or text hashes) · state_hash · question_hash ·
  status (cached, pending, answered, discarded, unanswered) · tries`. It lets
  `Advance` know exactly what the run still needs and powers progress counts.
- `profile.auto_match boolean NOT NULL DEFAULT true`.
- `platform.model_quote(model, quote jsonb, fetched_at)`: the worker upserts
  every decision quote it fetches; the API reads it for estimates.
- `platform.active_match_runs(p_limit)` and
  `platform.operation_costs(p_ids uuid[])` (`SECURITY DEFINER`, caller's own
  ledger only).
- RLS: every new tenant table gets the `tenant` policy for `zapply_app` and
  `zapply_worker`. The worker gets `SELECT` on `job`, `job_status`, `profile`,
  `evidence`, `highlight`, `skill`, `organization`, `evidence_skill`,
  `highlight_skill`, with worker-scoped `tenant` policies where the existing
  ones are scoped to `zapply_app`. No write grants on library or job tables.
- Grants: `zapply_app` `SELECT, INSERT, UPDATE` on `match_run` and
  `match_override` (start, cancel, corrections, supersede), `SELECT, INSERT` on
  `llm_request` and `decision_request_item`, `SELECT` on the rest;
  `zapply_worker` read/write on run and question tables and `INSERT, SELECT`
  on `decision_answer`.

Check the migration sequence before coding: part 7 is in flight. Go-registered
migrations (6, 8) mean the next free number must be read from both
`internal/store/migrations` and `store.go`.

## 4. Matching domain (`internal/match`)

Pure, unit-tested code with no database access:

- **Posting reader.** `Lines(description) []Line`: normalize whitespace and
  bullet markers (`-`, `*`, `•`, `·`, numbered lists), split paragraphs into
  sentences, drop lines under three words, split lines over 400 characters,
  keep 120. `YearsIn(line) *int` parses "5+ years", "at least five years",
  "3-5 years" (lower bound), "minimum of 2 yrs", in English first; other
  languages leave it empty.
- **Questions.** Builders for each stage, with the wording in one file:
  `LineClass`, `LineType`, `Seniority`, `DegreeLevel`, `LanguageLevel`,
  `EvidenceStrength` (the four-level `score`), `HighlightShows`, `SkillValued`.
  Data (the line, the evidence document, the highlight) goes inside the
  instructions object, as cvd's `with_data` does; the question sentence refers
  to it by name. Evidence documents are rendered from library fields in a fixed
  field order, so equal content gives an equal hash.
- **States.** `read`, `level` and the skill questions use the posting
  (`title`, `company`, `description`). `evidence` and `highlights` use only
  `title` and `company`, so scores survive description edits that keep a line.
- **Requirements.** `Requirements(lines, answers)` applies the class rules,
  dedupes, keeps 40, and attaches `min_years` and levels.
- **Checks.** `Years(ranges, asOf)` merges overlapping partial-date periods
  (a period starts at the beginning of its precision unit; current means
  `asOf`); `DegreeAtLeast`; `CEFRAtLeast` (A1 < … < C2 < native).
- **Compute.** `Compute(snapshot, requirements, answers, overrides, asOf) →
  Result` implements finalize exactly as the spec states: expected strengths,
  recency, checks, relevance, highlight and skill ranks, coverage with
  confidence, job score, must-haves, insights, `selection`, `skills`,
  `suggestions`. Deterministic for equal inputs, ties broken by library order.
  Unanswered pairs are unknown, not zero: they don't lower coverage or
  relevance, and the result counts them.

## 5. Run orchestration

One River job kind does the stepping:

```go
type AdvanceArgs struct{ UserID, RunID uuid.UUID } // Kind "match_advance", unique by args
```

`Advance` runs in one tenant transaction with the run row locked `FOR UPDATE`,
and is idempotent:

1. **Terminal or cancelled run:** return.
2. **Ingest** any terminal operation of the run not yet ingested (through
   `internal/decide`). Discarded questions with `tries < 2` go back to
   `pending`; otherwise `unanswered`.
3. **Fill the current stage:** build its questions, mark cache hits
   `answered`, and if nothing is in flight, enqueue **one** request of pending
   questions. One request at a time matches the gateway's one-call-in-flight
   rule and keeps cancellation cheap.
4. **Advance the stage** when every question in it is answered or unanswered:
   `read` → build requirements (store `match_run_requirement`, seniority) →
   `level` (skipped if none) → `evidence` (evidence × requirement and skills)
   → `highlights` (pairs with expected strength ≥ 0.4) → `finalize`.
5. **Finalize:** run `Compute`, store `score`, `must_met`, `must_total`,
   counters and `finished_at`; status `completed`, or `partial` when anything
   is `unanswered`. If `rerun_requested`, create the follow-up run in the same
   transaction.
6. **Mirror status:** a run whose in-flight operation is `blocked` or
   `needs_review` shows the same status, reason and `next_eligible_at`;
   otherwise `running`. An operation that ends `failed` after an owner
   resolution sends its questions back to `pending` as a new operation.
7. Emit `match_run.changed` whenever status, stage or counters change.

**Library reads.** At run creation the service snapshots eligible evidence
(non-archived, not `career_break`, ≤ 300) into `match_run_item` with hashes and
`facts`, and records `skills_hash` and `library_fingerprint`. Questions are
built from live rows only while their hash equals the snapshot; an item edited
mid-run is skipped (the run will be stale anyway).

The run records `DECISION_MODEL` at creation; its cache lookups use that model,
so changing the configured model starts fresh answers for new runs only.

## 6. Automatic runs and supersede

`jobs.Service` gets an optional dependency:

```go
type Matcher interface {
    JobSaved(ctx context.Context, tx pgx.Tx, uid uuid.UUID, job db.Job, created, hashChanged bool) error
}
```

`CreateJob` and `UpdateJob` call it inside their transaction after the write.
`match.Service.JobSaved` does nothing unless it's a create or the hash changed,
the title or description is non-empty, the stage is not `closed`, and
`profile.auto_match` is true. Then:

- No active run → create one (`trigger = auto`) and `InsertTx` its
  `match_advance`.
- An active run with nothing dispatched (all its operations `pending` or
  `blocked`) → cancel them through `platform.cancel_operation`, mark the run
  `cancelled`, create the new run.
- An active run that has dispatched → set `rerun_requested`; finalize starts
  the follow-up.

Moves, archive changes and notes don't change `content_hash`, so they never
trigger runs. Library edits never call the matcher. Tests run the jobs service
with and without a matcher, so part 4 behavior is unchanged when matching is
disabled.

## 7. Recovery

- The worker's periodic scan also calls `platform.active_match_runs(1000)` and
  inserts `match_advance` for each; River uniqueness makes duplicates harmless.
- `Advance` ingests any terminal operation it finds, so the hook is an
  optimization, not a correctness requirement.
- Operations and their River jobs are created in one transaction, so neither
  exists without the other.

## 8. Staleness and the board

- `library_fingerprint` = SHA-256 over sorted `(evidence id, content_hash)` of
  eligible evidence plus `skills_hash`; one aggregate query per user.
- `GET /jobs` computes it once per request and joins each job's latest run
  (`DISTINCT ON (job_id)` over an index on `(user_id, job_id, created_at
  DESC)`): `stale = run.job_hash <> job.content_hash OR
  run.library_fingerprint <> current`.
- `GET /match-runs/{id}` additionally counts changed, added and removed items.

## 9. API

As listed in the spec. Implementation notes:

- **Estimate** builds the questions a run would ask, looks them up in the
  cache, packs the misses and sums each request's reservation from
  `platform.model_quote`. Stages after `read` depend on answers not yet known,
  so the estimate assumes every line is a requirement (capped at 40) and
  every pair reaches the highlight stage; it's an upper bound. No quote, or one
  older than 15 minutes → `estimate_unavailable`; starting a run still works.
- **Start**, **cancel**, **list** and **detail** as specified. Detail loads the
  run, requirements, items, cached answers and overrides, calls `Compute` with
  `asOf = finished_at`, maps hashes to current ids, and adds the settled cost
  from `platform.operation_costs`.
- **Corrections** `PUT/DELETE /jobs/{id}/match-overrides/{text_hash}` upsert
  the override, recompute the latest completed run's `score` and `must_*`,
  bump its version and emit `match_run.changed`.
- OpenAPI goes to 0.6.0: `MatchRun`, `MatchRunSummary`, `MatchEstimate`,
  `Requirement`, `Coverage`, `EvidenceMatch`, `Insight`, `Selection`;
  `JobSummary.match`; `Profile.auto_match`; `EventTopic` gains
  `match_run.changed`. `make generate` regenerates server and client.

## 10. Web

- `web/src/app/jobs/match/`: `match.store.ts` (signals; per-job latest run,
  estimate, corrections; cleared on identity change like the jobs store;
  generation-guarded loads as in the part 4 fixes), `match-tab.ts/.html`, and
  small components for the score ring, requirement row (quoted line, class
  control, coverage with confidence) and evidence row.
- The job sheet gets a Match tab. SSE `match_run.changed` for the open job
  refetches the run; other jobs' events update board badges through the jobs
  store.
- Board cards and the table get a score badge (dimmed when stale); the table a
  sortable score column, client-side within the loaded page.
- Settings toggle for `auto_match` where the profile is edited.
- Requirement text renders as plain text only.

## 11. Tests and acceptance gates

1. **Transport (fake HTTP).** Request body shape and URL; answers, `usage.cost`
   and `id` parsed; a `200` HTML body classified uncertain, never parsed;
   `413` triggers a split; byte-bound reservations for ASCII, CJK and emoji;
   pricing with unknown fields rejected. Part 5's tests pass unchanged and the
   chat diagnostic still works end to end. `cancel_operation` racing `reserve`:
   exactly one wins.
2. **Decision cache.** Equal content hits, changed wording misses; packing
   respects the question limit and byte budget and groups by state; each
   invalid answer shape is discarded; ingesting twice is a no-op.
3. **Matching domain (unit).** Line splitting on real-world postings (bullets,
   paragraphs, numbered lists); `YearsIn`; requirement rules and the 40 cap;
   `Years` across overlapping, partial and current ranges; degree and CEFR
   ordering; `Compute` golden tests on recorded answers covering expected
   strengths, recency, checks capping strength, overrides (`ignore` leaves the
   denominator), the floor, suggestions, career breaks excluded, unanswered
   pairs, deterministic ties.
4. **Orchestration (Postgres + fake provider).**
   - Cold run walks every stage, one request at a time, and completes.
   - Warm re-run: zero requests. Editing one evidence: only its questions.
     Editing the description but keeping lines: only `read` questions.
   - Discarded answers retried once, then `unanswered` and `partial`.
   - Allowance exhausted mid-run → `blocked` with reason, then resumes; no
     membership → `blocked`, proceeds after a plan is assigned.
   - Uncertain attempt → `needs_review`; after owner resolution its questions
     go out in a new operation; the lost attempt is never resent.
   - Cancel before dispatch sends nothing; after dispatch the answers are still
     cached.
   - Lost hook recovered by the scan; two workers never create duplicate
     operations for one run.
5. **Auto-run.** Create with description → run; title edit → run; move,
   archive, notes → none; closed stage → none; `auto_match` off → none; rapid
   saves → supersede before dispatch, `rerun_requested` after; a rolled-back
   job write leaves no run and no River job.
6. **API.** Estimate with and without a quote; start idempotency and `409`
   with `existing_id`; tenant isolation on every endpoint; corrections update
   the score and emit; stale on the board after job and library edits; cost
   visible only to its owner.
7. **Web.** Store tests in the existing harness: identity change clears runs,
   late responses don't overwrite newer runs, SSE refetch, correction
   round-trip. Typecheck and production build. Browser walkthrough with the
   fake provider: save a job, watch the stages, read gaps, correct a
   requirement, edit evidence, see stale, re-run.
8. **Live check.** One manual run against `/api/v1/systemone` after deploy, by
   the owner with a plan. CI never spends credits.

Run `make generate`, generated-file drift, `make vet`, the integration suite
with `-race`, web tests, typecheck and build, and the image build.

## 12. Build order

1. The API probe (section 0), recorded here.
2. System One transport, stored requests, operation kinds, hooks and
   cancellation (section 1), with part 5 regressions green. Mergeable on its
   own, and what parts 8 and 10 build on.
3. `internal/decide` and its tables.
4. Migration, sqlc queries, `internal/match` domain and unit tests.
5. Orchestration, recovery and worker grants.
6. API, OpenAPI and the auto-run hook.
7. Web.
8. Docs: record delivered behavior in `06-matching.md`, the part 5 note on
   reservations, `.env.example` (`DECISION_MODEL`), README.

## Deploying

Apply the new migrations with `make migrate` before rolling out API and worker.
Set `DECISION_MODEL` on the worker. Existing diagnostic operations keep working
across the deploy: the River kind is unchanged and an operation without a
stored request is a diagnostic. Without `DECISION_MODEL`, runs wait as
`blocked` with `model_not_configured` and the rest of the app is unaffected.
