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
import { RouterLink } from '@angular/router';

import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideBriefcase,
  lucideCheck,
  lucideCheckCheck,
  lucideExternalLink,
  lucideInbox,
  lucidePlug,
  lucideX,
} from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { NotificationsService } from '../core/notifications.service';
import {
  categories,
  categoryLabel,
  dayLabel,
  parseSender,
  type MailAccount,
  type MailCategory,
} from '../mail/format';
import { MailStatusChange } from '../mail/status-change';

type Notification = Schemas['Notification'];
type Message = Schemas['MailMessage'];

/** Notifications arrive in bursts while a backlog is classified; refetch at most this often. */
const REFRESH_MS = 800;

/**
 * The member's notification inbox. Today every notification is a mail about an
 * application; other kinds slot in as new `kind`s with their own row layout.
 */
@Component({
  selector: 'app-notifications-page',
  imports: [DatePipe, MailStatusChange, NgIcon, RouterLink, ZardButtonComponent],
  providers: [
    provideIcons({
      lucideBriefcase,
      lucideCheck,
      lucideCheckCheck,
      lucideExternalLink,
      lucideInbox,
      lucidePlug,
      lucideX,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './notifications.page.html',
  styleUrls: ['../mail/mail.css', './notifications.page.css'],
})
export class NotificationsPage {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  protected readonly notes = inject(NotificationsService);

  protected readonly categories = categories;
  protected readonly label = categoryLabel;
  protected readonly sender = parseSender;
  protected readonly now = signal(Date.now());
  protected readonly items = signal<Notification[]>([]);
  protected readonly accounts = signal<MailAccount[]>([]);
  protected readonly jobTitles = signal<Record<string, string>>({});
  protected readonly category = signal<MailCategory | ''>('');
  protected readonly unreadOnly = signal(false);
  protected readonly more = signal(false);
  protected readonly loading = signal(false);
  protected readonly loaded = signal(false);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly live = computed(() =>
    this.accounts().filter((a) => a.status !== 'disconnected'),
  );
  protected readonly addresses = computed(() =>
    Object.fromEntries(this.accounts().map((a) => [a.id, a.address])),
  );
  protected readonly categoryName = computed(() => {
    const c = this.category();
    return c ? categoryLabel(c) : '';
  });
  /** Notifications grouped under Today, Yesterday or their date, newest first as loaded. */
  protected readonly groups = computed(() => {
    const out: { day: string; items: Notification[] }[] = [];
    for (const n of this.items()) {
      const day = dayLabel(n.occurred_at, this.now());
      const last = out.at(-1);
      if (last?.day === day) last.items.push(n);
      else out.push({ day, items: [n] });
    }
    return out;
  });
  private generation = 0;
  private refreshTimer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    effect(() => {
      this.auth.identity();
      this.items.set([]);
      void this.load();
      void this.loadAccounts();
    });
    const change = this.events.changes.subscribe((e) => {
      if (e.topic === 'mail.message.classified' || e.topic === 'notification.updated')
        this.refreshSoon();
    });
    const resync = this.events.resync.subscribe(() => void this.load());
    const tick = setInterval(() => this.now.set(Date.now()), 60_000);
    inject(DestroyRef).onDestroy(() => {
      this.generation++;
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
      void this.load();
    }, REFRESH_MS);
  }

  private async loadAccounts(): Promise<void> {
    const identity = this.auth.identity();
    if (!identity) return;
    try {
      const list = Api.ok(await this.api.client.GET('/v1/mail/accounts'));
      if (identity === this.auth.identity()) this.accounts.set(list.items);
    } catch {
      // Only used to label rows with their mailbox; the list still works without it.
    }
  }

  async load(append = false): Promise<void> {
    const identity = this.auth.identity(),
      generation = ++this.generation;
    if (!identity) return;
    const before = append ? this.items().at(-1)?.id : undefined;
    this.loading.set(true);
    try {
      const category = this.category();
      const page = Api.ok(
        await this.api.client.GET('/v1/notifications', {
          params: {
            query: {
              limit: 50,
              ...(category && { category }),
              ...(this.unreadOnly() && { unread: true }),
              ...(before && { before }),
            },
          },
        }),
      );
      if (identity !== this.auth.identity() || generation !== this.generation) return;
      this.items.update((rows) => (append ? [...rows, ...page.items] : page.items));
      this.more.set(!!page.next_cursor);
      this.loaded.set(true);
      void this.loadJobTitles(page.items);
    } catch (e) {
      if (identity === this.auth.identity() && generation === this.generation)
        this.error.set(e instanceof Error ? e.message : 'Could not load notifications.');
    } finally {
      if (identity === this.auth.identity() && generation === this.generation)
        this.loading.set(false);
    }
  }

  private async loadJobTitles(items: Notification[]): Promise<void> {
    const known = this.jobTitles();
    const ids = [
      ...new Set(items.map((n) => n.mail?.job_id).filter((id): id is string => !!id)),
    ].filter((id) => !(id in known));
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
    void this.load();
  }

  show(unread: boolean): void {
    this.unreadOnly.set(unread);
    void this.load();
  }

  older(): void {
    void this.load(true);
  }

  protected replaceMail(n: Notification, m: Message): void {
    this.items.update((list) => list.map((x) => (x.id === n.id ? { ...x, mail: m } : x)));
  }

  /** Marks one read in place, so it doesn't vanish from under the pointer in the unread view. */
  async read(n: Notification): Promise<void> {
    if (n.read_at) return;
    const at = new Date().toISOString();
    this.items.update((list) => list.map((x) => (x.id === n.id ? { ...x, read_at: at } : x)));
    try {
      await this.notes.markRead([n.id]);
    } catch (e) {
      this.items.update((list) => list.map((x) => (x.id === n.id ? { ...x, read_at: null } : x)));
      this.error.set(e instanceof Error ? e.message : 'Could not mark it read.');
    }
  }

  async readAll(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.notes.markAllRead();
      await this.load();
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not mark them read.');
    } finally {
      this.busy.set(false);
    }
  }
}
