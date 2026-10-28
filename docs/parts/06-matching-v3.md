# Part 6, v3: whole-posting matching

Status: pipeline 3 is the default for manual and automatic runs.

Matching follows CVD's architecture. The complete job title, company and raw
posting form shared state. Each typed decision question carries the single
experience entry, highlight or skill it judges. There is no posting chunking,
requirement extraction, requirement classification, coverage round or gap analysis.

One round asks for role seniority, each skill's relevance (yes probability),
each eligible entry's relevance and each highlight's support for a fit (0–4
ordered scales). Career breaks are excluded. Go ranks entries and highlights
by expected relevance and skills by their probabilities. There is no recency
multiplier or requirement attribution added to relevance.

Entries meeting CVD's 1.5/4 relevance floor are selected for automatic CVs when
their library flag permits it. Relevant entries with automatic inclusion off
are suggestions. Highlights retain their inclusion flags and are ranked within
their entry. Existing CV rules still preserve degrees and allow manual picks.
There is no aggregate match percentage; `score` stays null, `must_met` and
`must_total` stay zero, and `requirements` and `insights` are empty arrays.
Missing answers mark rankings partial; rendering continues to use completed runs.

Questions share a posting state, so requests can pack different subjects
together. Individual question hashes include their own input data: changing
one entry does not invalidate unrelated entry or skill answers. Display flags
stay out of model inputs, so changing automatic inclusion reuses answers.
The allowance gateway, request limits, retries, cache and cancellation remain
in force. Estimates price only uncached questions. A posting too large for a
single question fails through existing request-size checks; it is never
silently truncated to the first 120 lines.

Migration 17 changes the database default to 3. Pipeline 1 and 2 remain
versioned so historical answers keep their original interpretation. Those
results are marked stale, their coverage scores are omitted from job summaries,
and the match tab requests a re-run instead of showing the old gap checklist.
Historical overrides have no effect on pipeline 3. The API's explicit pipeline
parameter remains available for historical comparison.
