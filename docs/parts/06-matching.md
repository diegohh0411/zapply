# Part 6: Matching pipeline

Status: scope agreed; not built yet. The [implementation plan](06-matching-plan.md)
records the proposed design.

The library (part 2) holds what a person can claim, and a job (part 4) holds
what an employer asks for. Matching reads both and answers four questions:
**what does this job ask for, which of my evidence shows it, what is missing,
and how close is what I have?** Rendering (part 7) uses the answer to choose
what goes on the CV. Every paid model call goes through the part 5 gateway, so
allowances, reservations and reconciliation already apply.

## Decision models, not chat models

Matching uses **System One decision models** (for example TypeSafe's Jev or
Microsoft Decision-1) through OpenRouter's System One API
(`POST https://openrouter.ai/api/v1/systemone`), the same route cvd used. A
decision model doesn't write text. It reads a **state** and answers typed
**questions** about it:

| Type | Question | Answer |
|---|---|---|
| `noul` | Yes or no | The probability of yes |
| `choice` | One of named options | The chosen option and a probability for each |
| `score` | A level on an ordered scale | The expected level and a probability for each |

That shapes the whole design:

- **Go owns the workflow.** Go decides what to ask, splits the work into
  requests, and computes every result from the answers.
- **Answers are calibrated.** Each answer is a probability distribution, so
  scores are expected values with a confidence, not a single label.
- **Nothing is invented.** The model can only pick among options we wrote, so it
  can't make up a requirement, a citation or a claim. Requirements are lines
  quoted from the posting; explanations are the user's own highlights.
- **It's cheap.** Decision models charge for input only, at about $0.04 per
  million tokens. A full match costs fractions of a cent, so asking many small
  questions is affordable.
- **Prompt injection has little to grab.** A manipulated posting can shift
  probabilities, but it can't make the model output instructions, ids or text.

## What changes from cvd

