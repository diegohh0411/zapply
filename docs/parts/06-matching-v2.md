# Part 6, v2: linear matching

Status: implemented as pipeline 2, the default for new runs. Pipeline 1
([06-matching.md](06-matching.md)) remains available so the two can be
compared on the same job, and so runs it produced keep recomputing with the
rules that asked them.

## Why

A real run of pipeline 1 asked 843 questions for one posting: 41 lines × 4
read questions, plus 76 skills, plus every entry and every highlight scored
against every requirement (14 × (18 + 25)). The count grows with requirements
× library size, and each score request held a single entry, so a run needed
about 21 sequential requests.

cvd asked about 163 questions for the same library by keeping the posting as
one shared state and scoring each entry once against the whole posting. It
could not say what was missing, which is why pipeline 1 added requirements.

Pipeline 2 keeps the requirements and their gaps, citations and checks, and
returns to cvd's shape. It asks about 230 questions for that run, in two
rounds of parallel requests.

## Shape

Two states per run:

- **Posting:** `posting.title`, `posting.company` and `posting.lines`. The
  description is not repeated; the lines are its source slices.
- **Posting and library:** the same posting plus `library.evidence`, a compact
  list of claims (kind, category, title, organization name, dates, summary,
  details, highlight text and metrics, linked skill names). Items are referred
  to by path. Ids, content hashes, selection flags and career breaks stay out.

Questions referring to the posting only use the posting state, so they stay
cached when the library changes. Every library edit re-asks the library
questions, which is intended: each run should rank the current library.

| Round | Subject key | Type | State | Count |
|---|---|---|---|---|
| 1 | `line:i:class`: must, preferred, duty, **trait** or other | choice | posting | lines |
| 1 | `seniority` | choice | posting | 1 |
| 1 | `language:<skill>`: minimum level for each library language skill | choice | posting | language skills |
| 1 | `skill:<skill>`: does the posting value it | noul | posting | skills |
| 1 | `relevance:<entry>`: cvd's relevance to the whole posting, 0–4 | score | library | entries |
| 1 | `highlight:<highlight>`: the same for each highlight | score | library | highlights |
| 2 | `strength:<req>`: how strongly the library shows it, 0–3 | score | library | requirements |
| 2 | `cover:<req>`: which entry shows it best, or none | choice | library | requirements |
| 2 | `cite:<req>`: which highlight shows it best, or none | choice | library | requirements |
| 2 | `check:<req>`: degree, language or other | choice | posting | requirements |
| 2 | `degree:<req>`: minimum degree the line requires | choice | posting | requirements |

Round two needs the requirement list, so it waits for round one. Within a
round every request is sent at once (see the parallel dispatch notes in
[06-matching.md](06-matching.md)), with at most 32 questions per request so a
round splits across workers.

## Requirements

A line becomes a requirement when its class is must, preferred, duty or
trait. Trait covers personal qualities and working style ("curious", "kind",
"rigorous thinker"); pipeline 1 counted such lines as double-weighted musts.
Weights: must 2, preferred 1, duty 1, trait ½. The 40-requirement cap,
deduplication, years parsing, empty-description fallback and corrections
(including the new trait class) are unchanged.

## Scoring

The model judges, Go decides, as before.

- **Strength** of a requirement is the expected utility of `strength:<req>`
  with the pipeline 1 utilities [0, .25, .6, 1].
- **Attribution.** `cover:<req>` gives a probability for every entry. The
  entry with the highest probability of at least 0.2 is the supporting
  evidence. Every entry also gets a soft share, weight × probability ×
  strength, for ranking. This fills the entry × requirement table from one
  question per requirement.
- **Checks.** Years use the union of dates of entries with cover probability
  of at least 0.2. A `degree` requirement uses its own line's `degree:<req>`
  level ("bachelor required" and "master preferred" differ) and is met by the entry with a completed degree at that level (preferring
  the more probable cover); otherwise strength is capped at 0.6. A `language`
  requirement uses the posting's level for each library language named in the
  line; every named language must meet its level ("C1 English and C1
  Spanish"), or strength is capped at 0.6.
- **Citations.** Up to three highlights with `cite:<req>` probability of at
  least 0.25, when strength is at least 0.4.
- **Status, must-haves met and job score** use the pipeline 1 thresholds and
  formula, with the recency of the attributed entry.
- **Entry relevance** = recency × (relevance + Σ soft shares). An entry is
  selected (or suggested, if it is not auto-included) when its relevance is at
  least 0.375 (cvd's floor of 1.5 on 0–4) or it supports a requirement with
  strength of at least 0.4.
- **Highlights** within an entry are ordered by relevance plus their citation
  shares. Skills rank as in pipeline 1.

Any missing applicable answer makes the run partial, without a score.

## Comparing the pipelines

Start a pipeline 1 run on a job with
`POST /v1/jobs/{id}/match-runs?pipeline=1`, then a default run, and compare
them with [`scripts/match-compare.sql`](../../scripts/match-compare.sql). It
reports questions, requests, cost and duration per run, each requirement's
class, status, strength and evidence side by side, and how many requirements
and selected entries agree.

## Open questions

- Calibration of `cover` and `cite` with many options is unproven. The
  comparison above is the check before relying on it.
- The shared library state was about 11K tokens for an 18-entry library
  against a 32K context. Compaction is tracked in issue #18.
