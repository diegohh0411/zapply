# Part 9: Web app (started with part 4)

Status: in progress. Shell, sign-in, live updates, jobs and library screens are built. Matching, rendering and mail screens arrive with their parts.

## Stack

- **Angular 22**: standalone components, signals, `OnPush`, lazy routes, and `withComponentInputBinding` for query parameters such as `?job=` and `?view=`.
- **ZardUI**: shadcn-style components copied into `web/src/app/shared` (so they're ours to change). It uses Tailwind 4 and `@ng-icons/lucide`.
  - **Missing components:** the CLI (`@ngzard/ui@1.0.0-beta.31`) failed to write `dialog`, `sheet`, `dropdown` and `alert-dialog`. They were installed from the registry JSON (`https://zardui.com/r/<name>.json`) with the CLI's import rewrites applied by hand.
  - **Broken imports:** the CLI's rewrite also breaks `core/i18n/locales/*.ts` imports, which were fixed to `../i18n.*`.
  - **Peer dependencies:** `.npmrc` sets `legacy-peer-deps`, because the CLI still installs `lucide-angular`, which doesn't declare support for Angular 22. That package was removed: the components use `@ng-icons`.
- **API client:** `openapi-typescript` generates `src/app/api/schema.d.ts` from `api/openapi.yaml` (`npm run gen:api`, also run by `make generate`), and `openapi-fetch` makes typed calls. CI fails if the types are stale.
- **Auth:** `@supabase/supabase-js` with an email magic link or Google. In development, the API's `POST /dev/token` mints dev tokens instead; it only exists with `ZAPPLY_ENV=development` and `AUTH_DEV_SECRET`.

## Hosting: embedded in `zapply api`

The build output is embedded with `go:embed` (`internal/webui`) and served by the API for every path that isn't `/v1`, `/healthz`, `/readyz` or `/dev`. App routes fall back to `index.html`.

- **Why embedded:** one image, so UI and API always deploy in sync. It is same-origin, so it needs no CORS. Static traffic is negligible next to the API.
- **When to move to a CDN:** when global latency or per-PR previews matter. The SPA is a separate build either way; moving means pointing a CDN at `web/dist` and adding CORS.
- **Caching:** hashed assets (`main-HFRQZ5VH.js`) are `immutable` for a year. `index.html` and `/config.json` are `no-cache`. Responses carry `nosniff`, `Referrer-Policy` and `X-Frame-Options: DENY`.
- **`/config.json`** carries the public runtime settings (`supabase_url`, `supabase_anon_key`, `dev_login`), so one image works in every environment.
- **Builds:** the Dockerfile builds the app in a Node stage and copies it in before `go build`. Locally, `make web` does the same. A binary built without it serves `404` with a hint, and the API logs a warning.

## Data flow

- Stores load once, then follow `/v1/events` through a fetch-based SSE client (`core/events.service.ts`). `EventSource` can't send the bearer token.
  - The client resumes with `Last-Event-ID`.
  - A `resync` (first connect, or after `reset`) makes stores refetch, following the part 3 client contract.
  - A dot in the sidebar shows whether the stream is live.
- **Jobs:** `JobsStore` holds all statuses, job summaries and views. The board and table filter on the client, which suits the hundreds of jobs one person tracks.
  - Board drags show at once with a local key (`predecessor + "\0"`, which always sorts between neighbours), then take the server's position.
  - A drag that fails reloads the board.
- **Concurrent edits:** forms keep the ETag and send `If-Match`. A `412` reloads the latest version and says so.
  - The job sheet also refreshes itself when another tab changes the job, unless it has unsaved edits.
- **Errors:** validation errors (`422 errors[]`) are shown next to their fields. A duplicate posting offers to open the existing job.

## Screens

| Route | What |
|---|---|
| `/login` | Magic link and Google (Supabase), or development sign-in |
| `/jobs` | The board (drag between and within columns) or a sortable table. Saved views as tabs, search, stage chips and an archived toggle. `?job=<id>` opens the job sheet: details form, status, archive, delete, and an activity timeline with notes. A dialog manages columns (rename, color, stage, reorder, add, delete with move-to) |
| `/library/profile` | Profile and links |
| `/library/experience` | Evidence by category, children nested under their parent. The editor builds its fields from `GET /v1/kinds` (`details_schema`) and handles highlights and skills |
| `/library/organizations` | Add, edit, delete |
| `/library/skills` | Grouped by type. Edit, merge, delete |

## Not yet

- Dragging columns (they are reordered with buttons in the dialog). Board virtualization.
- Reordering and moving evidence (`/v1/evidences/{id}/move`).
- UI tests. This part was checked by driving a local build in a browser: sign-in, create, status change, drag between columns, live update from another client, table, saved view, evidence create and edit, profile save.
