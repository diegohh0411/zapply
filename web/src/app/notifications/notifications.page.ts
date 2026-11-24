import {
  ChangeDetectionStrategy,
  Component,
  computed,
  DestroyRef,
  effect,
  inject,
  signal,
} from '@angular/core';
import { Router, RouterLink } from '@angular/router';

import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideArchive,
  lucideArchiveRestore,
  lucideBellOff,
  lucideBookmarkPlus,
  lucideBriefcase,
  lucideCheck,
  lucideCheckCheck,
  lucideExternalLink,
  lucideInbox,
  lucideMail,
  lucideMailOpen,
  lucideRadar,
  lucideX,
} from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { NotificationsService } from '../core/notifications.service';
import { categoryLabel, dayLabel, parseSender, relativeTime } from '../mail/format';
import { MailStatusChange } from '../mail/status-change';

type Notification = Schemas['Notification'];
type Message = Schemas['MailMessage'];
type View = 'inbox' | 'unread' | 'archived';

/** Notifications arrive in bursts while a backlog is classified; refetch at most this often. */
const REFRESH_MS = 800;

/** What a mail notification says happened, as a short verb phrase after the sender. */
const mailVerb: Record<string, string> = {
  received: 'confirmed your application',
  assessment: 'sent an assessment',
  interview: 'wants to talk',
  rejection: 'sent a decision',
  offer: 'sent an offer',
};

/**
 * The notification inbox. Rows read "who did what" with the thing it is about
 * underneath, like Notion's and Linear's inboxes. Each kind renders its own row:
 * mail about an application, or a new opening on a watched job board.
 */
@Component({
  selector: 'app-notifications-page',
  imports: [MailStatusChange, NgIcon, RouterLink, ZardButtonComponent],
  providers: [
    provideIcons({
      lucideArchive,
      lucideArchiveRestore,
      lucideBellOff,
      lucideBookmarkPlus,
      lucideBriefcase,
      lucideCheck,
      lucideCheckCheck,
      lucideExternalLink,
      lucideInbox,
      lucideMail,
      lucideMailOpen,
      lucideRadar,
      lucideX,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './notifications.page.html',
  styleUrl: './notifications.page.css',
})
export class NotificationsPage {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  private readonly router = inject(Router);
  protected readonly notes = inject(NotificationsService);

  protected readonly label = categoryLabel;
  protected readonly sender = parseSender;
  protected readonly verb = (m: Message): string => mailVerb[m.category] ?? 'sent an update';
  protected readonly ago = (iso: string): string => relativeTime(iso, this.now());
  protected readonly now = signal(Date.now());
  protected readonly view = signal<View>('inbox');
  protected readonly items = signal<Notification[]>([]);
  protected readonly jobTitles = signal<Record<string, string>>({});
  protected readonly hasMailbox = signal(true);
  protected readonly more = signal(false);
  protected readonly loading = signal(false);
  protected readonly loaded = signal(false);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly views: { value: View; label: string }[] = [
    { value: 'inbox', label: 'Inbox' },
    { value: 'unread', label: 'Unread' },
    { value: 'archived', label: 'Archived' },
  ];
  protected readonly hasRead = computed(() => this.items().some((n) => n.read_at));
  /** Grouped under Today, Yesterday or their date, newest first as loaded. */
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
      void this.checkMailbox();
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

  /** Only decides whether the empty inbox suggests connecting Gmail. */
  private async checkMailbox(): Promise<void> {
    try {
      const list = Api.ok(await this.api.client.GET('/v1/mail/accounts'));
      this.hasMailbox.set(list.items.some((a) => a.status !== 'disconnected'));
    } catch {
      this.hasMailbox.set(true);
    }
  }

  async load(append = false): Promise<void> {
    const identity = this.auth.identity(),
      generation = ++this.generation;
    if (!identity) return;
    const before = append ? this.items().at(-1)?.id : undefined;
    this.loading.set(true);
    try {
      const page = Api.ok(
        await this.api.client.GET('/v1/notifications', {
          params: { query: { limit: 50, view: this.view(), ...(before && { before }) } },
        }),
      );
      if (identity !== this.auth.identity() || generation !== this.generation) return;
      this.items.update((rows) => (append ? [...rows, ...page.items] : page.items));
      this.more.set(!!page.next_cursor);
      this.loaded.set(true);
      void this.loadJobTitles(page.items);
    } catch (e) {
      if (identity === this.auth.identity() && generation === this.generation)
        this.fail(e, 'Could not load notifications.');
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

  private fail(e: unknown, fallback: string): void {
    this.error.set(e instanceof Error ? e.message : fallback);
  }

  show(v: View): void {
    if (v === this.view()) return;
    this.view.set(v);
    this.items.set([]);
    this.loaded.set(false);
    void this.load();
  }

  older(): void {
    void this.load(true);
  }

  protected replaceMail(n: Notification, m: Message): void {
    this.items.update((list) => list.map((x) => (x.id === n.id ? { ...x, mail: m } : x)));
  }

  private patch(id: string, p: Partial<Notification>): void {
    this.items.update((list) => list.map((x) => (x.id === id ? { ...x, ...p } : x)));
  }

  private drop(id: string): void {
    this.items.update((list) => list.filter((x) => x.id !== id));
  }

  /** Opens what the notification is about: the job when one is linked, else the mail. */
  open(n: Notification, e: Event): void {
    if ((e.target as HTMLElement).closest('button, a')) return;
    void this.read(n);
    if (n.job_alert) {
      window.open(n.job_alert.url, '_blank', 'noopener,noreferrer');
      return;
    }
    const m = n.mail;
    if (!m) return;
    if (m.job_id) void this.router.navigate(['/jobs'], { queryParams: { job: m.job_id } });
    else window.open(m.gmail_url, '_blank', 'noopener,noreferrer');
  }

  /** Starts a job from the alert's posting: the new-job sheet reads it at once. */
  saveAlert(n: Notification): void {
    if (!n.job_alert) return;
    void this.read(n);
    void this.router.navigate(['/jobs'], { queryParams: { job: 'new', url: n.job_alert.url } });
  }

  /** Read state changes in place, so a row doesn't vanish from under the pointer. */
  async read(n: Notification): Promise<void> {
    if (n.read_at) return;
    this.patch(n.id, { read_at: new Date().toISOString() });
    try {
      await this.notes.markRead([n.id]);
    } catch (e) {
      this.patch(n.id, { read_at: null });
      this.fail(e, 'Could not mark it read.');
    }
  }

  async toggleRead(n: Notification): Promise<void> {
    if (!n.read_at) return this.read(n);
    this.patch(n.id, { read_at: null });
    try {
      await this.notes.markUnread([n.id]);
    } catch (e) {
      this.patch(n.id, { read_at: n.read_at });
      this.fail(e, 'Could not mark it unread.');
    }
  }

  async archive(n: Notification): Promise<void> {
    const restore = !!n.archived_at;
    this.drop(n.id);
    try {
      if (restore) await this.notes.restore([n.id]);
      else await this.notes.archive([n.id]);
    } catch (e) {
      this.fail(e, 'That did not work. Try again.');
      void this.load();
    }
  }

  async bulk(action: 'read' | 'archive'): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      if (action === 'read') await this.notes.markAllRead();
      else await this.notes.archiveRead();
      await this.load();
    } catch (e) {
      this.fail(e, 'That did not work. Try again.');
    } finally {
      this.busy.set(false);
    }
  }
}
