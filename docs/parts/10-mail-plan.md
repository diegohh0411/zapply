# Part 10: Mail inbox (Gmail) plan

Status: built (see "Delivered" at the end for where the build differs from this plan).

cvd watched IMAP with one IDLE connection per desktop app. zapply replaces that
with the Gmail API and Google OAuth. It reads a member's mail, classifies each
message once, and attaches the relevant ones to the matching tracked job.

## Agreed scope

- **Gmail only**, behind a small `MailProvider` interface. Microsoft Graph and
  IMAP can follow without schema changes.
- **Read only.** No labels, no sending, no drafts.
- **Push from day one** (Gmail `users.watch` → Cloud Pub/Sub), built so the app
  is ready for a public launch. A slow reconcile poll is the safety net.
- **Classify every message once**, funded from the member's allowance through
  the existing model gateway. No sender pre-filter in the MVP.
- **Keep data only for relevant mail** (see "Retention").
- **A few testers first.** A new GCP project, OAuth app unverified. Public
  launch is gated on Google verification, which this plan prepares for but
  does not complete.

## Retention

| Classified as | Stored | Not stored |
|---|---|---|
| Relevant (`received`, `assessment`, `interview`, `rejection`, `offer`) | Everything we fetched: sender, subject, date, snippet, category, job link. **Kept until the member deletes their zapply account.** | Message body (fetched with `format=full`, read in memory for the snippet and headers, never persisted) |
| `other` | `provider_message_id`, thread id, received time, category, `classified_at`: just enough not to reprocess it | Sender, subject, snippet, anything readable |

The snippet is read into memory for classification and discarded when the
message is `other`. This is also the story to tell Google and the member: only
mail that looks like a job application is kept.

## Flow

```
Gmail ─ users.watch ─▶ Pub/Sub topic ─ push (OIDC-signed) ─▶ POST /hooks/google/pubsub
                                                              │ verify token, find account by emailAddress
                                                              ▼
                                       enqueue sync job for that account (River, unique per account)
                                                              ▼
sync job:  history.list(from account.history_id)  ─▶ new message ids
           ─▶ messages.get (headers + snippet) ─▶ insert mail_message + enqueue classify job
           ─▶ move history_id, all in one transaction
classify:  decision over (from, subject, snippet) ─▶ category (+ job link if relevant)
           ─▶ scrub snippet/subject/from if `other` ─▶ outbox event ─▶ SSE
```

- **Why push beats per-user polling:** polling costs N accounts × tick rate even
  when nothing arrives. Push costs one small call per actual change and nothing
  while idle. Gmail also pushes within seconds.
- The notification carries only `emailAddress` and `historyId`. The handler does
  not trust it for content: it only enqueues a sync for that account, so
  duplicates and out-of-order delivery collapse into one job (River unique by
  account). Always ack with 200 after the enqueue commits.
- Gmail limits one notification per second per watched user and may drop or
  delay some. So:
  - a **renewal job** calls `users.watch` daily per account (Google requires at
    least every 7 days and recommends daily);
  - a **reconcile job** runs `history.list` for any account with no sync in the
    last few hours (default 6h). This is the only polling, and it is cheap.
- The cursor (`history_id`) moves in the same transaction that inserts messages;
  `UNIQUE (account_id, provider_message_id)` makes a re-read harmless.
- `history.list` can return 404 when the cursor is too old (usually valid for at
  least a week, sometimes only hours). Fall back to a bounded list of the last 30
  days and reset the cursor. First connect backfills the same window.
- **Local development:** no public URL, so no push. Set `MAIL_RECONCILE_INTERVAL`
  to something short (for example `30s`) and the reconcile scan syncs the mailbox.

### GCP setup for Pub/Sub

- A topic in the new project; grant `roles/pubsub.publisher` on it to
  `gmail-api-push@system.gserviceaccount.com`.
- A push subscription to the API's public URL with OIDC authentication using a
  dedicated service account. The handler verifies the signature, issuer,
  audience and service-account email before touching the body.
- Pub/Sub retries unacked messages with backoff; a dead-letter topic is optional.

The webhook has no signed-in member, so account lookup by address goes through a
private platform function (like the system keys in part 5), and
`UNIQUE (provider, address)` is global: one Gmail address can be connected to one
zapply user at a time.

## Credentials

- One OAuth client in the **new GCP project**, type "Web application", redirect
  URI `/oauth/google/callback` on the API.
- Authorization code flow with PKCE, `access_type=offline`, `prompt=consent`, and
  a signed `state` bound to the member session.
- **Scope: `gmail.readonly`, with `messages.get` `format=full`.** This is the one
  combination the docs support for getting the snippet (see below). A field
  mask (`fields=id,threadId,internalDate,snippet,payload/headers`) keeps every
  body part out of the response, so only headers and the `snippet` leave Google.
- The refresh token is encrypted with the existing `internal/secrets` key ring.
  Access tokens are cached in memory only.
