# Automatic CV summaries

CV previews register a summary draft. A generation is identified by the agent
input, locale, job description, model and endpoint. The agent input is the
headings of the entries on the page (title, organization, dates, current) and
the highlights the page shows. Skills, entry summaries, details, the previous
summary, and contact details are excluded.

The agent sees only what the page shows. The browser fits the CV to its page
budget and reports the optional IDs it dropped, with the `fit_key` the preview
carried (`PUT /v1/jobs/{id}/cv/fit`). The key hashes the content without the
summary, the trim order, the template and the page budget. The server accepts
a fit only when the key matches the current content and the dropped IDs are
optional items. Until a fit is reported for the current content, the preview
reports `waiting_for_fit` and no summary is queued. A fit for content that has
since changed is a 409 conflict, and the browser reloads and fits again.

Fitting reserves exactly as many summary lines as the owner's **summary rows**
setting (1–4, default 2), so the fit does not depend on the summary's text. The
key covers the rows, so changing the setting asks for a new fit. A manual
summary is fitted as written instead, and its text is part of the key.

The generated summary must wrap to exactly those rows. The server measures with
the Typst CLI the CV compiles with: it compiles a page holding only the summary
and counts its lines. When a summary is queued, a calibration paragraph gives
the characters per line for the template version and locale (classic/1 holds
about 114), and the instruction asks for the character range that ends on the
last row. After each answer the worker measures it. If it wraps to more or fewer
rows, the answer and feedback naming the line count and the characters to add
or cut are sent back in the same conversation, up to twice. Each revision is a
separate, separately charged attempt of the same operation; the turns are kept
in `summary_turn`, since request bodies never change. If the last answer is
still too long, trailing sentences are dropped until it fits, then trailing
words with an ellipsis; one still too short is published. While a revision is
queued the preview reports the summary as running. If the server can't measure
(no Typst CLI), summaries are published unchecked.

Owners select a text model from the OpenRouter catalogue and configure its
provider endpoint in the existing model settings panel. The endpoint must
support bounded output. The existing system key and per-member USD allowance
are used; no separate credit balance or provider credentials are introduced.

Generation runs through Google ADK Go with a fresh, tool-free session and a
single model call. The shared durable model worker reserves the full bounded
request cost, enforces allowance and concurrency limits, then settles actual
OpenRouter usage. Uncertain or cancelled charges retain the existing
reconciliation behavior.

Draft and library changes invalidate summaries through the outbox transaction.
A worker scans invalidated drafts once per second, after a 700 ms quiet period,
and queues replacement operations. Running requests check durable invalidation
every 200 ms and cancel their context. Replaced operations cannot publish into
the current draft. Requests are also scheduled 700 ms out to coalesce rapid
selection changes. Background regeneration works with the browser closed only
while the stored fit still matches the content. When the content changed, the
worker stops the summary and waits for the next CV view to report a fit.

Automatic summaries appear in preview and export. Saving a manual summary
protects it; future generated text is offered as a suggestion. “Use automatic
summary” removes the manual override. The last generated summary remains
visible while a replacement runs. Background polling preserves unsaved edits.

Deploy migration 00022 and restart both API and worker. Generation stays
unconfigured until an owner selects a model and endpoint. No live provider
calls are made by the automated tests.

Each completion is capped at 4,096 tokens, reasoning included; the thinking
effort sets how much of it a model spends. Manually saved summaries keep their
500-character limit; the editor displays a counter and prevents oversized saves.

Apply migration 00023 before deploying the updated API and worker. It sets the
initial cap to 2,048 and invalidates existing summary snapshots for regeneration.

Apply migration 00024 before deploying the updated API and worker. The character
cap is saved in each queued request and included in the generation snapshot.
Changing it invalidates automatic summaries. Older clients that omit the
setting preserve its saved value, and older queued requests retain their
original 500-character limit. Existing manual summaries are preserved.

Apply migration 00028 before deploying the updated API and worker. It adds the
stored fit (`fit_key`, `fit_dropped`) to each summary snapshot. Existing
snapshots have no fit, so their summaries wait for the next CV view to report one.

Apply migration 00029 before deploying the updated API and worker. It replaces
the character and token caps with `summary_rows` (default 2), adds
`summary_turn`, and invalidates automatic summaries. The worker needs the Typst
CLI (`TYPST_DIR`, as the image sets) to measure rows.
