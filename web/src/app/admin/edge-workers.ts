import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, effect, inject, signal, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';

type Worker = Schemas['EdgeWorker'];

/** What a worker's row edits. Hosts are one text box, one host per line. */
interface Draft { name: string; hosts: string; max_per_hour: number; max_concurrent: number }

// A worker polls every 25 seconds, so one not seen for 90 is offline.
const ONLINE_MS = 90_000;
const REFRESH_MS = 20_000;

/** "linkedin.com, Indeed.com" or one per line → ['linkedin.com', 'indeed.com']. */
export function parseHosts(text: string): string[] {
  return [...new Set(text.split(/[\s,;]+/).map((h) => h.trim().toLowerCase().replace(/\.$/, '')).filter(Boolean))];
}

// Edge workers: browsers on residential machines that pull scrape jobs from this
// server, for sites that block datacenter addresses (LinkedIn). A worker may
// serve any user's postings, so it opens only the hosts listed here, within its
// hourly and concurrent limits. The "Edge workers" provider in Scraping tries them.
@Component({
  selector: 'app-edge-workers',
  imports: [FormsModule, DatePipe, ZardButtonComponent, ZardInputComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section aria-labelledby="edge-heading" class="space-y-5">
      <header class="space-y-1">
        <h2 id="edge-heading" class="text-lg font-semibold">Edge workers</h2>
        <p class="max-w-2xl text-sm text-muted-foreground">
          A worker runs on a home machine (a laptop with Podman), reads pages in an isolated headless Chrome and sends them here. It makes only
          outbound HTTPS requests. It serves anyone's postings, so it opens only the sites listed below, never private addresses, and within its limits.
          Add an <span class="font-medium">Edge workers</span> provider above to use them; if none is online the next provider is tried at once.
        </p>
      </header>

      @if (error()) {
        <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">{{ error() }}</p>
      }

      @if (reveal(); as r) {
        <div class="space-y-3 rounded-lg border border-amber-300 bg-amber-50 p-4 text-amber-950 dark:border-amber-700 dark:bg-amber-950/30 dark:text-amber-100" role="status">
          <h3 class="text-sm font-semibold">Token for “{{ r.name }}”: copy it now</h3>
          <p class="text-sm">It is shown only once; only its hash is stored. If you lose it, rotate the token.</p>
          <div class="flex flex-wrap items-center gap-2">
            <code class="min-w-0 flex-1 break-all rounded bg-background px-2 py-1 font-mono text-xs text-foreground">{{ r.token }}</code>
            <button z-button zType="outline" zSize="sm" (click)="copy(r.token, 'Token copied.')">Copy token</button>
          </div>
          <p class="text-sm">On the machine, in <span class="font-mono">deploy/edge/.env</span>:</p>
          <pre class="overflow-x-auto rounded bg-background p-3 font-mono text-xs text-foreground">{{ envSnippet(r.token) }}</pre>
          <div class="flex flex-wrap gap-2">
            <button z-button zType="outline" zSize="sm" (click)="copy(envSnippet(r.token), '.env copied.')">Copy .env</button>
            <button z-button zType="ghost" zSize="sm" (click)="reveal.set(null)">I have saved it</button>
          </div>
          <p class="text-sm">Then run <code class="font-mono">cd deploy/edge &amp;&amp; podman compose up -d</code>.</p>
        </div>
      }
      <p class="text-sm text-muted-foreground" role="status">{{ notice() }}</p>

      <ul class="space-y-3">
        @for (w of workers(); track w.id) {
          @let d = drafts[w.id];
          <li class="space-y-4 rounded-lg border p-4">
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div class="space-y-1">
                <h3 class="text-sm font-medium">{{ w.name }}</h3>
                <p class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                  <span [class.text-green-700]="online(w)" [class.dark:text-green-400]="online(w)">{{ !w.enabled ? 'Disabled' : online(w) ? 'Online' : 'Offline' }}</span>
                  <span>Last seen {{ w.last_seen_at ? (w.last_seen_at | date: 'medium') : 'never' }}</span>
                  <span>{{ w.jobs_24h }} jobs in 24 h</span>
                  <span>{{ successRate(w) }}</span>
                </p>
              </div>
              <div class="flex gap-1">
                <button z-button zType="outline" zSize="sm" [zDisabled]="busy()" (click)="toggle(w)">{{ w.enabled ? 'Disable' : 'Enable' }}</button>
                <button z-button zType="outline" zSize="sm" [zDisabled]="busy()" (click)="rotate(w)">Rotate token</button>
              </div>
            </div>
            @if (d) {
              <div class="grid gap-4 md:grid-cols-4">
                <label class="field"
                  ><span>Name</span><input z-input [name]="'edge-name-' + w.id" [(ngModel)]="d.name" maxlength="80" autocomplete="off" />
                </label>
                <label class="field md:col-span-2"
                  ><span>Allowed sites</span>
                  <textarea
                    class="min-h-20 w-full rounded-md border bg-background px-3 py-2 font-mono text-sm"
                    [name]="'edge-hosts-' + w.id"
                    [(ngModel)]="d.hosts"
                    rows="3"
                    placeholder="linkedin.com"
                    autocomplete="off"
                    autocapitalize="off"
                    spellcheck="false"
                  ></textarea>
                  <span class="text-xs font-normal text-muted-foreground">One host per line; each also covers its subdomains. Empty allows none.</span>
                </label>
                <div class="grid gap-4">
                  <label class="field"
                    ><span>Jobs per hour</span><input z-input type="number" min="1" max="600" [name]="'edge-hour-' + w.id" [(ngModel)]="d.max_per_hour" />
                  </label>
                  <label class="field"
                    ><span>At once</span><input z-input type="number" min="1" max="10" [name]="'edge-concurrent-' + w.id" [(ngModel)]="d.max_concurrent" />
                  </label>
                </div>
              </div>
              <div class="flex flex-wrap items-center gap-3">
                <button z-button zSize="sm" [zDisabled]="busy() || !changed(w)" (click)="save(w)">Save worker</button>
                @if (changed(w)) {
                  <button z-button zType="ghost" zSize="sm" [zDisabled]="busy()" (click)="reset(w)">Discard</button>
                }
              </div>
            }
          </li>
        } @empty {
          <li class="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">{{ loading() ? 'Loading edge workers…' : 'No edge workers yet.' }}</li>
        }
      </ul>

      <form class="space-y-4 rounded-lg border p-4" (ngSubmit)="create()">
        <h3 class="text-sm font-medium">Add an edge worker</h3>
        <div class="grid gap-4 md:grid-cols-4">
          <label class="field"
            ><span>Name</span><input z-input name="new-name" [(ngModel)]="draft.name" maxlength="80" placeholder="Home laptop" autocomplete="off" required />
          </label>
          <label class="field md:col-span-2"
            ><span>Allowed sites</span>
            <textarea
              class="min-h-20 w-full rounded-md border bg-background px-3 py-2 font-mono text-sm"
              name="new-hosts"
              [(ngModel)]="draft.hosts"
              rows="3"
              placeholder="linkedin.com"
              autocomplete="off"
              autocapitalize="off"
              spellcheck="false"
            ></textarea>
          </label>
          <div class="grid gap-4">
            <label class="field"
              ><span>Jobs per hour</span><input z-input type="number" min="1" max="600" name="new-hour" [(ngModel)]="draft.max_per_hour" />
            </label>
            <label class="field"
              ><span>At once</span><input z-input type="number" min="1" max="10" name="new-concurrent" [(ngModel)]="draft.max_concurrent" />
            </label>
          </div>
        </div>
        <p class="text-xs text-muted-foreground">
          Keep the volume low: LinkedIn's terms restrict automated access, and the worker reads only the logged-out pages. Twenty jobs an hour is a sensible ceiling.
        </p>
        <button z-button type="submit" [zDisabled]="busy() || !draft.name.trim()">{{ busy() ? 'Working…' : 'Create worker' }}</button>
      </form>
    </section>
  `,
  styles: [':host { display: block; }'],
})
export class EdgeWorkers {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  readonly workers = signal<Worker[]>([]);
  readonly error = signal('');
  readonly notice = signal('');
  readonly loading = signal(false);
  readonly busy = signal(false);
  /** A token to show once: after create or rotate. */
  readonly reveal = signal<{ name: string; token: string } | null>(null);
  private readonly now = signal(Date.now());
  drafts: Record<string, Draft> = {};
  draft: Draft = { name: '', hosts: 'linkedin.com', max_per_hour: 20, max_concurrent: 1 };
  private generation = 0;

  constructor() {
    effect(() => {
      this.auth.identity();
      untracked(() => {
        this.generation++;
        this.workers.set([]);
        this.drafts = {};
        this.reveal.set(null);
        this.notice.set('');
        void this.load();
      });
    });
    const timer = setInterval(() => {
      this.now.set(Date.now());
      if (!this.busy()) void this.refresh();
    }, REFRESH_MS);
    inject(DestroyRef).onDestroy(() => clearInterval(timer));
  }

  async load(): Promise<void> {
    this.loading.set(true);
    try {
      await this.refresh(true);
    } finally {
      this.loading.set(false);
    }
  }

  /** Reads the list; edits in progress survive a background refresh. */
  private async refresh(report = false): Promise<void> {
    const identity = this.auth.identity(), seq = this.generation;
    try {
      const list = Api.ok(await this.api.GET('/v1/admin/edge-workers'));
      if (identity !== this.auth.identity() || seq !== this.generation) return;
      const previous = this.workers();
      this.workers.set(list);
      const next: Record<string, Draft> = {};
      for (const w of list) {
        // Keep what is being typed, unless the worker changed underneath it.
        const old = this.drafts[w.id];
        const before = previous.find((x) => x.id === w.id);
        next[w.id] = old && before && before.version === w.version && this.changedDraft(old, before) ? old : this.toDraft(w);
      }
      this.drafts = next;
    } catch (e) {
      if (report && identity === this.auth.identity()) this.error.set(this.message(e));
    }
  }

  private toDraft(w: Worker): Draft {
    return { name: w.name, hosts: w.allowed_hosts.join('\n'), max_per_hour: w.max_per_hour, max_concurrent: w.max_concurrent };
  }
  private changedDraft(d: Draft, w: Worker): boolean {
    return JSON.stringify(d) !== JSON.stringify(this.toDraft(w));
  }
  changed(w: Worker): boolean {
    const d = this.drafts[w.id];
    return !!d && this.changedDraft(d, w);
  }
  reset(w: Worker): void {
    this.drafts[w.id] = this.toDraft(w);
    this.error.set('');
  }

  online(w: Worker): boolean {
    return w.enabled && !!w.last_seen_at && this.now() - new Date(w.last_seen_at).getTime() < ONLINE_MS;
  }
  successRate(w: Worker): string {
    return w.jobs_24h ? `${Math.round((100 * w.done_24h) / w.jobs_24h)}% succeeded` : 'no jobs yet';
  }
  envSnippet(token: string): string {
    return `ZAPPLY_URL=${window.location.origin}\nZAPPLY_EDGE_TOKEN=${token}`;
  }

  async copy(text: string, done: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(text);
      this.notice.set(done);
    } catch {
      this.notice.set('Copy failed: select the text and copy it by hand.');
    }
  }

  private async act<T>(work: () => Promise<T>, ok: (value: T) => void): Promise<void> {
    if (this.busy()) return;
    const identity = this.auth.identity();
    this.busy.set(true);
    this.error.set('');
    this.notice.set('');
    try {
      const value = await work();
      if (identity === this.auth.identity()) ok(value);
    } catch (e) {
      if (identity === this.auth.identity()) this.error.set(this.message(e));
    } finally {
      this.busy.set(false);
    }
  }

  create(): Promise<void> {
    const d = this.draft;
    return this.act(
      async () =>
        Api.ok(
          await this.api.POST('/v1/admin/edge-workers', {
            body: { name: d.name.trim(), allowed_hosts: parseHosts(d.hosts), max_per_hour: d.max_per_hour, max_concurrent: d.max_concurrent },
          }),
        ),
      async (created) => {
        this.reveal.set({ name: created.worker.name, token: created.token });
        this.draft = { name: '', hosts: 'linkedin.com', max_per_hour: 20, max_concurrent: 1 };
        await this.refresh();
      },
    );
  }

  save(w: Worker): Promise<void> {
    const d = this.drafts[w.id];
    if (!d) return Promise.resolve();
    return this.patch(w, { name: d.name.trim(), allowed_hosts: parseHosts(d.hosts), max_per_hour: d.max_per_hour, max_concurrent: d.max_concurrent }, 'Worker saved.');
  }
  toggle(w: Worker): Promise<void> {
    return this.patch(w, { enabled: !w.enabled }, w.enabled ? 'Worker disabled: its token stops working at once.' : 'Worker enabled.');
  }
  private patch(w: Worker, body: Schemas['EdgeWorkerPatch'], done: string): Promise<void> {
    return this.act(
      async () =>
        Api.ok(
          await this.api.PATCH('/v1/admin/edge-workers/{id}', {
            params: { path: { id: w.id }, header: { 'If-Match': `"${w.version}"` } },
            body,
          }),
        ),
      async () => {
        this.notice.set(done);
        delete this.drafts[w.id];
        await this.refresh();
      },
    );
  }

  rotate(w: Worker): Promise<void> {
    if (!window.confirm(`Rotate the token for “${w.name}”? The worker stops until it is given the new one.`)) return Promise.resolve();
    return this.act(
      async () => Api.ok(await this.api.POST('/v1/admin/edge-workers/{id}/token', { params: { path: { id: w.id } } })),
      async (rotated) => {
        this.reveal.set({ name: rotated.worker.name, token: rotated.token });
        await this.refresh();
      },
    );
  }

  private message(error: unknown): string {
    if (error instanceof ApiError && error.status === 412) return 'Someone else changed this worker. The list was refreshed; redo your change.';
    if (error instanceof ApiError && error.fieldErrors.length) return error.fieldErrors.map((e) => (e.field ? `${e.field}: ${e.message}` : e.message)).join('; ');
    return error instanceof Error ? error.message : 'Could not complete the request.';
  }
}