- `invalid_grant` sets the account to `needs_reconnect`, stops its sync and watch,
  and emits `mail.account.needs_reconnect`. Disconnect calls `users.stop`, revokes
  the token, deletes it, and keeps classified messages unless the member asks to
  delete them.

### What the Google docs say (checked 2026-10-28)

- Every scope that reads mail is **restricted**: `gmail.readonly`,
  `gmail.metadata`, `gmail.modify` and more. Only `gmail.labels` and add-on
  scopes are lighter, and they don't help.
- `gmail.metadata` is accepted by `messages.get`, `history.list` and
  `users.watch`. With it, `messages.list` cannot use `q`, and `format=full` and
  `raw` are refused. Discovery via `history.list` would work, but the snippet
  would not be documented (below).
- **Where `snippet` is documented.** It is a top-level field of the `Message`
  resource ("A short part of the message text."). The REST reference lists no
  field set per format beyond these:
  - `minimal`: "only email message ID and labels; does not return the email
    headers, body, or payload."
  - `metadata`: "only email message ID, labels, and email headers."
  - `full`: "the full email message data with body content parsed in the
    `payload` field."
  - `full` and `raw` "cannot be used when accessing the api using the
    gmail.metadata scope."
  So `snippet` is not promised for `minimal` or `metadata` (both say "only ..."),
  and is not available under `gmail.metadata` at all if you need `full`. The
  separate Gmail MCP tool reference says its `MINIMAL` view has the snippet and
  `METADATA_ONLY` does not; it is a different API surface, but it points the same
  way. Community clients that read `snippet` from `format=metadata` are relying on
  undocumented behaviour.
- Consequence: the supported route to the snippet is `gmail.readonly` +
  `format=full`. The scope is restricted either way.

### Verification, in practice

- Google's verification page lists **"personal use by you or a few known users"**
  and **test-mode projects limited to listed test users** as exempt from
  verification. That is our testers phase.
- A project with an external consent screen in **Testing** gets refresh tokens
  that expire in 7 days when any non-basic scope is requested. Testers would
  need to reconnect weekly. What happens to an unverified app set to *In
  production* is not described in the docs I read, so I'm not relying on a
  workaround. Plan for weekly reconnects, with a clear "reconnect" banner, until
  verification.
- Public launch: restricted scopes plus data stored on our servers requires
  Google's security assessment (CASA, by an approved assessor), repeated at least
  every 12 months. The docs don't state its price. The storage policy above,
  read-only access and the narrowest scope all reduce what the assessment covers.
- Separate limit: 100 refresh tokens per Google account per OAuth client; the
  oldest silently dies. Irrelevant unless someone reconnects constantly.

## Schema (migration `00018_mail.sql`)

```
mail_account  id, user_id, provider ('google'), address, status
              ('active'|'needs_reconnect'|'disconnected'),
              refresh_token_enc, key_version, history_id,
              watch_expires_at, last_synced_at, last_error, created_at
              UNIQUE (provider, address)
mail_message  id, user_id, account_id, provider_message_id, thread_id,
              received_at, category, classified_at NULL, job_id NULL,
              from_address NULL, subject NULL, snippet NULL,   -- null once `other`
              created_at
              UNIQUE (account_id, provider_message_id)
```

Both tables get forced RLS, and `zapply_worker` gets explicit grants. Only the
worker writes `mail_message`. `refresh_token_enc` is never selectable by
`zapply_app`; the API stores it through a private platform function.

## Classification

- Reuse `internal/decide`: a typed question over `(from, subject, snippet)`,
  billed through the gateway with a reservation, keyed by the content sent.
- The first answer is the category. For relevant categories a second small
  decision picks one of the member's open jobs (by company and title) or none.
  It is a suggestion; changing a job's status stays manual in the MVP.
- If the allowance is exhausted, classify jobs wait like other model work and the
  message shows as unclassified.
- A member-facing switch pauses classification. The connect screen states that
  every incoming message's sender, subject and preview is sent to the model.

## API, events and web

- `GET /v1/mail/accounts`, `POST /v1/mail/accounts/google/start`,
  `POST /v1/mail/accounts/google/complete`, `PATCH /v1/mail/accounts/{id}` (pause or
  resume), `DELETE /v1/mail/accounts/{id}`.
