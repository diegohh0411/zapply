import { DatePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  computed,
  DestroyRef,
  effect,
  inject,
  signal,
} from '@angular/core';
import { ActivatedRoute, Router } from '@angular/router';

import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideMail,
  lucidePause,
  lucidePlay,
  lucidePlus,
  lucideRefreshCw,
  lucideUnplug,
  lucideX,
} from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardProgressComponent } from '@/shared/components/progress';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ConfirmService } from '../core/confirm.service';
import { EventsService } from '../core/events.service';
import {
  accountBadge,
  accountNote,
  categories,
  categoryLabel,
  connectNotice,
  mailProgress,
  relativeTime,
  type MailAccount,
  type MailCategory,
  type Tone,
} from './format';

type Message = Schemas['MailMessage'];

const toneClass: Record<Tone, string> = {
  ok: 'bg-emerald-500/10 text-emerald-700 dark:text-emerald-400',
  busy: 'bg-sky-500/10 text-sky-700 dark:text-sky-400',
  warn: 'bg-amber-500/10 text-amber-700 dark:text-amber-400',
  muted: 'bg-muted text-muted-foreground',
};
const dotClass: Record<Tone, string> = {
  ok: 'bg-emerald-500',
  busy: 'bg-sky-500 animate-pulse',
  warn: 'bg-amber-500',
  muted: 'bg-muted-foreground/50',
};
const categoryClass: Record<MailCategory, string> = {
  received: 'bg-muted text-foreground',
  assessment: 'bg-sky-500/10 text-sky-700 dark:text-sky-400',
  interview: 'bg-violet-500/10 text-violet-700 dark:text-violet-400',
  rejection: 'bg-destructive/10 text-destructive',
  offer: 'bg-emerald-500/10 text-emerald-700 dark:text-emerald-400',
};

/** Account and message events arrive in bursts while a backlog is classified; refetch at most this often. */
const REFRESH_MS = 800;

