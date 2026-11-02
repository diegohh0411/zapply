import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { relevanceLabel } from '../match/match-presentation';

@Component({
  selector: 'app-relevance-indicator',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <span class="group relative inline-flex shrink-0">
      <button type="button" class="rounded-full focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand"
        [attr.aria-label]="description()" [attr.aria-describedby]="tooltipId()">
        <span class="block size-4 rounded-full border border-brand/40" [style.background]="fill()" aria-hidden="true"></span>
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
  protected readonly fill = computed(() => `conic-gradient(var(--color-brand) ${this.percent()}%, transparent 0)`);
}
