# Part 6 implementation plan

Status: implemented against the [agreed scope](06-matching.md), with live
provider verification still pending. The sections below retain the build design;
the verification record distinguishes tested behavior from live prerequisites.
Identity email uses migration 00010; matching follows at 00011 and shared
model naming at 00012.

## Implementation and verification record

Implemented in `internal/model/systemone`, `internal/model/openrouter/decisions.go`,
`internal/decide`, `internal/match`, migration `00011_matching.sql`, the generated
0.6.0 API, and the jobs/admin/profile web features. Matching uses the existing
operation state machine, reservation gate, encrypted keys and River kind.

The fake HTTP fixture in `internal/model/openrouter/decisions_test.go` verifies the
stable `/api/v1/systemone` route, response accounting, JSON-only decoding,
per-question billing expansion and request rejection before sending. It is a
fixture contract, not proof of OpenRouter's live billing behavior. Any catalogue
model the owner selects can dispatch. Without a known bound it gets
`ownerSelectedBound`, which conservatively assumes full context and the
request fee charged once per question. These billing assumptions are not
provider-verified. Generation lookup is off for these models, so an uncertain attempt
waits for the owner to resolve it.

`internal/match/service_test.go` covers cold and fully cached runs, discarded
answers and partial results, corrections, tenant isolation, stale library input,
automatic superseding, unconfigured model binding, retained model snapshots,
uncertain accounting followed by a new operation, cached estimates, and the
cancel/reserve race. Domain and cache tests cover source spans, short lines,
years, overlapping periods, deterministic ties, unknown coverage, ignored
requirements, numeric JSON canonicalization and answer validation. Web tests
exercise identity changes, out-of-order responses, SSE and correction reloads.

Implementation details resolved during testing:

- `match_run_operation` retains every operation, including retries, for complete
  cost accounting after question mappings advance to a new operation.
- Job locks are exposed to the worker through a tenant-scoped definer function;
  the worker has no general permission to edit jobs. Automatic superseding checks
  dispatch history while holding the reservation lock.
- A local size rejection records adapter packing limits before a new bundle is
  created. This does not authorize retrying an uncertain HTTP rejection.
- Canonicalization normalizes numeric spelling as well as key order, so jsonb
  expansion of exponent notation preserves the cache and request hashes.
- Staleness fingerprints complete evidence/skill snapshots, including linked
  organizations and highlights. Details recompute from immutable inputs at
  `finished_at` and hide deleted evidence, highlights and skills from selections.
- More than 300 eligible evidence documents produces a failed run with
  `evidence_limit_exceeded`; the job save and board remain usable.
- Estimates require a fresh verified quote. Before reading completes they assume
  no scoring cache hits and reserve for up to 40 longest lines; retries and price
  changes remain excluded. Completed warm runs can estimate zero requests.
- The Part 7 `selection`, `skills` and `suggestions` contract is exposed at the
  top level of run detail as well as in its grouped `result`.

Verification completed on 2026-10-27:

- `make generate`, followed by checksums of all generated Go/client files: no drift.
- `make vet` and `git diff --check`.
- `go test -race -count=1 ./...` with disposable Postgres 17, plus final targeted
  matching/API/cache/transport/gateway regression runs after refinements.
- Web `npm test`, `npm run typecheck` and `npm run build`.
- Local application image build (`docker build -t zapply:matching-check .`).

Live paid probes and a browser walkthrough against a live provider have **not**
been performed. No paid API call is part of the tests. Run migration 00011 before
updating API/worker processes; configure the owner model in Administration →
System keys. Without a model, runs wait as `model_not_configured`.

## Review follow-up: stable cache inputs and language checks

Posting-read state contains only the posting and source lines. Each skill's name
is supplementary data in its own Noul question, so adding or changing a skill
preserves all unrelated posting answers. Evidence scoring uses a dedicated
content projection without CV inclusion flags or content-hash bookkeeping;
highlight metric values and units are included. Display flags remain in the
immutable snapshot for deterministic selection and staleness.

