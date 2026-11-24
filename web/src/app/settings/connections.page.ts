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
  lucideBot,
  lucideHistory,
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
import { Api, ApiError } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ConfirmService } from '../core/confirm.service';
import { EventsService } from '../core/events.service';
import {
  accountBadge,
  accountNote,
  connectNotice,
  mailProgress,
  relativeTime,
  type MailAccount,
  type Tone,
} from '../mail/format';

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
/** Account events arrive in bursts while a backlog is classified; refetch at most this often. */
const REFRESH_MS = 800;

@Component({
  selector: 'app-connections-page',
  imports: [
    DatePipe,
    NgIcon,
    ZardButtonComponent,
    ZardDialogImports,
    ZardInputComponent,
    ZardProgressComponent,
  ],
  providers: [
    provideIcons({
      lucideBot,
      lucideHistory,
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
  templateUrl: './connections.page.html',
  styleUrls: ['../mail/mail.css', './connections.page.css'],
})
/** Settings › Connections: the accounts and services zapply reads on the member's behalf. */
export class ConnectionsPage {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  private readonly confirm = inject(ConfirmService);
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);

  protected readonly badge = accountBadge;
  protected readonly progress = mailProgress;
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
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  /** Mailboxes that are still connected, in the order they were added. */
  protected readonly live = computed(() =>
    this.accounts().filter((a) => a.status !== 'disconnected'),
  );
  protected readonly rescanPresets = [7, 30, 60, 90];
  protected readonly rescanOpen = signal(false);
  protected readonly rescanning = signal<MailAccount | null>(null);
  protected readonly rescanDays = signal(60);
  protected readonly rescanValid = computed(() => {
    const d = this.rescanDays();
    return Number.isInteger(d) && d >= 1 && d <= 90;
  });
  protected readonly rescanSince = computed(() => this.now() - this.rescanDays() * 86_400_000);
  private refreshTimer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    const q = this.route.snapshot.queryParams as Record<string, string | undefined>;
    if (q['oauth_state'] && q['oauth_code']) void this.complete(q['oauth_state'], q['oauth_code']);
    effect(() => {
      this.auth.identity();
      this.accounts.set([]);
      void this.loadAccounts();
    });
    const change = this.events.changes.subscribe((e) => {
      if (e.topic === 'mail.account.updated') this.refreshSoon();
    });
    const resync = this.events.resync.subscribe(() => void this.loadAccounts());
    // Keeps "checked 5 min ago" and the retry countdown honest between events.
    const tick = setInterval(() => this.now.set(Date.now()), 30_000);
    inject(DestroyRef).onDestroy(() => {
      clearInterval(tick);
      if (this.refreshTimer) clearTimeout(this.refreshTimer);
      change.unsubscribe();
      resync.unsubscribe();
    });
  }

  private refreshSoon(): void {
    if (this.refreshTimer) return;
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = null;
      void this.loadAccounts();
    }, REFRESH_MS);
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
        text: `Scanning the last ${days} days of ${a.address}. New updates appear in Notifications as they are classified.`,
      });
    });
  }

  async disconnect(a: MailAccount): Promise<void> {
    const ok = await this.confirm.ask({
      title: `Disconnect ${a.address}?`,
      description:
        'zapply stops reading this mailbox and revokes its Google access. The application updates already found stay in Notifications, and you can connect it again later. To stop reading without revoking access, pause it instead.',
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
