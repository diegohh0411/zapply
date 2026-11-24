import { computed, inject, Injectable } from '@angular/core';
import { injectQuery, QueryClient } from '@tanstack/angular-query';

import { Api } from './api';
import { AuthService } from './auth.service';
import { EventsService } from './events.service';

/** Topics that can change how many notifications are unread. */
const TOPICS = new Set(['notification.updated', 'mail.message.classified']);
/** Classification arrives in bursts while a backlog is read; recount at most this often. */
const REFRESH_MS = 800;

const summaryKey = (identity: string | null) => ['notifications-summary', identity] as const;

/**
 * The member's unread notification count, cached per member and kept current
 * from the live feed, and the actions that mark notifications read. Shared by
 * the sidebar badge and the notifications page.
 */
@Injectable({ providedIn: 'root' })
export class NotificationsService {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  private readonly client = inject(QueryClient);

  private readonly summary = injectQuery(() => ({
    queryKey: summaryKey(this.auth.identity()),
    queryFn: async () => Api.ok(await this.api.client.GET('/v1/notifications/summary')),
    enabled: !!this.auth.identity(),
  }));

  /** Unread count; 0 until the first summary loads. The badge is a hint, so errors show 0. */
  readonly unread = computed(() => this.summary.data()?.unread ?? 0);
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    this.events.changes.subscribe((e) => {
      if (TOPICS.has(e.topic)) this.refreshSoon();
    });
  }

  private refreshSoon(): void {
    if (this.timer) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.refresh();
    }, REFRESH_MS);
  }

  async refresh(): Promise<void> {
    await this.client.invalidateQueries({ queryKey: summaryKey(this.auth.identity()) });
  }

  async markRead(ids: string[]): Promise<void> {
    if (!ids.length) return;
    this.set(await this.api.client.POST('/v1/notifications/read', { body: { ids } }));
  }

  async markUnread(ids: string[]): Promise<void> {
    if (!ids.length) return;
    this.set(await this.api.client.POST('/v1/notifications/read', { body: { ids, unread: true } }));
  }

  async markAllRead(): Promise<void> {
    this.set(await this.api.client.POST('/v1/notifications/read', { body: { all: true } }));
  }

  async archive(ids: string[]): Promise<void> {
    if (!ids.length) return;
    this.set(await this.api.client.POST('/v1/notifications/archive', { body: { ids } }));
  }

  async restore(ids: string[]): Promise<void> {
    if (!ids.length) return;
    this.set(
      await this.api.client.POST('/v1/notifications/archive', { body: { ids, restore: true } }),
    );
  }

  async archiveRead(): Promise<void> {
    this.set(await this.api.client.POST('/v1/notifications/archive', { body: { read: true } }));
  }

  private set(res: { data?: { unread: number }; error?: unknown; response: Response }): void {
    const identity = this.auth.identity();
    const { unread } = Api.ok(res);
    this.client.setQueryData(summaryKey(identity), (old: { unread: number } | undefined) => ({
      ...old,
      unread,
    }));
  }
}