Language checks and their explanations use the snapshot's full language skill
list, including skills not linked to the evidence being scored. Regression tests
cover cache reuse after inclusion toggles, new-skill-only requests, metric edits,
and unlinked/mismatched/insufficient language proficiency. This changes cache keys
for old state shapes; existing answers remain stored but are not reused for the
new shapes. Live dispatch is still disabled pending verified billing bounds.

## Shared model naming

Migration `00012_model_naming.sql` renames the shared gateway tables and their
constraints to `model_operation`, `model_request`, `platform.model_attempt` and
`platform.model_usage`. It preserves existing rows, grants, foreign keys and
function ownership, and rewrites the platform functions to use the new names.
Migrations 7 and 10 (identity email) retain their original identifiers. Matching
runs at version 11 before this naming migration.

The shared packages live under `internal/model`. Administration uses
`/v1/admin/model-attempts`, generated `ModelOperation`/`ModelAttempt` names, and
`model_operation.changed` events. River jobs use `model_operation`. `LLM_MODEL`
and `LLM_ENDPOINT` remain specific to the text diagnostic.

Apply migrations through **12** before updating API and worker processes.

## Outcome and boundaries

Deliver match runs end to end on System One decision models: a System One
transport in the part 5 gateway, a generic per-question decision cache, posting
reading, evidence/highlight/skill scoring, deterministic finalize, user
corrections, automatic runs on job saves, the API, events and the Match tab.

No chat-model calls, generated text, evaluation tooling, embeddings,
multi-job ranking or per-user model choice. Owner model configuration is
included. No new infrastructure: Postgres, River and the existing gateway
carry everything.

## Existing integration points

- `internal/worker/worker.go`: `Runtime.Process` is the gateway's state machine
  (quote → `platform.reserve` → `Generate` → `platform.settle` / `uncertain`).
  It serves one River kind (`model_operation`) and one chat prompt
  (`openrouter.DiagnosticPrompt`). Matching adds a second request type to the
  same state machine; it does not fork it.
- `internal/model/provider.go`: `Provider.Quote(ctx)`, `Generate(ctx, key,
  quote)`, `Lookup(ctx, key, generationID)`. `internal/model/openrouter` talks to
  `/chat/completions`, reads endpoint metadata from
  `/models/{id}/endpoints`, requires `max_tokens` support, and reserves the
  endpoint's full context.
