import { effect, inject, Injectable, signal } from '@angular/core';

import { Api } from './api';
import { AuthService } from './auth.service';
import { EventsService } from './events.service';

/** Topics that can change how many notifications are unread. */
const TOPICS = new Set(['notification.updated', 'mail.message.classified']);
/** Classification arrives in bursts while a backlog is read; recount at most this often. */
const REFRESH_MS = 800;

/**
 * The member's unread notification count, kept current from the live feed,
 * and the actions that mark notifications read. Shared by the sidebar badge
 * and the notifications page.
 */
@Injectable({ providedIn: 'root' })
export class NotificationsService {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);

  readonly unread = signal(0);
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    effect(() => {
      this.unread.set(0);
      if (this.auth.identity()) void this.refresh();
    });
    this.events.changes.subscribe((e) => {
      if (TOPICS.has(e.topic)) this.refreshSoon();
    });
    this.events.resync.subscribe(() => void this.refresh());
  }

  private refreshSoon(): void {
    if (this.timer) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.refresh();
    }, REFRESH_MS);
  }

  async refresh(): Promise<void> {
    const identity = this.auth.identity();
    if (!identity) return;
    try {
      const sum = Api.ok(await this.api.client.GET('/v1/notifications/summary'));
      if (identity === this.auth.identity()) this.unread.set(sum.unread);
    } catch {
      // The badge is a hint; the next event or resync tries again.
    }
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
    this.unread.set(Api.ok(res).unread);
  }
}
