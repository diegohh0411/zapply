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
import { DecimalPipe, DatePipe, NgTemplateOutlet, SlicePipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ZardButtonComponent } from '@/shared/components/button';
import { MatchStore } from './match.store';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideCheck,
  lucideChevronDown,
  lucideCircleHelp,
  lucideSearch,
  lucideRefreshCw,
  lucideMinus,
} from '@ng-icons/lucide';
import {
  coverageGroup,
  coverageLabel,
  primaryRequirements,
  requirementClass,
  type MatchFilter,
} from './match-presentation';
@Component({
  selector: 'app-match-tab',
  imports: [
    DecimalPipe,
    DatePipe,
    NgTemplateOutlet,
    SlicePipe,
    FormsModule,
    RouterLink,
    ZardButtonComponent,
    NgIcon,
  ],
  viewProviders: [
    provideIcons({
      lucideCheck,
      lucideChevronDown,
      lucideCircleHelp,
      lucideSearch,
      lucideRefreshCw,
      lucideMinus,
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
  protected readonly error = signal('');
  protected readonly active = computed(() =>
    ['pending', 'running', 'blocked', 'needs_review'].includes(this.run()?.status ?? ''),
  );
  protected readonly result = computed(() =>
    ['completed', 'partial'].includes(this.run()?.status ?? '') ? this.run()?.result : undefined,
  );
  protected readonly requirements = computed(() =>
    primaryRequirements(this.result()?.requirements ?? []),
  );
  protected readonly context = computed(() =>
    (this.result()?.requirements ?? []).filter(
      (req) => !['must', 'preferred', 'duty'].includes(req.class),
    ),
  );
  protected readonly supported = computed(
    () => this.requirements().filter((req) => coverageGroup(req) === 'supported').length,
  );
  protected readonly review = computed(
    () => this.requirements().filter((req) => coverageGroup(req) === 'review').length,
  );
  protected readonly gaps = computed(
    () => this.requirements().filter((req) => coverageGroup(req) === 'gap').length,
  );
  protected readonly filter = signal<MatchFilter>('attention');
  protected readonly limit = signal(6);
  protected readonly filtered = computed(() =>
    this.requirements().filter(
      (req) =>
        this.filter() === 'all' ||
        (this.filter() === 'supported'
          ? coverageGroup(req) === 'supported'
          : ['gap', 'review'].includes(coverageGroup(req))),
    ),
  );
  protected readonly coverageGroup = coverageGroup;
  protected readonly coverageLabel = coverageLabel;
  protected readonly requirementClass = requirementClass;
  protected readonly filters = ['attention', 'supported', 'all'] as const;
  protected setFilter(filter: MatchFilter): void {
    this.filter.set(filter);
    this.limit.set(6);
  }
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
      this.filter.set('attention');
      this.limit.set(6);
    });
    effect((onCleanup) => {
      const id = this.jobId();
      this.store.watch(id);
      onCleanup(() => this.store.unwatch(id));
    });
    inject(DestroyRef).onDestroy(() => this.store.unwatch(this.jobId()));
  }
  protected async action(kind: string, hash = '', value = ''): Promise<void> {
    const id = this.jobId();
    this.busy.set(true);
    this.error.set('');
    try {
      if (kind === 'start') await this.store.start(id);
      else if (kind === 'cancel') await this.store.cancel(id, this.run()!.id);
      else await this.store.correct(id, hash, value);
    } catch (e) {
      if (id === this.jobId()) this.error.set((e as Error).message);
    } finally {
      if (id === this.jobId()) this.busy.set(false);
    }
  }
}