- Outside `/v1`, because neither request carries a bearer token:
  `GET /oauth/google/callback` (only redirects to the app with the code and state)
  and `POST /hooks/google/pubsub` (identified by Pub/Sub's signed token).
- `GET /v1/mail/messages?category=&job_id=` with cursor pagination; relevant mail
  only.
- Events: `mail.account.updated`, `mail.message.classified`.
- Web: a settings card to connect, pause or disconnect; an inbox list grouped by
  category; linked messages on the job detail page (part 9).

## Build order

1. Create the GCP project, consent screen (scope `gmail.readonly`), OAuth client,
   Pub/Sub topic and push subscription.
2. Migration, `MailProvider` interface, Google client, connect and callback,
   encrypted token storage.
3. Pub/Sub topic, push handler, sync job, cursor, backfill, watch renewal,
   reconcile, `needs_reconnect`.
4. Classification, retention scrub, job linking.
5. API, events, web.
6. Fake `MailProvider` and a fake Pub/Sub sender for tests: cursor expiry,
   revoked token, duplicate and out-of-order notifications, scrub on `other`.

## Open questions

- None blocking. If a later Google doc change documents `snippet` for
  `format=metadata`, we can switch to it and to `gmail.metadata`; the scope change
  would require users to consent again.

## Delivered

Everything in the build order is implemented: `internal/mail` (service, sync,
classification, workers), `internal/mail/google` (OAuth and Gmail over `net/http`),
migration `00018_mail.sql`, the endpoints above, and the web app's Mail page and
a Mail tab on the job sheet. Where the build differs from the plan:

| Plan | Built | Why |
|---|---|---|
| Callback and push under `/v1/mail/...` | `/oauth/google/callback` and `/hooks/google/pubsub` | `/v1` requires a bearer token; the browser redirect and Pub/Sub carry none |
| The callback completes the connection | The callback redirects to `/mail?oauth_state&oauth_code`; the app calls `POST /v1/mail/accounts/google/complete` as the signed-in member | A callback that trusts only the state lets a consent link sent to someone else attach their mailbox to the sender's account. The state now has to belong to the caller |
| Read as many messages as arrive | At most 100 per run, none older than 30 days; a backlog continues in a follow-up sync, and the cursor only advances over what was read | Keeps a run inside the job timeout and never skips mail |
| Pending mail is classified | Not while the mailbox is paused; dropped (content cleared) once disconnected; at most 3 open requests per member | A pause or disconnect must stop spend; matching keeps its model slots |
| Pub/Sub pull subscription for local development | Not built. Use a short `MAIL_RECONCILE_INTERVAL` | Pull needs a service-account client for the Pub/Sub API; reconcile covers development with no new dependency |
| Content scrubbed from model requests ("scrub snippet/subject/from") | Also blanks the stored request: `platform.scrub_mail_request` | `model_request` is insert-only and held the exact text sent. A restricted platform function blanks `state` for mail operations only, after the answer is recorded |
| One `classify` step | Two questions: category, then (for relevant mail only) which open job | The job link is a separate, optional answer; failing it keeps the message, unlinked |
| A "pause classification" switch | Account status `paused`: the mailbox is not read at all | Nothing is fetched or sent to the model while paused |
| `failed` messages retried | After 3 failed category requests the message is dropped (`state='failed'`, content cleared) | No retry UI yet |
| `mail_pending_messages` re-wakes lost jobs | Hands each waiting message out once per 15 minutes | Avoids re-queuing every message every minute |

Other behaviour worth knowing:

- Retention is enforced by `CHECK` constraints on `mail_message`: a message in
  category `other`, or one that failed, cannot hold a sender, subject or snippet.
- Refresh tokens are sealed with the same key ring as the system provider keys and
  live in `platform.mail_credential`, readable only through platform functions.
  `zapply secrets reencrypt` re-seals them too.
- A member has at most 8 mail requests open at once; the rest wait, so a backfill
  with no allowance does not park hundreds of blocked operations.
- Deleting a zapply account deletes its mailboxes and messages (foreign keys
  cascade). Disconnecting a mailbox revokes the grant but keeps classified mail.

### Setup

1. In the Google Cloud project, enable the Gmail API and the Pub/Sub API.
2. OAuth consent screen: external, scope `gmail.readonly`, add testers while the
   app is unverified. In Testing, refresh tokens expire after 7 days.
3. OAuth client (Web application): authorized redirect URIs
   `<PUBLIC_URL>/oauth/google/callback` (add `http://localhost:8080/oauth/google/callback`
   for development).
4. Push (optional): create a topic; grant `roles/pubsub.publisher` on it to
   `gmail-api-push@system.gserviceaccount.com`; create a push subscription to
   `<PUBLIC_URL>/hooks/google/pubsub` with OIDC authentication using a dedicated
   service account and the push URL as the audience.
5. Environment, on the API and the worker: `GOOGLE_OAUTH_CLIENT_ID`,
   `GOOGLE_OAUTH_CLIENT_SECRET`, `PUBLIC_URL`, and for push `GOOGLE_PUBSUB_TOPIC`
   (`projects/<project>/topics/<topic>`), `GOOGLE_PUBSUB_SERVICE_ACCOUNT`
   (the subscription's service account email) and optionally
   `GOOGLE_PUBSUB_AUDIENCE` (defaults to the push URL). `SYSTEM_KEY_RING` must be set
   too, and the member needs a membership for classification to run.
