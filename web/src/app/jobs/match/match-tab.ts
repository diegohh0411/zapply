import {
  Component,
  ChangeDetectionStrategy,
  computed,
  effect,
  inject,
  input,
  signal,
  DestroyRef,
} from '@angular/core';
import { DecimalPipe, DatePipe } from '@angular/common';
import { ZardButtonComponent } from '@/shared/components/button';
import { MatchStore } from './match.store';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideChevronDown, lucideSearch, lucideRefreshCw } from '@ng-icons/lucide';
@Component({
  selector: 'app-match-tab',
  imports: [DecimalPipe, DatePipe, ZardButtonComponent, NgIcon],
  viewProviders: [
    provideIcons({
      lucideChevronDown,
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
  protected readonly run = computed(() => this.store.runs().get(this.jobId()));
  protected readonly estimate = computed(() => this.store.estimates().get(this.jobId()));
  protected readonly busy = signal(false);
  protected readonly confirmRematch = signal(false);
  protected readonly error = signal('');
  protected readonly active = computed(() =>
    ['pending', 'running', 'blocked', 'needs_review'].includes(this.run()?.status ?? ''),
  );
  protected readonly result = computed(() =>
    ['completed', 'partial'].includes(this.run()?.status ?? '') ? this.run()?.result : undefined,
  );
  protected readonly wholePosting = computed(() => (this.result()?.scoring_version ?? 0) >= 3);
  protected readonly relevantCount = computed(
    () => this.result()?.evidence.filter((e) => e.relevance >= 0.375).length ?? 0,
  );
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
    effect((onCleanup) => {
      const id = this.jobId();
      this.confirmRematch.set(false);
      this.busy.set(false);
      this.error.set('');
      this.store.watch(id);
      onCleanup(() => this.store.unwatch(id));
    });
    inject(DestroyRef).onDestroy(() => this.store.unwatch(this.jobId()));
  }
  protected async action(kind: 'start' | 'cancel'): Promise<void> {
    const id = this.jobId();
    this.busy.set(true);
    this.error.set('');
    this.confirmRematch.set(false);
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
