import { DatePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  effect,
  inject,
  signal,
} from '@angular/core';
import { ActivatedRoute, Router } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ConfirmService } from '../core/confirm.service';
import { EventsService } from '../core/events.service';
import {
  accountStatus,
  categories,
  categoryLabel,
  connectNotice,
  type MailAccount,
  type MailCategory,
} from './format';
import { MailStatusChange } from './status-change';

type Message = Schemas['MailMessage'];

@Component({
  selector: 'app-mail-page',
  imports: [DatePipe, MailStatusChange, ZardButtonComponent],
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
        <p
          [attr.role]="n.kind === 'error' ? 'alert' : 'status'"
          class="rounded-lg border p-3 text-sm"
          [class.border-destructive]="n.kind === 'error'"
          [class.text-destructive]="n.kind === 'error'"
        >
          {{ n.text }}
        </p>
      }
      @if (error()) {
        <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">
          {{ error() }}
        </p>
      }

      <section class="space-y-3 rounded-xl border bg-card p-5" aria-labelledby="accounts-title">
        <div class="flex items-start justify-between gap-4">
          <h2 id="accounts-title" class="font-medium">Connected mailboxes</h2>
          @if (configured()) {
            <button z-button zType="outline" [zDisabled]="busy()" (click)="connect()">
              Connect Gmail
            </button>
          }
        </div>
        @if (!configured() && loaded()) {
          <p class="text-sm text-muted-foreground">
            Mail is not set up on this server. An administrator needs to add a Google OAuth client.
          </p>
        }
        @for (a of accounts(); track a.id) {
          @if (a.status !== 'disconnected') {
            <div class="flex flex-wrap items-center justify-between gap-3 border-t pt-3">
              <div class="min-w-0">
                <p class="truncate font-medium">{{ a.address }}</p>
                <p class="text-sm text-muted-foreground">
                  {{ status(a) }}
                  @if (a.last_synced_at && a.status === 'active') {
                    · checked {{ a.last_synced_at | date: 'short' }}
                  }
                </p>
              </div>
              <div class="flex gap-2">
                @if (a.status === 'needs_reconnect') {
                  <button z-button zType="outline" [zDisabled]="busy()" (click)="connect()">
                    Reconnect
                  </button>
                } @else {
                  <button
                    z-button
                    zType="outline"
                    [zDisabled]="busy()"
                    (click)="pause(a, a.status === 'active')"
                  >
                    {{ a.status === 'active' ? 'Pause' : 'Resume' }}
                  </button>
                }
                <button z-button zType="outline" [zDisabled]="busy()" (click)="disconnect(a)">
                  Disconnect
                </button>
              </div>
            </div>
          }
        } @empty {
          @if (configured() && loaded()) {
            <p class="text-sm text-muted-foreground">No mailbox connected yet.</p>
          }
        }
        <p class="text-xs text-muted-foreground">
          zapply reads each new inbox message's sender, subject and a short preview, and sends them
          to a language model to see whether they concern an application. Mail that does not is
          dropped on the spot: only its identifier is kept so it is never read twice. Mail that does
          is kept with its preview. Message bodies are never stored. Disconnecting revokes access.
        </p>
      </section>

      <section class="space-y-3" aria-labelledby="inbox-title">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <h2 id="inbox-title" class="font-medium">Application updates</h2>
          <div class="flex flex-wrap gap-2" role="group" aria-label="Filter by category">
            <button
              z-button
              [zType]="category() === '' ? 'default' : 'outline'"
              zSize="sm"
              (click)="filter('')"
            >
              All
            </button>
            @for (c of categories; track c.value) {
              <button
                z-button
                [zType]="category() === c.value ? 'default' : 'outline'"
                zSize="sm"
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
                <span class="rounded-full bg-muted px-2 py-0.5 text-xs font-medium">{{
                  label(m.category)
                }}</span>
                <span class="truncate font-medium">{{ m.from }}</span>
                <time class="ml-auto text-muted-foreground" [attr.datetime]="m.received_at">{{
                  m.received_at | date: 'medium'
                }}</time>
              </div>
              <p class="font-medium">{{ m.subject || '(no subject)' }}</p>
              <p class="text-sm text-muted-foreground">{{ m.snippet }}</p>
              @if (m.job_id && jobTitles()[m.job_id]) {
                <p class="text-xs text-muted-foreground">Job: {{ jobTitles()[m.job_id] }}</p>
              }
              <app-mail-status-change [message]="m" (changed)="replace($event)" />
            </li>
          } @empty {
            <li class="p-6 text-center text-sm text-muted-foreground">
              @if (loadingMessages()) {
                Loading…
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
  protected readonly status = accountStatus;
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
  private generation = 0;

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
      if (e.topic === 'mail.account.updated') void this.loadAccounts();
      if (e.topic === 'mail.message.classified') void this.loadMessages();
    });
    const resync = this.events.resync.subscribe(() => void this.load());
    inject(DestroyRef).onDestroy(() => {
      this.generation++;
      change.unsubscribe();
      resync.unsubscribe();
    });
  }

  protected replace(m: Message): void {
    this.messages.update((list) => list.map((x) => (x.id === m.id ? m : x)));
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
        'zapply stops reading this mailbox and revokes its access. Application updates already found stay here.',
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
