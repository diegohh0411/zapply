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
    <div class="mx-auto max-w-3xl space-y-6 px-4 py-6">
      <header class="flex items-start justify-between gap-4">
        <div>
          <h1 class="text-xl font-semibold tracking-tight">Usage</h1>
          <p class="text-sm text-muted-foreground">
            How much you can use right now, and what you have used.
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
        @if (u.membership; as m) {
          <p class="text-sm text-muted-foreground">
            Plan: <span class="font-medium text-foreground">{{ m.plan_name }}</span>
            @if (m.status === 'suspended') {
              <span class="ml-2 font-medium text-destructive">Suspended</span>
            }
          </p>
        } @else {
          <p class="text-sm text-muted-foreground">No plan assigned yet.</p>
        }
        @if (u.blocked_reason) {
          <p role="status" class="rounded-lg border bg-muted p-4 text-sm">
            {{ message(u.blocked_reason) }}
          </p>
        }

        <section class="space-y-4 rounded-xl border bg-card p-6">
          <div>
            <p class="text-sm text-muted-foreground">Available right now</p>
            <p class="text-3xl font-semibold tabular-nums">{{ money(availableNow(u)) }}</p>
          </div>
          <p class="text-sm text-muted-foreground">
            This is the smaller of the two limits below. Both apply to every AI request you make.
          </p>
          @if (u.next_capacity_at) {
            <p class="text-sm">
              Some of your usage frees up on
              <strong>{{ u.next_capacity_at | date: 'medium' }}</strong>, so your limit grows back a
              little. It does not reset all at once.
            </p>
          }
        </section>

        <div class="grid gap-4 md:grid-cols-2">
          @for (days of windowDays; track days) {
            @let limit = limitFor(u, days);
            <section class="space-y-3 rounded-xl border bg-card p-5">
              <div class="flex items-baseline justify-between gap-2">
                <h2 class="font-medium">Last {{ days }} days</h2>
                <span class="text-sm tabular-nums">{{ limit.left }} left</span>
              </div>
              <progress
                class="h-2 w-full accent-primary"
                max="100"
                [value]="limit.percent"
                [attr.aria-label]="'Last ' + days + ' days: ' + limit.used + ' used'"
              ></progress>
              <p class="text-sm text-muted-foreground">
                {{ limit.used }} used of your {{ limit.allowance }} limit
              </p>
            </section>
          }
        </div>

        <section class="space-y-3">
          <h2 class="font-medium">Recent activity</h2>
          <div class="overflow-x-auto rounded-lg border">
            <table class="w-full text-left text-sm">
              <thead class="bg-muted">
                <tr>
                  <th class="p-3">When</th>
                  <th class="p-3">Activity</th>
                  <th class="p-3 text-right">Cost</th>
                </tr>
              </thead>
              <tbody>
                @for (entry of entries(); track entry.id) {
                  <tr class="border-t">
                    <td class="p-3 whitespace-nowrap">{{ entry.accounted_at | date: 'medium' }}</td>
                    <td class="p-3">
                      <p class="font-medium">
                        {{ label(entry) }}
                        @if (entry.company) {
                          <span class="font-normal text-muted-foreground">at {{ entry.company }}</span>
                        }
                      </p>
                      <p class="text-xs text-muted-foreground">{{ detail(entry) }}</p>
                    </td>
                    <td class="p-3 text-right tabular-nums">{{ money(entry.amount_usd) }}</td>
                  </tr>
                } @empty {
                  <tr>
                    <td colspan="3" class="p-6 text-center text-muted-foreground">
                      No usage yet. Activity appears here after your first AI request.
                    </td>
                  </tr>
                }
              </tbody>
            </table>
          </div>
          @if (hasMore()) {
            <button z-button zType="outline" [zDisabled]="loading()" (click)="more()">
              Show older activity
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
  protected readonly windowDays = [7, 30];
  private readonly stepLabels: Record<string, string> = {
    match: 'matched your profile',
    summary: 'wrote a CV summary',
    extraction: 'read a job posting',
    mail: 'sorted emails',
    other: 'other AI work',
  };
  protected readonly money = usageUsd;
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

  /** A job's name when its work is linked to one; otherwise a plain description of the work. */
  protected label(entry: Schemas['UsageEntry']): string {
    if (entry.title) return entry.title;
    return entry.app === 'mail' ? 'Emails not linked to a job' : 'Other job work';
  }

  /** The application the work came from, and what it did, in plain words. */
  protected detail(entry: Schemas['UsageEntry']): string {
    const app = entry.app === 'mail' ? 'Mail' : 'Jobs';
    const work = entry.steps.map((step) => this.stepLabels[step] ?? 'other AI work').join(', ');
    return `${app} · ${work}`;
  }

  /** Both windows apply to every request, so the smaller remainder is what the user can actually spend. */
  protected availableNow(u: Schemas['Usage']): string {
    return Number(u.available_7d_usd) <= Number(u.available_30d_usd)
      ? u.available_7d_usd
      : u.available_30d_usd;
  }

  /** Plain-language figures for one window: what is left, what is used, and the bar fill. */
  protected limitFor(u: Schemas['Usage'], days: number) {
    const seven = days === 7;
    const spent = seven ? u.spent_7d_usd : u.spent_30d_usd;
    const cap = seven ? u.cap_7d_usd : u.cap_30d_usd;
    return {
      days,
      left: usageUsd(seven ? u.available_7d_usd : u.available_30d_usd),
      used: usageUsd(String(Number(spent) + Number(u.reserved_usd))),
      allowance: usageUsd(cap),
      percent: usagePercent(spent, u.reserved_usd, cap),
    };
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