@Component({
  selector: 'app-mail-page',
  imports: [DatePipe, NgIcon, ZardButtonComponent, ZardProgressComponent],
  providers: [
    provideIcons({
      lucideMail,
      lucidePause,
      lucidePlay,
      lucidePlus,
      lucideRefreshCw,
      lucideUnplug,
      lucideX,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-5xl space-y-6 px-4 py-6">
      <header>
        <h1 class="text-xl font-semibold tracking-tight">Mail</h1>
        <p class="text-sm text-muted-foreground">
          Connect Gmail and zapply picks out the mail about your applications: confirmations,
          assessments, interviews, rejections and offers.
        </p>
      </header>

      @if (notice(); as n) {
        <div
          [attr.role]="n.kind === 'error' ? 'alert' : 'status'"
          class="flex items-start justify-between gap-3 rounded-lg border p-3 text-sm"
          [class.border-destructive]="n.kind === 'error'"
          [class.text-destructive]="n.kind === 'error'"
        >
          <p>{{ n.text }}</p>
          <button
            z-button
            zType="ghost"
            zSize="icon-xs"
            type="button"
            aria-label="Dismiss"
            (click)="notice.set(null)"
          >
            <ng-icon name="lucideX" />
          </button>
        </div>
      }
      @if (error()) {
        <div
          role="alert"
          class="flex items-start justify-between gap-3 rounded-lg border border-destructive p-3 text-sm text-destructive"
        >
          <p>{{ error() }}</p>
          <button
            z-button
            zType="ghost"
            zSize="icon-xs"
            type="button"
            aria-label="Dismiss"
            (click)="error.set('')"
          >
            <ng-icon name="lucideX" />
          </button>
        </div>
      }

      <section class="space-y-4 rounded-xl border bg-card p-5" aria-labelledby="accounts-title">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="space-y-1">
            <h2 id="accounts-title" class="font-medium">Mailboxes</h2>
            @if (configured()) {
              <p class="max-w-xl text-sm text-muted-foreground">
                You can connect more than one Gmail account. Each one is read on its own and adds to
                the same list below. Connecting an address that is already here reconnects it.
              </p>
            }
          </div>
          @if (configured()) {
            <button
              z-button
              [zType]="live().length ? 'outline' : 'default'"
              [zDisabled]="busy()"
              (click)="connect()"
            >
              <ng-icon name="lucidePlus" />
              {{ live().length ? 'Add another Gmail account' : 'Connect Gmail' }}
            </button>
          }
        </div>

        @if (!configured() && loaded()) {
          <p class="text-sm text-muted-foreground">
            Mail is not set up on this server. An administrator needs to add a Google OAuth client.
          </p>
        }

        @for (a of live(); track a.id) {
          <article
            class="space-y-3 rounded-lg border bg-background p-4"
            [attr.aria-label]="a.address"
          >
            <div class="flex flex-wrap items-center gap-x-3 gap-y-1">
              <p class="min-w-0 truncate text-base font-medium">{{ a.address }}</p>
              <span
                class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium"
                [class]="tone(a).pill"
              >
                <span class="size-1.5 rounded-full" [class]="tone(a).dot"></span>
                {{ badge(a).label }}
              </span>
              @if (a.last_synced_at && a.status === 'active') {
                <span class="ml-auto text-xs text-muted-foreground">
                  Checked {{ ago(a.last_synced_at) }}
                </span>
              }
            </div>

            @if (progress(a); as p) {
              @if (p.working) {
                <div class="space-y-1.5">
                  <z-progress [value]="p.percent" class="h-1.5" />
                  <p class="text-sm tabular-nums text-muted-foreground" aria-live="polite">
                    {{ p.done }}/{{ p.total }} mails classified
                    @if (a.status === 'paused') {
                      <span>· paused</span>
                    }
                  </p>
                </div>
              } @else if (p.total > 0) {
                <p class="text-sm text-muted-foreground">
                  {{ p.total }} {{ p.total === 1 ? 'mail' : 'mails' }} classified ·
                  {{ a.mails_relevant }} about your applications
                </p>
              } @else if (a.status === 'active') {
                <p class="text-sm text-muted-foreground">
                  Looking for mail about your applications…
                </p>
              }
            }

            @if (note(a); as text) {
              <p
                class="text-sm"
                [class.text-amber-700]="badge(a).tone === 'warn'"
                [class.dark:text-amber-400]="badge(a).tone === 'warn'"
                [class.text-muted-foreground]="badge(a).tone !== 'warn'"
                [attr.role]="badge(a).tone === 'warn' ? 'status' : null"
              >
                {{ text }}
              </p>
            }

            <div class="flex flex-wrap items-center gap-2 pt-1">
              @switch (a.status) {
                @case ('needs_reconnect') {
                  <button z-button [zDisabled]="busy()" (click)="connect()">
                    <ng-icon name="lucideRefreshCw" /> Reconnect
                  </button>
                }
                @case ('paused') {
                  <button z-button zType="outline" [zDisabled]="busy()" (click)="pause(a, false)">
                    <ng-icon name="lucidePlay" /> Resume
                  </button>
                }
                @default {
                  <button z-button zType="outline" [zDisabled]="busy()" (click)="pause(a, true)">
                    <ng-icon name="lucidePause" /> Pause
                  </button>
                  @if (!a.retry_at) {
                    <button z-button zType="outline" [zDisabled]="busy()" (click)="syncNow(a)">
                      <ng-icon name="lucideRefreshCw" /> Check now
                    </button>
                  }
                }
              }
              <button
                z-button
                zType="destructive"
                class="ml-auto"
                [zDisabled]="busy()"
                (click)="disconnect(a)"
              >
                <ng-icon name="lucideUnplug" /> Disconnect
              </button>
            </div>
          </article>
        } @empty {
          @if (configured() && loaded()) {
            <div
              class="flex flex-col items-center gap-2 rounded-lg border border-dashed py-8 text-center"
            >
              <ng-icon name="lucideMail" class="size-6 text-muted-foreground" />
              <p class="text-sm font-medium">No mailbox connected</p>
              <p class="max-w-sm text-sm text-muted-foreground">
                Connect a Gmail account and your application updates will show up below as they
                arrive.
              </p>
            </div>
          }
        }

        <details class="text-xs text-muted-foreground">
          <summary class="cursor-pointer select-none">How zapply handles your mail</summary>
          <p class="mt-2 max-w-3xl">
            zapply reads each new inbox message's sender, subject and a short preview, and sends
            them to a language model to see whether they concern an application. Mail that does not
            is dropped on the spot: only its identifier is kept so it is never read twice. Mail that
            does is kept with its preview. Message bodies are never stored. Pause stops all reading
            and keeps the connection. Disconnect revokes zapply's access to the mailbox; the updates
            already found stay here.
          </p>
        </details>
      </section>

      <section class="space-y-3" aria-labelledby="inbox-title">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <h2 id="inbox-title" class="font-medium">Application updates</h2>
          <div class="flex flex-wrap gap-2" role="group" aria-label="Filter by category">
            <button
              z-button
              [zType]="category() === '' ? 'default' : 'outline'"
              zSize="sm"
              [attr.aria-pressed]="category() === ''"
              (click)="filter('')"
            >
              All
            </button>
            @for (c of categories; track c.value) {
              <button
                z-button
                [zType]="category() === c.value ? 'default' : 'outline'"
                zSize="sm"
                [attr.aria-pressed]="category() === c.value"
                (click)="filter(c.value)"
              >
                {{ c.label }}
              </button>
            }
          </div>
        </div>
        <ul class="divide-y rounded-lg border">
          @for (m of messages(); track m.id) {
            <li class="space-y-1 p-4">
              <div class="flex flex-wrap items-center gap-2 text-sm">
                <span
                  class="rounded-full px-2 py-0.5 text-xs font-medium"
                  [class]="catClass(m.category)"
                  >{{ label(m.category) }}</span
                >
                <span class="truncate font-medium">{{ m.from }}</span>
                <time class="ml-auto text-muted-foreground" [attr.datetime]="m.received_at">{{
                  m.received_at | date: 'medium'
                }}</time>
              </div>
              <p class="font-medium">{{ m.subject || '(no subject)' }}</p>
              <p class="text-sm text-muted-foreground">{{ m.snippet }}</p>
              <p class="flex flex-wrap gap-x-3 text-xs text-muted-foreground">
                @if (m.job_id && jobTitles()[m.job_id]) {
                  <span>Job: {{ jobTitles()[m.job_id] }}</span>
                }
                @if (live().length > 1 && addresses()[m.account_id]) {
                  <span>Mailbox: {{ addresses()[m.account_id] }}</span>
                }
              </p>
            </li>
          } @empty {
            <li class="p-6 text-center text-sm text-muted-foreground">
              @if (loadingMessages()) {
                Loading…
              } @else if (category()) {
                No {{ categoryName().toLowerCase() }} updates yet.
              } @else if (working()) {
                Still reading your mail. Updates appear here as they are classified.
              } @else if (loaded() && !live().length) {
                Connect a Gmail account to see updates here.
              } @else {
                Nothing yet. Updates appear here as they arrive.
              }
            </li>
          }
        </ul>
        @if (more()) {
          <button z-button zType="outline" [zDisabled]="loadingMessages()" (click)="older()">
            Older
          </button>
        }
      </section>
    </div>
  `,
})
export class MailPage {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  private readonly confirm = inject(ConfirmService);
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);

  protected readonly categories = categories;
  protected readonly label = categoryLabel;
  protected readonly badge = accountBadge;
  protected readonly progress = mailProgress;
  protected readonly catClass = (c: MailCategory): string => categoryClass[c];
  protected readonly tone = (a: MailAccount): { pill: string; dot: string } => {
    const t = accountBadge(a).tone;
    return { pill: toneClass[t], dot: dotClass[t] };
  };
  protected readonly now = signal(Date.now());
  protected readonly ago = (iso: string): string => relativeTime(iso, this.now());
  protected readonly note = (a: MailAccount): string => accountNote(a, this.now());
  protected readonly notice = signal(
    connectNotice(this.route.snapshot.queryParams as Record<string, string | undefined>),
  );
  protected readonly accounts = signal<MailAccount[]>([]);
  protected readonly configured = signal(false);
  protected readonly loaded = signal(false);
  protected readonly messages = signal<Message[]>([]);
  protected readonly jobTitles = signal<Record<string, string>>({});
  protected readonly category = signal<MailCategory | ''>('');
  protected readonly more = signal(false);
  protected readonly busy = signal(false);
  protected readonly loadingMessages = signal(false);
  protected readonly error = signal('');
  /** Mailboxes that are still connected, in the order they were added. */
  protected readonly live = computed(() =>
    this.accounts().filter((a) => a.status !== 'disconnected'),
  );
  protected readonly addresses = computed(() =>
    Object.fromEntries(this.accounts().map((a) => [a.id, a.address])),
  );
  protected readonly working = computed(() =>
    this.live().some((a) => a.status === 'active' && mailProgress(a).working),
  );
  protected readonly categoryName = computed(() => {
    const c = this.category();
    return c ? categoryLabel(c) : '';
  });
  private generation = 0;
  private refreshTimer: ReturnType<typeof setTimeout> | null = null;
  private stale = { accounts: false, messages: false };

  constructor() {
    const q = this.route.snapshot.queryParams as Record<string, string | undefined>;
    if (q['oauth_state'] && q['oauth_code']) void this.complete(q['oauth_state'], q['oauth_code']);
    effect(() => {
      this.auth.identity();
      this.accounts.set([]);
      this.messages.set([]);
      void this.load();
    });
    const change = this.events.changes.subscribe((e) => {
      if (e.topic === 'mail.account.updated') this.refreshSoon('accounts');
      if (e.topic === 'mail.message.classified') this.refreshSoon('messages');
    });
    const resync = this.events.resync.subscribe(() => void this.load());
    // Keeps "checked 5 min ago" and the retry countdown honest between events.
    const tick = setInterval(() => this.now.set(Date.now()), 30_000);
    inject(DestroyRef).onDestroy(() => {
      this.generation++;
      clearInterval(tick);
      if (this.refreshTimer) clearTimeout(this.refreshTimer);
      change.unsubscribe();
      resync.unsubscribe();
    });
  }

  private refreshSoon(what: 'accounts' | 'messages'): void {
    this.stale[what] = true;
    if (this.refreshTimer) return;
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = null;
      const { accounts, messages } = this.stale;
      this.stale = { accounts: false, messages: false };
      if (accounts) void this.loadAccounts();
      if (messages) void this.loadMessages();
    }, REFRESH_MS);
  }

  private async load(): Promise<void> {
    await Promise.all([this.loadAccounts(), this.loadMessages()]);
  }

  private fail(e: unknown, fallback: string): void {
    this.error.set(e instanceof Error ? e.message : fallback);
  }

  async loadAccounts(): Promise<void> {
    const identity = this.auth.identity();
    if (!identity) return;
    try {
      const list = Api.ok(await this.api.client.GET('/v1/mail/accounts'));
      if (identity !== this.auth.identity()) return;
      this.configured.set(list.configured);
      this.accounts.set(list.items);
      this.loaded.set(true);
    } catch (e) {
      if (identity === this.auth.identity()) this.fail(e, 'Could not load mailboxes.');
    }
  }

  async loadMessages(append = false): Promise<void> {
    const identity = this.auth.identity(),
      generation = ++this.generation;
    if (!identity) return;
    const before = append ? this.messages().at(-1)?.id : undefined;
    this.loadingMessages.set(true);
    try {
      const category = this.category();
      const page = Api.ok(
        await this.api.client.GET('/v1/mail/messages', {
          params: {
            query: { limit: 50, ...(category && { category }), ...(before && { before }) },
          },
        }),
      );
      if (identity !== this.auth.identity() || generation !== this.generation) return;
      this.messages.update((rows) => (append ? [...rows, ...page.items] : page.items));
      this.more.set(!!page.next_cursor);
      void this.loadJobTitles(page.items);
    } catch (e) {
      if (identity === this.auth.identity() && generation === this.generation)
        this.fail(e, 'Could not load mail.');
    } finally {
      if (identity === this.auth.identity() && generation === this.generation)
        this.loadingMessages.set(false);
    }
  }

  private async loadJobTitles(items: Message[]): Promise<void> {
    const known = this.jobTitles();
    const ids = [...new Set(items.map((m) => m.job_id).filter((id): id is string => !!id))].filter(
      (id) => !(id in known),
    );
    const found = await Promise.all(
      ids.map(async (id) => {
        try {
          const job = Api.ok(
            await this.api.client.GET('/v1/jobs/{id}', { params: { path: { id } } }),
          );
          return [id, job.company ? `${job.title} at ${job.company}` : job.title] as const;
        } catch {
          return null; // deleted since: the link is already cleared
        }
      }),
    );
    this.jobTitles.update((t) => ({
      ...t,
      ...Object.fromEntries(found.filter((x): x is readonly [string, string] => !!x)),
    }));
  }

  filter(c: MailCategory | ''): void {
    this.category.set(c);
    void this.loadMessages();
  }

  older(): void {
    void this.loadMessages(true);
  }

  /**
   * Google returned here with a code. The server finishes the flow as the
   * signed-in member, so a link forwarded to someone else cannot attach their
   * mailbox to this account. The code leaves the address bar at once.
   */
  private async complete(state: string, code: string): Promise<void> {
    void this.router.navigate([], { queryParams: {}, replaceUrl: true });
    this.busy.set(true);
    try {
      Api.ok(
        await this.api.client.POST('/v1/mail/accounts/google/complete', { body: { state, code } }),
      );
      this.notice.set(connectNotice({ connected: '1' }));
      await this.loadAccounts();
    } catch (e) {
      this.notice.set(
        connectNotice({
          error:
            e instanceof ApiError && e.status === 422
              ? 'expired'
              : e instanceof ApiError && e.status === 409
                ? 'in_use'
                : 'failed',
        }),
      );
    } finally {
      this.busy.set(false);
    }
  }

  async connect(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const { url } = Api.ok(await this.api.client.POST('/v1/mail/accounts/google/start'));
      window.location.assign(url);
    } catch (e) {
      this.fail(e, 'Could not start the Gmail connection.');
      this.busy.set(false);
    }
  }

  async syncNow(a: MailAccount): Promise<void> {
    await this.act(async () => {
      Api.ok(
        await this.api.client.POST('/v1/mail/accounts/{id}/sync', {
          params: { path: { id: a.id } },
        }),
      );
    });
  }

  async pause(a: MailAccount, paused: boolean): Promise<void> {
    await this.act(async () => {
      Api.ok(
        await this.api.client.PATCH('/v1/mail/accounts/{id}', {
          params: { path: { id: a.id } },
          body: { paused },
        }),
      );
    });
  }

  async disconnect(a: MailAccount): Promise<void> {
    const ok = await this.confirm.ask({
      title: `Disconnect ${a.address}?`,
      description:
        'zapply stops reading this mailbox and revokes its Google access. The application updates already found stay here, and you can connect it again later. To stop reading without revoking access, pause it instead.',
      okText: 'Disconnect',
    });
    if (!ok) return;
    await this.act(async () => {
      Api.ok(
        await this.api.client.DELETE('/v1/mail/accounts/{id}', { params: { path: { id: a.id } } }),
      );
    });
  }

  private async act(fn: () => Promise<void>): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await fn();
      await this.loadAccounts();
    } catch (e) {
      this.fail(e, 'That did not work. Try again.');
    } finally {
      this.busy.set(false);
    }
  }
}
