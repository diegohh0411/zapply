import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { relevanceLabel } from '../match/match-presentation';

@Component({
  selector: 'app-relevance-indicator',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <span
      class="inline-flex items-center gap-2 text-xs text-muted-foreground"
      [attr.title]="description() + '. Estimated relevance to this job.'"
      [attr.aria-label]="description()"
    >
      <span class="h-1.5 w-14 overflow-hidden rounded-full bg-muted" aria-hidden="true">
        <span class="block h-full rounded-full bg-brand" [style.width.%]="percent()"></span>
      </span>
      <span class="tabular-nums">{{ percent() }}%</span>
    </span>
  `,
})
export class RelevanceIndicator {
  readonly relevance = input.required<number>();
  readonly itemId = input.required<string>();
  protected readonly percent = computed(() =>
    Math.round(Math.max(0, Math.min(1, this.relevance())) * 100),
  );
  protected readonly tooltipId = computed(() => `relevance-${this.itemId()}`);
  protected readonly description = computed(
    () => `${relevanceLabel(this.relevance())}: ${this.percent()}%`,
  );
}
