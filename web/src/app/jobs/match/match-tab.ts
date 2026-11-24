import {
  Component,
  ChangeDetectionStrategy,
  computed,
  effect,
  inject,
  input,
  signal,
} from '@angular/core';
import { injectQuery } from '@tanstack/angular-query';
import { ZardButtonComponent } from '@/shared/components/button';
import { matchEstimateKey, matchRunKey, MatchStore } from './match.store';
import { AuthService } from '../../core/auth.service';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideSearch, lucideRefreshCw } from '@ng-icons/lucide';
import { ConfirmService } from '../../core/confirm.service';
@Component({
  selector: 'app-match-tab',
  imports: [ZardButtonComponent, NgIcon],
  viewProviders: [
    provideIcons({
      lucideSearch,
      lucideRefreshCw,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './match-tab.html',
})
export class MatchTab {
  readonly jobId = input.required<string>();
  protected readonly store = inject(MatchStore);
  private readonly auth = inject(AuthService);
  private readonly runQuery = injectQuery(() => {
    const id = this.jobId();
    return {
      queryKey: matchRunKey(this.auth.identity(), id),
      queryFn: () => this.store.fetchRun(id),
      enabled: !!this.auth.identity(),
    };
  });
  private readonly estimateQuery = injectQuery(() => {
    const id = this.jobId();
    return {
      queryKey: matchEstimateKey(this.auth.identity(), id),
      queryFn: () => this.store.fetchEstimate(id),
      enabled: !!this.auth.identity(),
      // Run starts remain available when estimates are unavailable.
      retry: false,
    };
  });
  protected readonly run = computed(() => this.runQuery.data() ?? undefined);
  protected readonly estimate = computed(() => this.estimateQuery.data());
  protected readonly loadError = computed(() => this.runQuery.error()?.message ?? '');
  protected readonly busy = signal(false);
  private readonly confirm = inject(ConfirmService);
  protected readonly error = signal('');
  protected readonly active = computed(() =>
    ['pending', 'running', 'blocked', 'needs_review'].includes(this.run()?.status ?? ''),
  );
  protected readonly result = computed(() =>
    ['completed', 'partial'].includes(this.run()?.status ?? '') ? this.run()?.result : undefined,
  );
  protected readonly wholePosting = computed(() => (this.result()?.scoring_version ?? 0) >= 3);
  /** One actionable line when the finished match should be re-run. */
  protected readonly notice = computed(() => {
    const r = this.run();
    if (!r) return '';
    if (!this.result()) return this.statusLabel();
    if (!this.wholePosting() || r.stale) return 'Match out of date';
    if (r.status === 'partial') return 'Match incomplete';
    return '';
  });
  protected readonly costHint = computed(() => {
    const e = this.estimate();
    return e?.available && e.cost_usd ? `Estimated cost $${e.cost_usd}` : null;
  });
  protected readonly statusLabel = computed(
    () =>
      (
        ({
          pending: 'Match queued',
          running: 'Comparing your experience',
          blocked: 'Match paused',
          needs_review: 'Match needs attention',
          completed: 'Match complete',
          partial: 'Match incomplete',
          failed: 'Match could not finish',
          canceled: 'Match canceled',
          cancelled: 'Match canceled',
        }) as Record<string, string>
      )[this.run()?.status ?? ''] ?? 'Match unavailable',
  );
  constructor() {
    effect(() => {
      this.jobId();
      this.busy.set(false);
      this.error.set('');
    });
  }
  protected async rerun(): Promise<void> {
    const ok = await this.confirm.ask({
      title: 'Re-run match?',
      description: 'Your match corrections and manual CV selections will be reset.',
      okText: 'Re-run',
    });
    if (ok) await this.action('start');
  }
  protected async action(kind: 'start' | 'cancel'): Promise<void> {
    const id = this.jobId();
    this.busy.set(true);
    this.error.set('');
    try {
      if (kind === 'start') await this.store.start(id);
      else await this.store.cancel(id, this.run()!.id);
    } catch (e) {
      if (id === this.jobId()) this.error.set((e as Error).message);
    } finally {
      if (id === this.jobId()) this.busy.set(false);
    }
  }
}
