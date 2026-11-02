import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideGauge } from '@ng-icons/lucide';
import { relevanceLabel } from '../match/match-presentation';

@Component({
  selector: 'app-relevance-indicator',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [NgIcon],
  providers: [provideIcons({ lucideGauge })],
  template: `
    <span class="group relative inline-flex shrink-0">
      <button type="button" class="inline-flex items-center gap-1 rounded text-xs text-muted-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand"
        [attr.aria-label]="description()" [attr.aria-describedby]="tooltipId()">
        <ng-icon name="lucideGauge" class="text-brand" aria-hidden="true" />
        <span aria-hidden="true">{{ percent() }}%</span>
      </button>
      <span [id]="tooltipId()" role="tooltip" class="pointer-events-none invisible absolute right-0 top-full z-20 mt-2 w-48 rounded-md border bg-background p-2 text-xs font-normal text-foreground shadow-lg group-hover:visible group-focus-within:visible">
        {{ description() }}. This is the model's relevance estimate for this experience and the job posting.
      </span>
    </span>
  `,
})
export class RelevanceIndicator {
  readonly relevance = input.required<number>();
  readonly itemId = input.required<string>();
  protected readonly percent = computed(() => Math.round(Math.max(0, Math.min(1, this.relevance())) * 100));
  protected readonly tooltipId = computed(() => `relevance-${this.itemId()}`);
  protected readonly description = computed(() => `${relevanceLabel(this.relevance())}: ${this.percent()}%`);
}
