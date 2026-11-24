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
import {
  injectInfiniteQuery,
  injectQuery,
  QueryClient,
  type InfiniteData,
} from '@tanstack/angular-query';

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

type Page = Schemas['NotificationPage'];

/** Notifications arrive in bursts while a backlog is classified; refetch at most this often. */
const REFRESH_MS = 800;

const listKey = (identity: string | null, view: View) => ['notifications', identity, view] as const;
const allKey = (identity: string | null) => ['notifications', identity] as const;
const mailboxKey = (identity: string | null) => ['mail-accounts', identity] as const;
const titlesKey = (identity: string | null) => ['notification-job-titles', identity] as const;

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
  private readonly queries = inject(QueryClient);
  protected readonly notes = inject(NotificationsService);

  protected readonly label = categoryLabel;
  protected readonly sender = parseSender;
  protected readonly verb = (m: Message): string => mailVerb[m.category] ?? 'sent an update';
  protected readonly ago = (iso: string): string => relativeTime(iso, this.now());
  protected readonly now = signal(Date.now());
  protected readonly view = signal<View>('inbox');
  /** Cached per view: coming back shows the last list at once and refreshes behind it. */
  protected readonly list = injectInfiniteQuery(() => ({
    queryKey: listKey(this.auth.identity(), this.view()),
    enabled: !!this.auth.identity(),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last: Page) => (last.next_cursor ? last.items.at(-1)?.id : undefined),
    queryFn: async ({ pageParam, queryKey: [, , view] }): Promise<Page> => {
      const page = Api.ok(
        await this.api.client.GET('/v1/notifications', {
          params: { query: { limit: 50, view, ...(pageParam && { before: pageParam }) } },
        }),
      );
      void this.loadJobTitles(page.items);
      return page;
    },
  }));
  protected readonly items = computed(() => this.list.data()?.pages.flatMap((p) => p.items) ?? []);
  protected readonly more = computed(() => !!this.list.hasNextPage());
  protected readonly loading = computed(() => this.list.isFetching());
  protected readonly loaded = computed(() => this.list.isSuccess());
  private readonly mailbox = injectQuery(() => ({
    queryKey: mailboxKey(this.auth.identity()),
    enabled: !!this.auth.identity(),
    queryFn: async () => Api.ok(await this.api.client.GET('/v1/mail/accounts')),
  }));
  /** Only decides whether the empty inbox suggests connecting Gmail. */
  protected readonly hasMailbox = computed(() => {
    const d = this.mailbox.data();
    return !d || d.items.some((a) => a.status !== 'disconnected');
  });
  private readonly titles = injectQuery(() => ({
    queryKey: titlesKey(this.auth.identity()),
    enabled: false,
    staleTime: Infinity,
    queryFn: (): Record<string, string> => ({}),
  }));
  protected readonly jobTitles = computed(() => this.titles.data() ?? {});
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
  private refreshTimer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    const change = this.events.changes.subscribe((e) => {
      if (e.topic === 'mail.message.classified' || e.topic === 'notification.updated')
        this.refreshSoon();
    });
    const tick = setInterval(() => this.now.set(Date.now()), 60_000);
    effect(() => {
      const e = this.list.error();
      if (e) this.fail(e, 'Could not load notifications.');
    });
    inject(DestroyRef).onDestroy(() => {
      clearInterval(tick);
      if (this.refreshTimer) clearTimeout(this.refreshTimer);
      change.unsubscribe();
    });
  }

  private refreshSoon(): void {
    if (this.refreshTimer) return;
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = null;
      void this.reload();
    }, REFRESH_MS);
  }

  /** Refetches every cached view; the one on screen updates in place. */
  private reload(): Promise<void> {
    return this.queries.invalidateQueries({ queryKey: allKey(this.auth.identity()) });
  }

  private async loadJobTitles(items: Notification[]): Promise<void> {
    const key = titlesKey(this.auth.identity());
    const known = this.queries.getQueryData<Record<string, string>>(key) ?? {};
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
    this.queries.setQueryData<Record<string, string>>(key, (t) => ({
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
  }

  older(): void {
    if (!this.list.isFetchingNextPage()) void this.list.fetchNextPage();
  }

  /** Edits the cached rows of the view on screen, so the change survives leaving the page. */
  private edit(fn: (rows: Notification[]) => Notification[]): void {
    this.queries.setQueryData<InfiniteData<Page, string | undefined>>(
      listKey(this.auth.identity(), this.view()),
      (d) => d && { ...d, pages: d.pages.map((p) => ({ ...p, items: fn(p.items) })) },
    );
  }

  protected replaceMail(n: Notification, m: Message): void {
    this.edit((list) => list.map((x) => (x.id === n.id ? { ...x, mail: m } : x)));
  }

  private patch(id: string, p: Partial<Notification>): void {
    this.edit((list) => list.map((x) => (x.id === id ? { ...x, ...p } : x)));
  }

  private drop(id: string): void {
    this.edit((list) => list.filter((x) => x.id !== id));
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
      void this.reload();
    }
  }

  async bulk(action: 'read' | 'archive'): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      if (action === 'read') await this.notes.markAllRead();
      else await this.notes.archiveRead();
      await this.reload();
    } catch (e) {
      this.fail(e, 'That did not work. Try again.');
    } finally {
      this.busy.set(false);
    }
  }
}