cvd asked one question per entry and bullet against the **whole** posting ("how
relevant is this entry?" on a 0–4 scale), whether the role values each skill,
the role's seniority and family, and 24 fixed focus tags. Its renderer dropped
anything below a relevance floor of 1.5.

| cvd | zapply | Reason |
|---|---|---|
| Relevance of an entry to the whole posting | Strength of an evidence against **each requirement** | A whole-posting score can't say what's missing. Per-requirement answers give gaps, coverage and citations |
| No requirements | Requirements are the posting's own lines, classified by the model | Gaps need a list of what's asked, and quoting the posting means nothing is invented |
| One local engine, runs in-process | The part 5 gateway, River jobs, cached answers | Survives deploys; re-matching pays only for new questions |
| Scores per run | Answers cached per question by content hash | Re-matching costs only what changed |
| The model judged seniority fit | Go computes years, levels and recency from the library's fields | These are arithmetic and comparisons, not judgment |
| 24 software focus tags | Dropped | zapply serves any career; requirements carry the focus |

Kept from cvd: the question types and their calibrated answers, scoring
highlights as well as evidence, the "does this role value this skill?"
question, the seniority question, labelling untrusted text as data, and a
relevance floor for what goes on a CV.

## Principles

- **Matching never writes into the library.** It reads evidence and highlights
  and produces scores, citations and a ranked suggestion.
- **The model judges, code decides.** The model answers narrow questions. Go
  computes every score, rank, coverage figure and selection from the answers,
  deterministically.
- **Code computes what code can.** Years of experience, degree levels, language
  levels and recency come from the library's structured fields.
- **Every claim is explained.** Each match names the requirement (quoted from
  the posting), the evidence and highlights that show it, and how confident the
  model is.
- **Pay once per question.** Answers are cached by the content they were asked
  about. Re-matching an unchanged job, or matching after editing one item, pays
  only for new questions.
- **Model calls are approved in one place.** Anything can be queued, but no
  provider call is made unless the part 5 gateway reserves allowance for it
  first (see "Model access").

## Pipeline

A **match run** scores the user's library against one job. Go builds the
questions; the gateway sends them in requests of up to 128 questions (or the
model's own limit) within its input budget. Requests run one at a time.

| Stage | Questions | State |
|---|---|---|
| 1. `read` | One `choice` per posting line, plus the role's seniority | The posting |
| 2. `level` | Degree or language level for requirements that need one | The posting |
| 3. `evidence` | One `score` per (evidence, requirement); one `noul` per skill | The job's title and company |
| 4. `highlights` | One `noul` per (highlight, requirement) where its evidence matched | The job's title and company |
| 5. `finalize` | None | — |

A stage with every answer cached sends nothing. A run with everything cached
makes no calls and finishes in one transaction.

### 1. Read the posting

Go splits the description into **lines**: line breaks and bullet markers first,
then sentences within long paragraphs. Lines under three words are dropped, and
lines over 400 characters are split at sentence boundaries. The first 120
lines are kept; a longer posting is marked `truncated`.

Each line gets one `choice` question, "What is this line of the job posting?":

| Option | Meaning | Becomes |
|---|---|---|
| `must` | A requirement the candidate must meet | Requirement, weight 2 |
| `preferred` | A preferred or bonus qualification | Requirement, weight 1 |
| `duty` | A responsibility of the role | Requirement, weight 1 |
| `other` | About the company, benefits, process or anything else | Ignored |

and a second `choice`, "What does this line ask for?": `skill`, `experience`,
`education`, `credential`, `language` or `other`. One `choice` gives the
seniority: `entry`, `junior`, `mid`, `senior`, `lead`, `executive`.

- A line becomes a **requirement** when its most likely class is `must`,
  `preferred` or `duty`. Its text is the line, quoted verbatim.
- At most 40 requirements are kept, `must` first, then in posting order.
  Exact duplicates (after normalizing case and spacing) are merged.
- **Years** come from the text, not the model: Go reads patterns like "5+
  years", "at least three years" and "3–5 years" into `min_years`.
- A job with an empty description is matched on its title as one requirement.
  A job with no requirements found completes with no score.

### 2. Levels

For requirements typed `education`, a `choice` over `none`, `secondary`,
`associate`, `bachelor`, `master`, `doctorate` asks which degree it requires.
For `language`, a `choice` over the CEFR levels asks which level it requires.
Skipped when there are no such requirements.

### 3. Score evidence and skills

The state is the job's title and company. For every eligible evidence and every
requirement, one `score` question:

> How strongly does `evidence` show `requirement`?
> 0. Not at all
> 1. **Adjacent**: related, but would not convince a reader on its own
> 2. **Transferable**: a defensible substitute a reviewer would accept
> 3. **Direct**: the evidence itself shows it

The evidence is sent as data inside the question: kind, category, title,
organization, dates, summary, details, skills and highlights. Keeping the
posting out of the state means an answer stays valid when the description
changes but the requirement line doesn't.

Each answer is a distribution over the four levels. Go turns it into an
**expected strength** with direct = 1.0, transferable = 0.6, adjacent = 0.25,
none = 0, and keeps the probabilities for display.

For each skill in the user's list, one `noul` with the posting as state: "Does
this role require or value the skill `X`?" Skills never count toward
coverage: a skill on its own proves no experience.

Which evidence is scored:

- All non-archived evidence, top-level and children. Children are scored on
  their own, because a thesis or a client assignment can be the strongest
  match.
- **Career breaks are never sent.** Part 2 decided matching never adds one.
- Evidence with `auto_include = false` is scored, and can show up as a
  suggestion, but is never selected (see "Selection").
- At most 300 evidence documents per run, chosen in library order. More fails
  the run with a clear reason.

### 4. Score highlights

Highlights are scored only where it matters: for each (evidence, requirement)
pair whose expected strength is at least 0.4, one `noul` per highlight of that
evidence: "Does `highlight` show `requirement`?" The answers pick the
highlights that prove each match.

### 5. Finalize (no model)

Go turns the answers into the result. The constants below have a version
(`scoring_version`). Changing them is a code change and never needs a call.

**Deterministic checks.** A requirement with `min_years` or a level gets a
check on top of the model's strength:

- **Years:** the union of the date ranges of evidence whose probability of
  `direct` or `transferable` is at least 0.5, overlapping periods merged,
  current roles counted to the run's date, partial dates counted from the start
  of their period. "3.5 of 5 years" caps the requirement's strength at 0.6.
- **Degree level:** `degree_program.degree_type` is compared on the order
  secondary < associate < bachelor < master < doctorate. Only completed degrees
  count; `in_progress` is reported as such.
- **Language level:** the matched language skill's CEFR proficiency (or
  `native`) is compared with the required level.

**Recency.** A match's weight decays with the time since its evidence ended:
full weight for current evidence or evidence that ended within 3 years, then
down 10% a year to a floor of 50%.

**Scores.**

- **Evidence relevance** = Σ over requirements of expected strength × recency ×
  requirement weight. It ranks evidence.
- **Highlight relevance** = Σ over requirements of P(yes) × requirement weight.
  It ranks highlights within their evidence; unscored highlights rank last, in
  library order.
- **Skill relevance** = P(role values it) × 2, plus a share of the relevance of
  the evidence it's linked to.
- **Requirement coverage** is the best checked strength any eligible evidence
  gives it: `met` (≥ 0.8), `partial` (≥ 0.5), `adjacent` (≥ 0.2) or `gap`.
- **Job score** (0–100) = Σ requirement weight × best checked strength ×
  recency, divided by Σ requirement weight. The run also reports "must-haves
  met: 4 of 6" (counting `met` and `partial`).
- **Confidence.** Each coverage shows the probability behind it ("direct,
  82%"). Requirements where the best answer is uncertain (no level above 0.5)
  are flagged so the user can check them.

## Insights and user corrections

From the same answers, at no extra cost:

- **Gaps:** must-haves with no match, listed first.
- **Near misses:** requirements met only at `adjacent`, or short on years or
  level, with the evidence that came close and what's short ("3.5 of 5 years";
  "master's required, bachelor's completed").
- **Hidden evidence:** a gap or near miss that evidence the user keeps out of
  CVs would cover.
- **Library prompts:** for each gap, a link to add evidence or a highlight in
  the library. Matching never writes it; the user decides whether it's true.
- **Seniority fit:** the posting's seniority beside the user's years of
  relevant experience.

The user can correct how the posting was read, which is where most errors start:

- Change a requirement's class (must, preferred, duty), or **ignore** it.
- Corrections are stored per job, keyed by the requirement's text. Finalize is
  deterministic, so a correction updates the score immediately with no call. A
  correction whose line disappears from the posting no longer applies, and the
  UI says so.

## Selection: the contract with part 7

Part 7 is being built at the same time, so this is the boundary between them.

- **Part 6 suggests; part 7 decides.** A completed run exposes a `selection`:
  every eligible evidence with relevance above the floor and
  `auto_include = true`, in rank order, each with its highlights in rank order,
  plus the skills in rank order. Evidence with `auto_include = false` that
  matched is listed separately as `suggestions`, never in `selection`.
- **A floor, as in cvd.** Evidence whose best expected strength is below 0.4
  is not selected. The floor is part of `scoring_version`.
- **The selection is ranked, not trimmed.** Part 7 fits a CV to pages, so it
  keeps items from the top until the page is full.
- **Part 6 stores no user picks.** Pinning and excluding items belong to the
  CV document in part 7, seeded from the selection. Re-running a match never
  overwrites a user's picks.
- **Part 7 must work without part 6.** With no completed run for a job,
  rendering falls back to `auto_include`.
- Shape of `GET /v1/match-runs/{id}` → selection fields (ids only; part 7
  loads the content from the library):

  ```
  selection:   [ { evidence_id, relevance, highlight_ids: [ranked...] } ]
  skills:      [ { skill_id, relevance } ]
  suggestions: [ { evidence_id, relevance } ]
  ```

## Model access

Work can always be queued. **A call to OpenRouter is made only when the
member's allowance covers it**, and that decision is made in one place: the
part 5 gateway's reservation (`platform.reserve`), immediately before each
call. It already checks, atomically and in this order, that the member has a
membership, that it isn't suspended, that one call's worst case fits the plan
(`request_exceeds_plan`), and that both rolling allowances cover it after
existing holds (`usage_limit`).

- Matching never calls a provider directly. Every request is a part 5
  operation, so the same gate applies to automatic and manual runs, and will
  apply to parts 7, 8 and 10 the same way.
- A run whose next request is refused waits as `blocked` with the gateway's
  reason and, for `usage_limit`, when capacity returns. It resumes on its own,
  including after the owner assigns a plan or lifts a suspension.
- Queuing checks nothing about budget. `POST /jobs/{id}/match-runs` returns
  `202`, and the run reports the reason if it can't proceed.

## Running automatically

A match runs on its own when a job is saved:

- **When:** a job is created with a description, or its `content_hash` changes.
  The run is queued in the same transaction as the job write, so it starts
  immediately and is never queued for a rolled-back write.
- **Which jobs:** only jobs in `saved`, `active` or `offer` stages.
- **Who:** any member with `auto_match` on. Members without a plan get a run
  that waits as `blocked` with the gateway's reason, and proceeds once a plan is
  assigned. No call is made until the allowance covers it.
- **Superseded runs:** a new automatic run replaces one that hasn't dispatched
  a request yet. A run that has dispatched finishes, and its answers are cached.
- **Library edits don't start runs.** One edit would otherwise re-score that
  item against every saved job. The jobs show as stale, with a one-click re-run.
- **A setting:** `auto_match` on the profile, on by default.

Manual runs work the same way, from the Match tab.

## Staleness and re-running

A run records the job hash, the skills-list hash and the hash of every evidence
it read. It becomes **stale** when any of them changes, or when evidence is
archived, deleted or added. Staleness is computed on read by comparing hashes,
and the UI shows how many items changed.

Re-running creates a new run and asks only questions whose answers aren't
cached. Editing one evidence re-asks its questions; editing the description
re-reads the posting, but every unchanged requirement line keeps its scores.
Runs are kept as history; the latest completed run is the job's current match.

There is no "force re-score" in the MVP. A new model or question wording
invalidates the cache naturally, because both are part of the key. A new
`scoring_version` needs no re-run at all.

## Failures, budget and cancellation

- **Budget.** Each request is one part 5 `llm_operation`. When an allowance
  can't cover the next request, the run shows `blocked` with the gateway's
  reason and `next_eligible_at`, and resumes on its own. Answers already paid
  for stay cached.
- **Incomplete answers.** Answers are checked against the questions sent:
  missing answers, unknown options or malformed probabilities are discarded.
  Discarded questions are asked once more in a later request; if they fail
  again, they're marked `unanswered` and the run completes as `partial`.
  Finalize treats an unanswered pair as unknown, not as "not at all".
- **Uncertain calls.** An uncertain part 5 attempt is never resent. The run
  waits in `needs_review`. Once the owner resolves the attempt, its questions
  are asked again in a **new** operation with its own reservation. The lost
  attempt is never resent; a decision request has no effect beyond its charge,
  so asking again is safe.
- **Cancel.** Cancelling stops requests that haven't dispatched. A request
  already sent finishes and settles, and its answers are still cached. The run
  ends `cancelled`.
- **One active run per job.** Starting another manually while one is active
  returns `409` with the active run's id.

## Estimate before running

The estimate counts uncached questions, packs them into requests as the run
would, and sums each request's reservation. Because decision models charge for
input only and the reservation is bounded by the request's size, the estimate
is close to the real cost. The UI compares it with the member's available
allowance.

## Untrusted text

A job description is untrusted text, and so is the library from the model's
point of view.

- The posting and the library are always sent as labelled data (the state, or
  a field inside a question), never as the question's instruction.
- The model can only answer with the options we defined, so a manipulated
  posting can at most skew probabilities, and the cited highlights show what
  each score rests on.
- Requirement text is displayed as plain text, never as HTML or Markdown.
- Library content goes only to the configured provider, through the gateway,
  on the member's own allowance. Nothing is shared between tenants.

## Schema (migration `00010_matching.sql`)

```
decision_answer         user_id · model · state_hash · question_hash
                        answer jsonb · operation_id · created_at
                        PK (user_id, model, state_hash, question_hash)

match_run               id · user_id · job_id · job_hash · skills_hash
                        library_fingerprint · model · question_version
                        scoring_version · trigger (manual, auto)
                        status · stage · truncated · seniority?
                        score? · must_met · must_total
                        questions_total · questions_cached · questions_asked
                        questions_unanswered · rerun_requested
                        error_code · version · created_at · started_at · finished_at

match_run_requirement   user_id · run_id · position · text · class · type
                        min_years? · level? · text_hash

match_run_item          user_id · run_id · evidence_id · evidence_hash · facts jsonb

match_run_operation     user_id · run_id · operation_id · stage

match_override          user_id · job_id · text_hash
                        class (must, preferred, duty, ignore) · updated_at
                        PK (user_id, job_id, text_hash)

profile                 + auto_match boolean NOT NULL DEFAULT true
```

- `decision_answer` is the **generic decision cache**, not matching-specific:
  parts 8 and 10 reuse it. `state_hash` and `question_hash` are SHA-256 over
  the canonical JSON actually sent, so the key changes whenever the content or
  the wording does.
- `facts` snapshots the structured fields finalize uses (dates, degree type and
  status, `auto_include`, highlight hashes in order), so a run's result stays
  stable as the library changes; staleness tells the user to re-run.
- Tenancy as in parts 2 and 4: `user_id` on every row, forced RLS with the
  `tenant` policy for `zapply_app` and `zapply_worker`, composite tenant FKs.
- `job_id` cascades on job deletion. `evidence_id` has no FK: history survives
  deleting evidence, and deleted items are hidden from results.
- Caches are per user. A global cache for posting reads (the same posting saved
  by many users) is possible later, but would mean one member's paid call
  serving another.

## API (`/v1`)

| Endpoint | Notes |
|---|---|
| `GET /jobs/{id}/match-estimate` | Uncached questions, requests needed, estimated cost, available allowance |
| `POST /jobs/{id}/match-runs` | Starts a run. `Idempotency-Key` required. `202` with the run; `409` with `existing_id` while one is active |
| `GET /jobs/{id}/match-runs` | The job's runs, newest first. Cursor pagination |
| `GET /match-runs/{id}` | The run: status, progress, seniority, requirements with coverage, checks, confidence and citations, ranked evidence, insights, `selection`, `skills`, `suggestions`, staleness, and the run's settled cost |
| `POST /match-runs/{id}/cancel` | Idempotent |
| `PUT /jobs/{id}/match-overrides/{text_hash}` | `{class: must, preferred, duty or ignore}`; `DELETE` restores the model's reading |

- Job summaries (`GET /jobs`) gain `match: {run_id, score, status, stale}` for
  the latest run, so the board can show a badge without a request per card.
- `GET, PUT /profile` gain `auto_match`.
- Errors follow part 2. Budget reasons reuse part 5's problem codes.
- **Events:** `match_run.changed` with `{id, job_id, version}`, emitted on every
  status or progress change and on corrections.

## Web

- **Job sheet → Match tab.** Before a run: the estimate and a Match button (or
  the automatic run's status). During: the stage and questions answered of
  total, and the blocked reason with when it resumes. After:
  - The job score, must-haves met, and seniority fit.
  - Gaps and near misses first, each with a link to add evidence.
  - Requirements as quoted from the posting, grouped must, preferred, duty,
    each marked met, partial, adjacent or gap with its confidence and checks
    ("3.5 of 5 years"), the evidence and highlights that show it, and a class
    control (must, preferred, duty, ignore).
  - Ranked evidence, each with the requirements it shows; ranked skills.
  - Suggestions: matching evidence the user keeps out of CVs.
  - A stale banner ("3 items changed since this match") with a Re-run button.
- **Board and table:** a score badge on each matched job, dimmed when stale.
  A table column sorts by score.
- **Settings:** the `auto_match` toggle.
- Live updates use the existing SSE invalidation.

## Gateway changes needed

Part 5 shipped a chat-completions path for its diagnostic only. Matching needs:

- **A System One transport.** `POST https://openrouter.ai/api/v1/systemone`
  with `model`, `state` and `questions`; answers and `usage.cost` parsed from
  the response, with its `id` kept for reconciliation. Not the alpha
  `/api/alpha/decisions` route, and not `https://openrouter.ai/v1/systemone`
  (without `/api`), which answers `200` with an HTML page.
- **Decision model configuration and quotes.** A separately configured
  decision model (`DECISION_MODEL`), priced from OpenRouter's model listing
  (`output_modalities=decisions`). Reservations are bounded by the request's
  size instead of a full context window.
- **Stored requests.** The exact state and questions are stored with the
  operation, so a retry sends the same request.
- **The decision cache** above, so every feature that asks questions pays once
  per question.

## Out of scope

- Generated text: tailored highlights, summaries, cover letters, prose
  explanations. Decision models don't write, and matching cites what the user
  wrote.
- Matching across many jobs at once, and job discovery.
- Embeddings and vector search.
- Evaluation. The MVP trusts the configured model; a labelled evaluation set
  comes later, if the app gets traction. CI still checks question building,
  answer handling and scoring against recorded answers, with no paid call.
- Model choice per user or per plan, and a forced re-score.
- Fetching a posting from its URL (part 11 sends the text it sees).
- Company research as a matching input (part 8). cvd prefixed the company
  profile to the posting state; part 8 can do the same.

## Decisions

- **Models:** System One decision models through OpenRouter's System One API
  (`/api/v1/systemone`).
  No chat model is needed for matching.
- **Requirements:** the posting's own lines, classified must, preferred, duty
  or other; years parsed by Go; levels asked as choices.
- **Scale:** a four-level `score` (none, adjacent, transferable, direct) read
  as a distribution, plus deterministic checks and user corrections.
- **Accuracy:** the MVP trusts the configured model; evaluation comes later.
- **Automatic runs:** yes, immediately on job create and job content change,
  with an opt-out.
- **Model access:** queuing is free; every provider call needs a part 5
  reservation first.
- **Skills:** ranked by part 6, delivered in the selection.
- **Recency and floor:** 3 years at full weight, then 10% a year down to 50%;
  evidence below 0.4 expected strength is not selected.

## Open questions

1. **Migration number.** Part 7 is being built at the same time. This spec
   takes `00010`; part 7 should take the next free number, and whichever merges
   second renumbers if needed.
2. **System One details to verify before building:** the per-request question
   limit for each model (cvd used 128), the response's `usage` and `id` fields,
   whether provider routing, `max_price` and generation lookup work for these
   requests, and each model's input context. The API reference doesn't state
   them.
   The plan falls back safely if they don't (see the plan's gateway section).
