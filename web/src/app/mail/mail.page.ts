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
import { ActivatedRoute, Router, RouterLink } from '@angular/router';

import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideBriefcase,
  lucideHistory,
  lucideInbox,
  lucideMail,
  lucidePause,
  lucidePlay,
  lucidePlus,
  lucideRefreshCw,
  lucideUnplug,
  lucideX,
} from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardDialogImports } from '@/shared/components/dialog';
import { ZardInputComponent } from '@/shared/components/input';
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
  dayLabel,
  mailProgress,
  parseSender,
  relativeTime,
  type MailAccount,
  type MailCategory,
  type Tone,
} from './format';
import { MailStatusChange } from './status-change';

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
/** Account and message events arrive in bursts while a backlog is classified; refetch at most this often. */
const REFRESH_MS = 800;

@Component({
  selector: 'app-mail-page',
  imports: [
    DatePipe,
    MailStatusChange,
    NgIcon,
    RouterLink,
    ZardButtonComponent,
    ZardDialogImports,
    ZardInputComponent,
    ZardProgressComponent,
  ],
  providers: [
    provideIcons({
      lucideBriefcase,
      lucideHistory,
      lucideInbox,
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
  templateUrl: './mail.page.html',
  styleUrl: './mail.page.css',
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
  protected readonly sender = parseSender;
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
  /** Messages grouped under Today, Yesterday or their date, newest first as loaded. */
  protected readonly groups = computed(() => {
    const out: { day: string; items: Message[] }[] = [];
    for (const m of this.messages()) {
      const day = dayLabel(m.received_at, this.now());
      const last = out.at(-1);
      if (last?.day === day) last.items.push(m);
      else out.push({ day, items: [m] });
    }
    return out;
  });
  protected readonly rescanPresets = [7, 30, 60, 90];
  protected readonly rescanOpen = signal(false);
  protected readonly rescanning = signal<MailAccount | null>(null);
  protected readonly rescanDays = signal(60);
  protected readonly rescanValid = computed(() => {
    const d = this.rescanDays();
    return Number.isInteger(d) && d >= 1 && d <= 90;
  });
  protected readonly rescanSince = computed(() => this.now() - this.rescanDays() * 86_400_000);
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

  protected replace(m: Message): void {
    this.messages.update((list) => list.map((x) => (x.id === m.id ? m : x)));
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

  protected openRescan(a: MailAccount): void {
    this.rescanning.set(a);
    this.rescanOpen.set(true);
  }

  protected setRescanDays(e: Event): void {
    this.rescanDays.set((e.target as HTMLInputElement).valueAsNumber);
  }

  async rescan(a: MailAccount): Promise<void> {
    if (!this.rescanValid()) return;
    const days = this.rescanDays();
    this.rescanOpen.set(false);
    await this.act(async () => {
      Api.ok(
        await this.api.client.POST('/v1/mail/accounts/{id}/rescan', {
          params: { path: { id: a.id } },
          body: { days },
        }),
      );
      this.notice.set({
        kind: 'ok',
        text: `Scanning the last ${days} days of ${a.address}. New updates appear below as they are classified.`,
      });
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