- `internal/store/migrations/00007_platform.sql`: `model_operation` (result ≤
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

New code: `internal/model/systemone` (wire types, transport), `internal/decide`
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
- Any documented request/per-question context limits and billing expansion
  for the configured model. The filtered model catalogue supplies context and
  pricing, not an assumed 128-question cap. A small successful probe cannot
  establish an upper bound on billing or request size.
- The status codes for an oversized request (`413`?), an unknown model and a
  bad key.

Record the documentation or fixture supporting each conclusion. Fake-provider
work can proceed without a live key. Unverified billing bounds disable live
dispatch for that model; unknown lookup behavior uses owner reconciliation.

## 1. System One in the gateway

**Wire types** (`internal/model/systemone`), mirroring cvd's `wire.rs`:

```go
type Question struct {
    Type         string          // noul | choice | score
    Instructions json.RawMessage // string, object or array
    Criteria     json.RawMessage // choice: {option: description}; score: [levels...]; noul: optional
}
type Request struct {
    Model     string
    State     json.RawMessage // string, object or array
    Questions map[string]Question
}
type Answer struct {
    Type          string
    Noul          *float64
    Choice        string
    Score         *float64
    Probabilities map[string]float64
    Confidence    *float64 // Choice/Score; Noul has no confidence field
    Legend        map[string]json.RawMessage // Score levels
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
- Errors reuse part 5's provider classification for verified uncharged
  rejections, balance and rate limits. Split and retry a size rejection only
  when the adapter establishes it was uncharged; a status such as `413` alone
  is not proof for every provider. Anything else after sending is uncertain.
- **Lookup:** if step 0 shows `/generation` finds System One ids, uncertain
  attempts reconcile automatically as in part 5. Otherwise they go to the
  owner's unresolved-usage view with their reservations retained.

**Owner configuration and quotes.** Add a singleton
`platform.model_settings(decision_model, version, updated_at)`, with owner-only
read/update functions and worker read access. The selected model is separate
from the chat diagnostic's `LLM_MODEL`; no `DECISION_MODEL` environment variable
is required. Use the existing encrypted system OpenRouter key, including key
version retention on attempts. Never expose it to the browser.

- `GET /v1/admin/decision-models` proxies
  `/api/v1/models?output_modalities=decisions`, handling catalogue pagination
  if returned. Cache metadata for 15 minutes; return id, name, context, pricing
  and compatibility reason. Check `architecture.output_modalities` too.
- `GET/PUT /v1/admin/model-settings` reads/updates `{decision_model}` with a
  version check. Validate a selection against refreshed metadata; a failed
  catalogue refresh preserves the saved setting. Both endpoints require the
  existing platform-owner check. No secret is returned or stored here.
- `QuoteDecision` accepts the run's model, not the current global selection.
  Capture the price snapshot with the attempt. Missing model, stale metadata
  that cannot be refreshed, or unsupported billing blocks before dispatch.
- A run with a selected model snapshots it at creation. A run created before
  configuration has a nullable model, waits as `model_not_configured`, then
  atomically binds the first configured model before lookup or dispatch. Later
  changes affect new runs only. Recovery wakes blocked runs after configuration.

**Reservations and packing require verified bounds.** Remove the previous
`bytes + overhead` and `32 output tokens per question` assumptions. JSON wire
bytes alone do not bound provider-side expansion, repeated state evaluation,
or output billing across every decision model.

For the MVP, enable only pricing profiles with zero completion rate and
supported prompt/request charges; reject nonzero unknown pricing components.
A model adapter must document whether billable input is bounded per request
or per independently evaluated question. Conservatively reserve the advertised
context times the prompt rate for each billable evaluation, plus the bounded
request charge. If evaluation is per question, multiply the context allowance
by the question count. If the advertised context does not bound billable
input, or request-fee multiplicity is unknown, mark the model unsupported until
a documented bound exists. No paid call is sent on a guessed bound.

A verified tokenizer and provider serialization can later tighten the bound.
Packing uses a separate conservative input-size check appropriate to the
adapter, documented model limits and local request/response payload budgets.
Start with all compatible pending questions sharing a state; split only to
meet those budgets or a confirmed size rejection. A single oversized question
is reported explicitly. No default provider question-count cap is invented.
Part 5's chat diagnostic and central allowance gate are unchanged.

**Stored requests.** A new tenant table keeps the exact request:

```
model_request   user_id · operation_id (PK, FK model_operation) · body jsonb · created_at
```

Forced RLS; `INSERT, SELECT` for `zapply_app` and `zapply_worker`. Store a
canonical request encoding or canonicalize the jsonb on serialization, so
retries preserve the hashed payload. `Process` loads it; operations without a
row are diagnostics. This does not authorize resending uncertain attempts.

**Operation kinds.** `model_operation.kind` gets a check constraint:
`diagnostic` or `decision`. `Process` reads the kind and calls `Generate` or
`Decide`; quoting, reservation, settlement, uncertainty and recovery are
shared. Jobs use River kind `model_operation`.

**Results.** `platform.settle` stores raw answers JSON in
`model_operation.result`. Raise its limit to 262,144 characters as a local
storage budget. Packing budgets response size from the accepted question
schema, including echoed legends. Bound HTTP response reads too; oversized
responses follow the uncertain-result path and retain their reservation
until accounting resolves. Never silently truncate answers.

**Completion hooks.** `Runtime` gets

```go
OnTerminal func(ctx context.Context, tx pgx.Tx, op Operation) error
```

called in the transaction that makes an operation `completed`, `failed` or
`cancelled`. `internal/decide` registers one that ingests answers (section 2)
and notifies the operation's owner; matching's owner inserts a `match_advance`
River job. Recovery (section 7) covers a hook lost to a crash.

**Cancellation.** The API can't update `model_operation`. Add
`platform.cancel_operation(p_id)`: it cancels only `pending` or `blocked`
operations of the caller, writes `model_operation.changed`, and returns whether
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
- **Packing.** Requests group questions sharing exactly the same state,
  subject to verified adapter limits and local payload budgets (section 1).
  Removing cache hits must not change another question's state or wording.
  Request question ids are short (`q1…`); the model never sees them, so every
  instruction explicitly references its subject. The mapping to cache keys is stored
  with the operation (`decision_request_item(operation_id, request_key,
  state_hash, question_hash)`).
- **Ingestion** (from the hook or recovery): parse the operation's result, and
  for every answer validate it against its question: type matches, `choice`
  names a known option, `score` probabilities cover the legend, probabilities
  are finite, in [0, 1] and sum to 1 ± 0.01, `noul` in [0, 1]. Score must
  equal Σ index × probability within numeric tolerance and its legend must
  match the supplied criteria; Choice must name a maximal-probability option.
  Choice/Score confidence must be finite in [0, 1]; Noul does not require it.
  Reject unexpected answer keys. Valid answers go
  into `decision_answer` (`ON CONFLICT DO NOTHING`); invalid or missing ones are
  reported to the owner as discarded.
- Parts 8 and 10 reuse this package unchanged.

## 3. Schema (migration `00011_matching.sql`)

The tables are as in the spec, plus `model_request`, `decision_request_item`, and:

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
  `model_request` and `decision_request_item`, `SELECT` on the rest;
  `zapply_worker` read/write on run and question tables and `INSERT, SELECT`
  on `decision_answer`.

Check the migration sequence before coding: part 7 is in flight. Go-registered
migrations (6, 8) mean the next free number must be read from both
`internal/store/migrations` and `store.go`.

## 4. Matching domain (`internal/match`)

Pure, unit-tested code with no database access:

- **Posting reader.** `Lines(description) []Line`: normalize whitespace and
  bullet markers (`-`, `*`, `•`, `·`, numbered lists), split paragraphs into
  sentences, retain short qualification lines, preserve source spans and
  neighbouring headings, split lines over 400 characters where possible,
  keep 120. `YearsIn(line) *int` parses "5+ years", "at least five years",
  "3-5 years" (lower bound), "minimum of 2 yrs", in English first; other
  languages leave it empty.
- **Questions.** Builders for `LineClass`, `LineType`, `Seniority`,
  `DegreeLevel`, `LanguageLevel`, `EvidenceStrength`, `HighlightStrength` and
  `SkillValued`. Classification, type, degree and language questions for every
  line are speculative and bundled with seniority and skills in `read`.
  Degree, language and seniority choices include `not_specified`; irrelevant speculative answers are
  ignored by Go. Unknown classifications are not silently treated as `other`.
- **State and instructions.** `read` state holds the posting, source lines and
  headings. Instructions refer to backticked paths such as
  `posting.lines[3].text`. Skills are supplementary named data in their own
  questions. `score` state contains `job_context` (title/company) and one full
  evidence document with indexed highlights. A structured instruction has
  `question` and `requirement` fields: the first asks about `evidence` or
  `evidence.highlights[2]`, the second holds the source line to compare with.
  This uses TypeSafe's documented supplementary-record pattern. Content is
  never interpolated into the question sentence. Evidence state includes all
  relevant library fields; canonical encoding makes equal content hash equally.
- **Score rubric.** Both evidence and highlight strength use the four ordered,
  self-contained criteria in the spec, with indices 0–3. Preserve the API score,
  legend, probabilities and confidence; the application strength uses utilities
  [0, 0.25, 0.6, 1]. Only skill valuation uses Noul. Question keys and numbers
  in prose cannot substitute for a descriptive instruction or criterion.
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
  Unanswered pairs are unknown, not zero. Report observed coverage as
  provisional if missing answers could change it. Do not label an unknown
  requirement a gap, and suppress the aggregate score when its inputs are
  incomplete. Ignore failed speculative answers that Go does not use; only
  missing applicable answers make a result partial.

### Example scoring request

This request demonstrates the wire shape; the application builds it from an
immutable snapshot and uses the complete rubric above in production:

```json
{
  "model": "<owner-selected-model-id>",
  "state": {
    "job_context": {"title": "Backend engineer", "company": "Example"},
    "evidence": {
      "title": "Software engineer",
      "highlights": [{"text": "Built and operated a Go payments service."}]
    }
  },
  "questions": {
    "q1": {
      "type": "score",
      "instructions": {
        "question": "How strongly does `evidence.highlights[0].text` demonstrate `requirement`? Judge the supplied highlight itself.",
        "requirement": "Experience building production services in Go"
      },
      "criteria": [
        "The highlight provides no evidence of the work described in the requirement.",
        "The highlight concerns a related area but gives no concrete example of the required work.",
        "The highlight shows concrete comparable work without explicitly demonstrating the specific required work.",
        "The highlight explicitly demonstrates the specific work described in the requirement."
      ]
    }
  }
}
```

A returned distribution `{0: 0.05, 1: 0.1, 2: 0.25, 3: 0.6}` has API score
2.4 and application strength 0.775. Retain the independently returned
confidence rather than using 0.775 as confidence. JSON probability keys are
strings on the wire. Other questions in this request are evaluated independently;
none can read `q1`'s answer.

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
   `read` → build requirements (store `match_run_requirement`, seniority,
   applicable speculative levels and skill answers) → `score` (all evidence
   and highlight × requirement questions, grouped per evidence state) →
   `finalize`. No level or highlight follow-up phase. The 0.4 parent threshold
   controls which highlight answers finalize uses, not which it asks.
5. **Finalize:** run `Compute`, store `score`, `must_met`, `must_total`,
   counters and `finished_at`; status `completed`, or `partial` when an
   applicable question is `unanswered`. If `rerun_requested`, create the follow-up run in the same
   transaction.
6. **Mirror status:** a run whose in-flight operation is `blocked` or
   `needs_review` shows the same status, reason and `next_eligible_at`;
   otherwise `running`. An operation that ends `failed` after an owner
   resolution sends its questions back to `pending` as a new operation.
7. Emit `match_run.changed` whenever status, stage or counters change.

**Library reads.** At run creation the service snapshots eligible evidence
(non-archived, not `career_break`, ≤ 300) into `match_run_item` with hashes and
`facts`, and records `skills_hash` and `library_fingerprint`. Snapshot the job, skills
and complete evidence/highlight documents needed to build questions too. Build
questions from that immutable snapshot; edits during a run make it stale and
do not mix new content into historical results.

The run binds the persisted owner-selected model as in section 1; its cache
lookups use that model. Existing bound runs retain it after a setting change.

## 6. Automatic runs and supersede

`jobs.Service` gets an optional dependency:

```go
type Matcher interface {
    JobSaved(ctx context.Context, tx pgx.Tx, uid uuid.UUID, job db.Job, created, hashChanged bool) error
}
```

`CreateJob` and `UpdateJob` call it inside their transaction after the write.
`match.Service.JobSaved` does nothing unless it's a create or the hash changed,
the description is non-empty on create (or content changed on update), the
stage is `saved`, `active` or `offer`, and
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
  cache, packs the misses and sums reservations using the verified adapter
  bound and `platform.model_quote`. Before `read` answers exist, assume every
  line is a requirement (capped at 40) and score every evidence/highlight pair.
  Display this as a conservative estimate excluding retries and price changes. No quote, or one
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
- Owner-only Admin model settings: filtered decision-model picker, pricing and
  context, compatibility explanations, save with a version check, explicit
  unconfigured/unavailable states. It reuses the existing key management UI.
- Requirement text renders as plain text only.

## 11. Tests and acceptance gates

1. **Transport (fake HTTP).** Request body shape and URL; answers, `usage.cost`
   and `id` parsed; a `200` HTML body classified uncertain, never parsed;
   confirmed size rejection triggers a split; reservation bounds cover the
   adapter's documented billing expansion, including multiple questions;
   unsupported pricing or unverified bounds reject dispatch. Part 5's tests pass unchanged and the
   chat diagnostic still works end to end. `cancel_operation` racing `reserve`:
   exactly one wins.
2. **Decision cache.** Equal content hits, changed wording misses; packing
   respects verified model limits and local payload budgets and groups by state; each
   invalid answer shape is discarded; ingesting twice is a no-op.
3. **Matching domain (unit).** Line splitting on real-world postings (bullets,
   paragraphs, numbered lists); `YearsIn`; requirement rules and the 40 cap;
   `Years` across overlapping, partial and current ranges; degree and CEFR
   ordering; `Compute` golden tests on recorded answers covering expected
   strengths, recency, checks capping strength, overrides (`ignore` leaves the
   denominator), the floor, suggestions, career breaks excluded, unanswered
   pairs, deterministic ties.
4. **Orchestration (Postgres + fake provider).**
   - Cold run walks `read → score → finalize`, one request at a time;
     speculative levels and highlights add no serial phase.
   - Warm re-run: zero requests. Editing one evidence: only its questions.
     Editing description while keeping requirement lines and title/company:
     only `read` questions. Editing title/company invalidates evidence states.
     Editing a highlight invalidates its parent evidence state.
   - Discarded answers retried once, then `unanswered` and `partial`.
   - Allowance exhausted mid-run → `blocked` with reason, then resumes; no
     membership → `blocked`, proceeds after a plan is assigned.
   - Uncertain attempt → `needs_review`; after owner resolution its questions
     go out in a new operation; the lost attempt is never resent.
   - Cancel before dispatch sends nothing; after dispatch the answers are still
     cached.
   - Lost hook recovered by the scan; two workers never create duplicate
     operations for one run.
5. **Owner model settings.** Nonowners cannot list/change settings; listing
   uses the decisions filter and existing key without disclosing it; stale or
   failed metadata refresh preserves selection; unsupported models cannot
   dispatch; bound runs keep their model, new runs use the changed model;
   unconfigured runs resume and bind once; key rotation retains reconciliation.
6. **Auto-run.** Create with description → run; title edit → run; move,
   archive, notes → none; closed stage → none; `auto_match` off → none; rapid
   saves → supersede before dispatch, `rerun_requested` after; a rolled-back
   job write leaves no run and no River job.
7. **API.** Estimate with and without a quote; start idempotency and `409`
   with `existing_id`; tenant isolation on every endpoint; corrections update
   the score and emit; stale on the board after job and library edits; cost
   visible only to its owner.
8. **Web.** Store tests in the existing harness: identity change clears runs,
   late responses don't overwrite newer runs, SSE refetch, correction
   round-trip. Typecheck and production build. Browser walkthrough with the
   fake provider: save a job, watch the stages, read gaps, correct a
   requirement, edit evidence, see stale, re-run.
9. **Live check.** One manual run against `/api/v1/systemone` after deploy, by
   the owner with a plan. CI never spends credits.

Run `make generate`, generated-file drift, `make vet`, the integration suite
with `-race`, web tests, typecheck and build, and the image build.

## 12. Build order

1. The API probe (section 0), recorded here.
2. Owner model settings/catalogue and System One transport, stored requests,
   operation kinds, hooks and
   cancellation (section 1), with part 5 regressions green. Mergeable on its
   own, and what parts 8 and 10 build on.
3. `internal/decide` and its tables.
4. Migration, sqlc queries, `internal/match` domain and unit tests.
5. Orchestration, recovery and worker grants.
6. API, OpenAPI and the auto-run hook.
7. Web.
8. Docs: record delivered behavior in `06-matching.md`, the part 5 note on
   reservations and owner configuration, README.

## Deploying

Apply the new migrations with `make migrate` before rolling out API and worker.
Choose the decision model in Admin using the existing OpenRouter key.
Existing diagnostic operations keep working
across the deploy: the worker accepts the previous persisted River kind, and
an operation without a stored request is a diagnostic. Without an owner-selected decision model, runs wait as
`blocked` with `model_not_configured` and the rest of the app is unaffected.

The [spec's design references](06-matching.md#design-references) document the
question semantics. Live transport/billing probes remain unperformed until
recorded here; nothing in this plan claims a successful paid verification.
