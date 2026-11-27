import { DatePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  computed,
  DestroyRef,
  effect,
  inject,
} from '@angular/core';
import { injectInfiniteQuery, injectQuery, QueryClient } from '@tanstack/angular-query';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { blockMessage, usagePercent, usageUsd } from './format';

/** Keyed by member, so one member's cached usage never shows for another. */
const usageKey = (identity: string | null) => ['usage', identity] as const;
const entriesKey = (identity: string | null) => ['usage-entries', identity] as const;
const PAGE = 50;

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
              <strong>{{ u.next_capacity_at | date: 'medium' }}</strong
              >, so your limit grows back a little. It does not reset all at once.
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
                          <span class="font-normal text-muted-foreground"
                            >at {{ entry.company }}</span
                          >
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
  private readonly queries = inject(QueryClient);
  /** Cached: coming back shows the last figures at once and refreshes behind them. */
  private readonly usageQuery = injectQuery(() => ({
    queryKey: usageKey(this.auth.identity()),
    enabled: !!this.auth.identity(),
    queryFn: async () => Api.ok(await this.api.client.GET('/v1/usage')),
  }));
  private readonly entriesQuery = injectInfiniteQuery(() => ({
    queryKey: entriesKey(this.auth.identity()),
    enabled: !!this.auth.identity(),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last: Schemas['UsageEntry'][]) =>
      last.length === PAGE ? last.at(-1)?.id : undefined,
    queryFn: async ({ pageParam }) =>
      Api.ok(
        await this.api.client.GET('/v1/usage/entries', {
          params: { query: { limit: PAGE, ...(pageParam && { before: pageParam }) } },
        }),
      ),
  }));
  protected readonly usage = computed(() => this.usageQuery.data() ?? null);
  protected readonly entries = computed(() => this.entriesQuery.data()?.pages.flat() ?? []);
  protected readonly loading = computed(
    () => this.usageQuery.isFetching() || this.entriesQuery.isFetching(),
  );
  protected readonly hasMore = computed(() => !!this.entriesQuery.hasNextPage());
  protected readonly error = computed(() => {
    const e = this.usageQuery.error() ?? this.entriesQuery.error();
    if (!e) return '';
    if (e instanceof Error) return e.message;
    return this.entriesQuery.isFetchNextPageError()
      ? 'Could not load charges.'
      : 'Could not load usage.';
  });
  protected readonly windowDays = [7, 30];
  private readonly stepLabels: Record<string, string> = {
    match: 'matched your profile',
    summary: 'wrote a CV summary',
    extraction: 'read a job posting',
    mail: 'sorted emails',
    scrape: 'read posting pages',
    search: 'searched for new roles',
    other: 'other AI work',
  };
  protected readonly money = usageUsd;
  protected readonly message = blockMessage;

  constructor() {
    // Usage frees up gradually; refetch when the next slice becomes available.
    effect((onCleanup) => {
      const at = this.usage()?.next_capacity_at;
      if (!at || document.hidden) return;
      const delay = Math.max(1000, Math.min(Date.parse(at) - Date.now() + 1000, 2_147_483_647));
      const timer = setTimeout(() => void this.load(), delay);
      onCleanup(() => clearTimeout(timer));
    });
    const change = this.events.changes.subscribe((e) => {
      if (['usage.changed', 'membership.changed'].includes(e.topic)) void this.load();
    });
    const visible = () => {
      if (!document.hidden) void this.load();
    };
    document.addEventListener('visibilitychange', visible);
    inject(DestroyRef).onDestroy(() => {
      change.unsubscribe();
      document.removeEventListener('visibilitychange', visible);
    });
  }

  /** A job's name when its work is linked to one; otherwise a plain description of the work. */
  protected label(entry: Schemas['UsageEntry']): string {
    if (entry.title) return entry.title;
    if (entry.app === 'alerts') return 'A search watcher';
    return entry.app === 'mail' ? 'Emails not linked to a job' : 'Other job work';
  }

  /** The application the work came from, and what it did, in plain words. */
  protected detail(entry: Schemas['UsageEntry']): string {
    const app = entry.app === 'mail' ? 'Mail' : entry.app === 'alerts' ? 'Job alerts' : 'Jobs';
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

  /** Refetches the figures and the activity list; what is on screen updates in place. */
  async load(): Promise<void> {
    const identity = this.auth.identity();
    if (!identity) return;
    await Promise.all([
      this.queries.invalidateQueries({ queryKey: usageKey(identity) }),
      this.queries.invalidateQueries({ queryKey: entriesKey(identity) }),
    ]);
  }

  more(): void {
    if (!this.entriesQuery.isFetchingNextPage()) void this.entriesQuery.fetchNextPage();
  }
}
