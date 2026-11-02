# Automatic CV summaries

CV previews register a summary draft. A selected-evidence snapshot, locale,
job description, model and endpoint identify each generation. The profile
summary and contact details are excluded from the agent input.

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
selection changes. This works with the browser closed and recovers on restart.

Automatic summaries appear in preview and export. Saving a manual summary
protects it; future generated text is offered as a suggestion. “Use automatic
summary” removes the manual override. The last generated summary remains
visible while a replacement runs. Background polling preserves unsaved edits.

Deploy migration 00022 and restart both API and worker. Generation stays
unconfigured until an owner selects a model and endpoint. No live provider
calls are made by the automated tests.

Generated and manually saved summaries have a 500-character cap, counted as
Unicode code points including spaces and punctuation. Oversized model output
is rejected; the editor displays a counter and prevents saving oversized text.

Owners also select a summary completion-token cap: 1,024, 2,048 (default),
4,096, or 8,192. This budget includes reasoning tokens as well as the
final paragraph; the 500-character summary limit is unchanged. The selected cap
is snapshotted in each queued request and used for both the full-cost allowance
reservation and the provider's `max_tokens`. Changing the setting invalidates
existing summaries. Requests queued before cap configuration retain their
original 256-token bound until replaced.

Apply migration 00023 before deploying the updated API and worker. It sets the
initial cap to 2,048 and invalidates existing summary snapshots for regeneration.
