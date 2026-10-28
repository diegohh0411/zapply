# Billing prediction plan

Status: implemented for [#20](https://github.com/diegohh0411/zapply/issues/20).
See the implementation record at the end for where the build differs from the
design.

## Problem

A decision reservation today is `context × prompt + request fee × questions`,
computed in `openrouter.QuoteDecision` and recomputed from the stored quote in
`match.Service.Estimate`. It is wrong in both directions:

- **Under-reserves.** Since #16, only `prompt` and `request` are read. A model
  priced through `input_cache_read`, `completion` or anything else reserves
  little or nothing. `prompt = 0` with a positive `input_cache_read` and no
  request fee reserves $0. With 8 calls in flight per member (#19), several
  under-reserved calls can pass `platform.reserve` at once, and the worker only
  logs the overrun after the money is spent.
- **Over-reserves.** On `~liquid/d1-ultra-latest` a 62 KB, 96-question request
  settled 12,705 input tokens for $0.00053. The context bound is a ceiling, not
  a prediction, so the allowance locks up far more than calls cost.

The formula also lives in two places, so `match-estimate` and the worker can
drift.

## Design

Split a quote into a **provider-specific price sheet** and a
**provider-neutral prediction**.

```
             provider-specific                 provider-neutral
 catalogue ──► billing.Sheet ──┐
                                ├─► billing.Predict(sheet, shape, calib) ──► Prediction
 request ───► billing.Shape ───┤                                             (expected,
 settlements ► billing.Calibration ┘                                          reserved,
                                                                               ceiling)
```

The provider's job is to say what each priced unit costs and how large a call
can get. Turning a request into tokens and tokens into money, with a margin
learned from settlements, is the same for every provider and lives in one
place. The worker and `Estimate` both call `billing.Predict`, so they cannot
disagree.

### New package `internal/model/billing`

```go
// Sheet is what a provider knows about a model's billing, captured at quote
// time and stored in the quote so estimates can predict without the network.
type Sheet struct {
	Provider string // "openrouter"
	Model    string

	// Rates in picodollars. Each is the highest rate among the components
	// that can apply to that unit, so the sheet never prices below reality.
	InputRate   int64 // per input token
	OutputRate  int64 // per output token
	RequestRate int64 // per call, or per question when RequestPerQuestion

	RequestPerQuestion bool
	InputPerQuestion   bool // state re-billed for each question

	Context   int // hard cap on billable input tokens per evaluation
	MaxOutput int // hard cap on billable output tokens per question

	Prior      Tokenization // used until calibration has enough samples
	Components []Component  // every catalogue component and how it was handled
	FetchedAt  time.Time
}

type Tokenization struct {
	TokensPerKB    int // input tokens per 1,000 request bytes, conservative
	OverheadTokens int // template and system prompt tokens per call
	OutputPerQ     int // output tokens per question
}

type Component struct {
	Name string // provider key, e.g. "input_cache_read"
	Rate string // decimal as reported
	Role string // "input" | "output" | "request" | "unused" | "ignored_zero" | "discount"
}

// Shape is what is known about a call before it is sent.
type Shape struct {
	RequestBytes int
	Questions    int
}

// Calibration summarises recent settlements for one model.
type Calibration struct {
	Samples        int
	TokensPerKB    int     // high quantile of settled input tokens per KB
	OverheadTokens int
	OutputPerQ     int
	CostRatio      big.Rat // high quantile of settled / predicted cost
	Exceeded       bool    // a recent settlement went over its reservation
}

type Prediction struct {
	InputTokens  int
	OutputTokens int
	Expected     int64  // predicted cost, no margin
	Reserved     int64  // min(Ceiling, Expected × margin)
	Ceiling      int64  // Context × InputRate + MaxOutput × OutputRate + fees
	Basis        string // "prior" | "calibrated" | "ceiling"
}

func Predict(s Sheet, shape Shape, c Calibration) (Prediction, error)
```

All arithmetic stays in integer picodollars via `big.Int`/`big.Rat`, as
`model.QuoteCost` does today. `QuoteCost` and `checkedMul` move into this
package.

### Provider seam

```go
// internal/model/provider.go
type DecisionProvider interface {
	DecisionSheet(ctx context.Context, model string) (billing.Sheet, error)
	Decide(context.Context, string, systemone.Request, Quote) (Result, error)
	DecisionLookupSupported(string) bool
	DecisionUsage(json.RawMessage) (billing.Usage, error) // input/output tokens from settled usage
}
```

`QuoteDecision` becomes a provider-neutral function in `internal/model`:

```go
func QuoteDecision(ctx context.Context, p DecisionProvider, r systemone.Request, c billing.Calibration) (Quote, error)
```

It fetches the sheet, measures the shape from `systemone.Canonical(r)`, calls
`billing.Predict`, enforces the request-size limits, and returns a `Quote`
that embeds the `Sheet` and `Prediction`. `Decide` keeps revalidating against
a fresh quote before dispatch, now comparing fresh `Reserved` with the held one.

A future provider (direct Anthropic, a local model) implements
`DecisionSheet` and `DecisionUsage`; it gets prediction, calibration and the
estimate for free. If a provider ever bills in a way a sheet cannot express
(tiered context pricing, per-image charges), it rejects the model from
`DecisionSheet` with a reason rather than bending the shared math. Adding an
optional `Predictor` override is possible later but not needed now.

Chat LLMs use the same sheet and prediction; see
[Chat LLMs](#chat-llms) below.

### OpenRouter sheet

`openrouter.DecisionSheet` classifies every key in the catalogue's `pricing`
object. Each component either feeds a rate or makes the model incompatible:

| Component | Role | Notes |
| --- | --- | --- |
| `prompt` | input | missing counts as 0, not an error |
| `input_cache_read`, `input_cache_write` | input | `InputRate` is the max of all input components |
| `completion`, `internal_reasoning` | incompatible if positive | see the implementation record: System One takes no output limit |
| `request` | request | per question, per `ownerSelectedBound` |
| `discount` | discount | never lowers a rate; recorded only |
| `audio`, `input_audio`, `input_audio_cache`, `output_audio`, `image`, `image_output` | unused | decisions send text and return JSON, so these bill nothing; recorded, not priced. Lets audio-capable decision models (e.g. Liquid's newest) load. The cost ratio catches it if one does bill |
| `web_search` | incompatible if positive | a per-call charge we cannot bound |
| anything else | incompatible if positive | `unbounded_pricing:<key>` |

Zero-valued components are recorded as `ignored_zero`. An unparsable rate is
`invalid_pricing`. This narrows #16 deliberately: #16 dropped the allowlist so
models with harmless extra components would load; the table keeps those
loading and only rejects positive components the formula cannot price. The
picker already shows `compatibility_reason`.

Bounds:

- `Context` = catalogue `context_length` (unchanged).
- `MaxOutput` per question = output tokens that fit in the answer budget,
  derived from `systemone.ResultBudget` at 1 token per byte, divided across
  questions. Until a model reports `completion > 0` this is moot; the fixture
  covers it.
- `InputPerQuestion` stays false: settled usage bills input once per request.

Priors (no calibration yet) are deliberately pessimistic but far below the
context ceiling: 1,000 tokens per KB (one token per byte; the d1 sample is
about 205), 1,024 overhead tokens, 16 output tokens per question.

### Prediction

```
input   = min(Context, overhead + bytes × tokensPerKB / 1000) × (questions if InputPerQuestion)
output  = outputPerQ × questions
expected = input × InputRate + output × OutputRate + RequestRate × (questions or 1)
ceiling  = Context × InputRate × evals + MaxOutput × OutputRate × questions + fees
margin   = max(1.25, CostRatio × 1.10)
reserved = min(ceiling, ceil(expected × margin))
```

- Request fees are exact, so they skip the margin.
- `Basis = "ceiling"` and `reserved = ceiling` when the model has fewer than
  5 samples *and* no prior is configured, or when `Calibration.Exceeded` is
  set. A model that overran falls back to the ceiling until 20 clean
  settlements arrive after the overrun.
- `reserved` is never below `expected`, and never 0 when any component is
  positive.

### Calibration

Everything needed is already stored:

- `platform.llm_attempt.quote` holds the quote JSON. It gains
  `shape.request_bytes`, `shape.questions` and the `prediction`.
- `platform.model_usage.usage` holds the provider's settled usage
  (`input_tokens`, `output_tokens`, `cost`).

Migration `00015_model_calibration.sql` adds:

```sql
-- One row per settled decision attempt with both shape and usage.
CREATE VIEW platform.decision_settlement AS
SELECT a.model, a.dispatched_at,
       (a.quote->'shape'->>'request_bytes')::int AS request_bytes,
       (a.quote->'shape'->>'questions')::int     AS questions,
       (a.quote->'prediction'->>'expected')::bigint AS expected,
       a.reserved, u.amount,
       (u.usage->>'input_tokens')::int  AS input_tokens,
       (u.usage->>'output_tokens')::int AS output_tokens
FROM platform.llm_attempt a JOIN platform.model_usage u ON u.attempt_id = a.id
WHERE a.quote ? 'shape' AND u.amount > 0;

CREATE FUNCTION platform.model_calibration(p_model text) RETURNS jsonb ...
-- Last 200 settlements for the model within 30 days: p95 tokens per KB,
-- p95 output per question, p95 amount/expected, sample count, and whether any
-- settlement after the latest 20 exceeded its reservation.
```

Settlements are cross-tenant, so the function is `SECURITY DEFINER` and only
returns aggregates. The token keys live in `DecisionUsage` so a provider with
different field names maps them once.

The worker calls `platform.model_calibration` in the same transaction that
reads the attempt history, then passes it to `QuoteDecision`. `Estimate` does
the same and calls `billing.Predict` per packed request with the stored sheet,
replacing the inline loop in `service.go`.

Older attempts without `shape` are ignored. Their byte counts were never
recorded, so they cannot calibrate.

### Overruns

Prediction can be wrong, so the existing safety nets stay and get one more:

- `platform.settle` already writes a `quote.exceeded` audit event, and the
  worker logs it.
- `Calibration.Exceeded` forces the ceiling for that model, so a single overrun
  stops further under-reservation within one quote cycle.
- Members with 8 calls in flight are each bounded by their own reservation,
  and every reservation is at least `expected × 1.25`.

## Steps

1. **`internal/model/billing`.** `Sheet`, `Shape`, `Calibration`,
   `Prediction`, `Predict`, and the moved integer helpers. Table tests for
   the formula, ceiling cap, margin, zero rates and overflow.
2. **OpenRouter sheet.** `DecisionSheet` with the component table, and
   `DecisionUsage`. Fixtures required by #20:
   - cache read pricier than prompt → `InputRate` = cache read rate;
   - missing `prompt`, positive `input_cache_read`, no request fee → reserved > 0;
   - positive `web_search` (or an unknown key) → `unbounded_pricing:web_search`;
   - positive `completion` → incompatible (changed after review).
3. **Provider-neutral `QuoteDecision`.** Move quoting out of the OpenRouter
   client, embed sheet, shape and prediction in `Quote`, and keep the
   size checks and revalidation in `Decide`. Drop `PerQuestion`,
   `RequestFeePerQuestion`, `InputRate`, `RequestRate` and `MaxInput` from the
   decision side of `Quote` once nothing reads them; `BillingBound` folds
   into the sheet.
4. **Calibration migration and worker wiring.** Migration 00015, the
   `model_calibration` query, and passing it into the quote. sqlc regen.
5. **`match-estimate`.** Replace the inline loop with `billing.Predict` per
   packed request; same calibration call.
6. **Replay test.** Record real d1 settlements (shape + usage + cost) as a
   fixture and assert, for every sample, `settled ≤ reserved ≤ 3 × settled`
   once calibrated, and `settled ≤ reserved` under the prior. That is the
   "stated margin" for #20's acceptance.
7. **Docs.** Update part 5 and part 6 where they describe the context-bound
   formula, and the #16 note about unfiltered pricing.

Steps 1–3 can land together without the migration: with zero samples every
quote uses the prior, which already fixes the $0 reservation and cuts the
over-reservation by about 5× on d1. Steps 4–6 add the learned margin.

## Decisions

- **Priors** stay conservative (one token per byte). These models are cheap;
  a reservation that is right most of the time is good enough.
- **Calibration** is global per model.
- **Unpinned upstream pricing** is accepted. The cost ratio and the ceiling
  fallback correct it after the fact.

## Chat LLMs

OpenRouter chat models, for new features, share the sheet and prediction.
The one difference from decision models is output, and it follows the pattern
LLM gateways use (LiteLLM, OpenRouter's own budget holds): reserve output at
the `max_tokens` the call sends, because the provider enforces it.

- **`Sheet.OutputEnforced`.** True when the endpoint stops billing output at
  a cap. `Predict` refuses a positive `OutputRate` without it
  (`billing.ErrUnboundedOutput`). Chat sheets set it; decision sheets don't,
  so positive output pricing still makes a decision model incompatible.
- **Output is reserved at the cap.** `Shape.MaxOutput` is the call's
  `max_tokens`, bounded by the endpoint's `max_completion_tokens`. Input is
  predicted with the margin; output is held in full, since `max_tokens`
  bounds it exactly; fees are exact. Expected output (for the estimate and the
  cost ratio) is the prior or calibrated tokens per call.
- **Reasoning.** OpenRouter bills reasoning as output and counts it toward
  `max_tokens`, so `internal_reasoning` is an output component and the output
  rate is the higher of it and `completion`.
- **`model.ChatPricer` / `model.QuoteChat`.** A provider implements
  `ChatSheet(ctx, model, endpoint)`; `QuoteChat` measures the messages, calls
  `Predict`, and returns rate strings for OpenRouter's `max_price`, which
  stops routing to a pricier upstream. `openrouter.ChatSheet` only accepts
  endpoints whose `supported_parameters` include `max_tokens`. The diagnostic
  `Quote` now goes through it.
- **One component table.** `openrouter/pricing.go` classifies pricing keys for
  both endpoint kinds. Audio and image components are unused because calls
  are text only; a multimodal feature must price them before sending media.
- **Calibration.** `platform.model_calibration` reads `prompt_tokens` and
  `completion_tokens` as well as `input_tokens` and `output_tokens`. A new chat
  feature passes the model's calibration to `QuoteChat`; the diagnostic uses
  the prior.

### Tokenizers

No tokenizer dependency for now. OpenRouter serves many model families with
different tokenizers, and it has no pre-request token-count endpoint. Go
options checked on 2026-10-27:

| Library | Covers | Notes |
| --- | --- | --- |
| [`tiktoken-go/tokenizer`](https://github.com/tiktoken-go/tokenizer) | OpenAI encodings | pure Go, vocabularies embedded; v0.10.0 released 2026-10-21 |
| [`pkoukk/tiktoken-go`](https://github.com/pkoukk/tiktoken-go) | OpenAI encodings | pure Go, most used; last release v0.1.8 (2025-09) |
| [`daulet/tokenizers`](https://github.com/daulet/tokenizers) | any Hugging Face `tokenizer.json` | cgo bindings to the Rust library; v1.27.0 (2026-03) |
| [`sugarme/tokenizer`](https://github.com/sugarme/tokenizer) | Hugging Face formats | pure Go; last release v0.3.0 (2025-09) |

A tokenizer is exact only for its own model family. A Claude, Gemini or Liquid
model needs its own tokenizer files, or a provider count endpoint such as
Anthropic's `count_tokens`. The calibrated tokens-per-KB ratio learns each
model's real rate from OpenRouter's native-tokenizer usage, which covers every
model. If exact counts are needed later, add an optional counter interface:
when a provider implements it, `Shape` carries the exact input count and the
prior is skipped. `tiktoken-go/tokenizer` is the first choice for OpenAI
models.

## Implementation record

Implemented in `internal/model/billing`, `internal/model/decision.go`,
`internal/model/openrouter/decisions.go`, migration
`00015_model_calibration.sql`, the worker quote path and `match.Estimate`.

Differences from the design above:

- **No `DecisionUsage`.** `platform.model_calibration` reads `input_tokens`
  and `output_tokens` from settled usage directly; OpenRouter's System One
  usage already uses those names. A provider with different keys adds the
  mapping when it arrives.
- **No `decision_settlement` view.** The calibration function aggregates the
  attempts and usage directly.
- **Calibration has no overhead term.** The observed tokens-per-KB ratio
  already includes template overhead, and the sheet's prior overhead is still
  added on top, so small requests stay covered.
- **The quote stores its calibration**, so `Decide` revalidates against fresh
  catalogue prices with the same calibration the reservation used.
- **`model.QuoteCost` is gone**: the diagnostic chat path now uses
  `QuoteChat`, so nothing called it.
- **The calibration function is `platform.model_calibration`**, not
  `decision_calibration`, and reads chat usage keys too.
- **Request bytes** are the JSON-marshalled request, the same measure as the
  request-size limit.
- **Positive output pricing is incompatible** (from the independent review).
  System One sends no output-token limit, and `systemone.ResultBudget` only
  checks answers after they are billed, so it does not bound billable output.
  (System One models do not reason, so `internal_reasoning` is moot today.)
  OpenRouter sheets have no output rate and `MaxOutput` 0; `billing` keeps
  output support for a provider that can enforce a limit.
- **Calibrated output never drops below the prior**, and a prediction of zero
  for a sheet with a positive ceiling reserves the ceiling. Settlements that
  omit `output_tokens` no longer count as zero output.
- **Corrections:** calibration sums every ledger delta for an attempt and
  reads tokens from the latest usage payload that reports them.

Tests: `internal/model/billing` covers the formula, ceiling cap, margin
widening, the overrun fallback, zero rates and overflow, and replays the
recorded d1 settlement (reserved ≥ settled under the prior, and within 3×
once calibrated; 1.35× in practice). `internal/model/openrouter` has the #20
fixtures: cache read pricier than prompt, missing prompt, audio components
accepted, and `completion`, `internal_reasoning`, `web_search`, unknown and
unparsable components rejected. `internal/match` checks that settled attempts
feed `platform.model_calibration`, including one corrected after a
settlement that lacked token counts. Only one recorded live settlement exists
so far; the replay fixture should grow as more are collected.
