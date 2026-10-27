import { DatePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  effect,
  inject,
  signal,
} from '@angular/core';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { blockMessage, usagePercent, usageUsd } from './format';

@Component({
  selector: 'app-usage-page',
  imports: [DatePipe, ZardButtonComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-5xl space-y-6 px-4 py-6">
      <header class="flex items-start justify-between gap-4">
        <div>
          <h1 class="text-xl font-semibold tracking-tight">Model usage</h1>
          <p class="text-sm text-muted-foreground">
            Your allowances cover the past 7 and 30 days. Both apply to every model request.
          </p>
        </div>
        <button z-button zType="outline" [zDisabled]="loading()" (click)="load()">Refresh</button>
      </header>
      @if (error()) {
        <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">
          {{ error() }}
        </p>
      }
      @if (usage(); as u) {
        <div class="text-sm">
          <span class="font-medium">{{ u.membership?.plan_name ?? 'No membership assigned' }}</span>
          @if (u.membership?.status === 'suspended') {
            <span class="ml-2 text-destructive">Suspended</span>
          }
        </div>
        @if (u.blocked_reason) {
          <p role="status" class="rounded-lg border bg-muted p-4 text-sm">
            {{ message(u.blocked_reason) }}
          </p>
        }
        <div class="grid gap-4 md:grid-cols-2">
          @for (window of windows; track window.days) {
            <section class="space-y-3 rounded-xl border bg-card p-5">
              <h2 class="font-medium">Past {{ window.days }} days</h2>
              <p class="text-2xl font-semibold tabular-nums">
                {{ money(window.days === 7 ? u.available_7d_usd : u.available_30d_usd) }}
                <span class="text-sm font-normal text-muted-foreground">available</span>
              </p>
              <progress
                class="h-2 w-full accent-primary"
                max="100"
                [value]="
                  percent(
                    window.days === 7 ? u.spent_7d_usd : u.spent_30d_usd,
                    u.reserved_usd,
                    window.days === 7 ? u.cap_7d_usd : u.cap_30d_usd
                  )
                "
                [attr.aria-label]="'Past ' + window.days + ' days allowance used'"
              ></progress>
              <dl class="grid grid-cols-2 gap-2 text-sm">
                <dt class="text-muted-foreground">Spent</dt>
                <dd class="text-right tabular-nums">
                  {{ money(window.days === 7 ? u.spent_7d_usd : u.spent_30d_usd) }}
                </dd>
                <dt class="text-muted-foreground">Reserved</dt>
                <dd class="text-right tabular-nums">{{ money(u.reserved_usd) }}</dd>
                <dt class="text-muted-foreground">Allowance</dt>
                <dd class="text-right tabular-nums">
                  {{ money(window.days === 7 ? u.cap_7d_usd : u.cap_30d_usd) }}
                </dd>
              </dl>
            </section>
          }
        </div>
        <p class="text-sm text-muted-foreground">
          Reservations cover model calls still running or being reconciled. Unused reserved amounts
          return after the charge is confirmed.
        </p>
        @if (u.next_capacity_at) {
          <p class="text-sm">
            Older usage next leaves a window at
            <strong>{{ u.next_capacity_at | date: 'medium' }}</strong
            >. This may return some capacity; it is not a full reset.
          </p>
        }
        <section class="space-y-3">
          <h2 class="font-medium">Recent charges</h2>
          <div class="overflow-x-auto rounded-lg border">
            <table class="w-full text-left text-sm">
              <thead class="bg-muted">
                <tr>
                  <th class="p-3">When</th>
                  <th class="p-3">Model</th>
                  <th class="p-3 text-right">Cost</th>
                </tr>
              </thead>
              <tbody>
                @for (entry of entries(); track entry.id) {
                  <tr class="border-t">
                    <td class="p-3">{{ entry.accounted_at | date: 'medium' }}</td>
                    <td class="p-3">{{ entry.model }}</td>
                    <td class="p-3 text-right tabular-nums">{{ money(entry.amount_usd) }}</td>
                  </tr>
                } @empty {
                  <tr>
                    <td colspan="3" class="p-6 text-center text-muted-foreground">
                      No model charges yet.
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          </div>
          @if (hasMore()) {
            <button z-button zType="outline" [zDisabled]="loading()" (click)="more()">
              Older charges
            </button>
          }
        </section>
      } @else if (loading()) {
        <p role="status" class="text-sm text-muted-foreground">Loading usage…</p>
      }
    </div>
  `,
})
export class UsagePage {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  protected readonly usage = signal<Schemas['Usage'] | null>(null);
  protected readonly entries = signal<Schemas['UsageEntry'][]>([]);
  protected readonly loading = signal(false);
  protected readonly error = signal('');
  protected readonly hasMore = signal(false);
  protected readonly windows = [{ days: 7 }, { days: 30 }];
  protected readonly money = usageUsd;
  protected readonly percent = usagePercent;
  protected readonly message = blockMessage;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private generation = 0;

  constructor() {
    effect(() => {
      this.auth.identity();
      this.usage.set(null);
      this.entries.set([]);
      void this.load();
    });
    const change = this.events.changes.subscribe((e) => {
      if (['usage.changed', 'membership.changed'].includes(e.topic)) void this.load();
    });
    const resync = this.events.resync.subscribe(() => void this.load());
    const visible = () => {
      if (!document.hidden) void this.load();
    };
    document.addEventListener('visibilitychange', visible);
    inject(DestroyRef).onDestroy(() => {
      this.generation++;
      clearTimeout(this.timer);
      change.unsubscribe();
      resync.unsubscribe();
      document.removeEventListener('visibilitychange', visible);
    });
  }

  async load(): Promise<void> {
    const identity = this.auth.identity(),
      generation = ++this.generation;
    clearTimeout(this.timer);
    if (!identity) return;
    this.loading.set(true);
    this.error.set('');
    try {
      const [usage, entries] = await Promise.all([
        this.api.client.GET('/v1/usage'),
        this.api.client.GET('/v1/usage/entries', { params: { query: { limit: 50 } } }),
      ]);
      if (identity !== this.auth.identity() || generation !== this.generation) return;
      const u = Api.ok(usage),
        list = Api.ok(entries);
      this.usage.set(u);
      this.entries.set(list);
      this.hasMore.set(list.length === 50);
      if (u.next_capacity_at && !document.hidden) {
        const delay = Math.max(
          1000,
          Math.min(Date.parse(u.next_capacity_at) - Date.now() + 1000, 2_147_483_647),
        );
        this.timer = setTimeout(() => void this.load(), delay);
      }
    } catch (e) {
      if (identity === this.auth.identity() && generation === this.generation)
        this.error.set(e instanceof Error ? e.message : 'Could not load usage.');
    } finally {
      if (identity === this.auth.identity() && generation === this.generation)
        this.loading.set(false);
    }
  }

  async more(): Promise<void> {
    const identity = this.auth.identity(),
      generation = this.generation,
      before = this.entries().at(-1)?.id;
    if (!before) return;
    this.loading.set(true);
    try {
      const list = Api.ok(
        await this.api.client.GET('/v1/usage/entries', {
          params: { query: { before, limit: 50 } },
        }),
      );
      if (identity === this.auth.identity() && generation === this.generation) {
        this.entries.update((rows) => [...rows, ...list]);
        this.hasMore.set(list.length === 50);
      }
    } catch (e) {
      if (identity === this.auth.identity() && generation === this.generation)
        this.error.set(e instanceof Error ? e.message : 'Could not load charges.');
    } finally {
      if (identity === this.auth.identity() && generation === this.generation)
        this.loading.set(false);
    }
  }
}
