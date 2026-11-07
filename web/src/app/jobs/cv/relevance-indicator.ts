import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';

interface RelevanceTier {
  readonly min: number;
  readonly label: string;
  readonly classes: string;
}

// Ordered from strongest to weakest; the first tier whose `min` is met wins.
const TIERS: readonly RelevanceTier[] = [
  {
    min: 0.75,
    label: 'Top match',
    classes: 'border-brand/30 bg-brand/15 text-brand',
  },
  {
    min: 0.5,
    label: 'Strong',
    classes: 'border-brand/20 bg-brand/8 text-brand',
  },
  {
    min: 0.25,
    label: 'Partial',
    classes: 'border-border bg-muted text-foreground/80',
  },
  {
    min: 0,
    label: 'Weak',
    classes: 'border-border bg-transparent text-muted-foreground',
  },
];

@Component({
  selector: 'app-relevance-indicator',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <span
      class="inline-flex items-center whitespace-nowrap rounded-full border px-2.5 py-0.5 text-xs font-medium"
      [class]="tier().classes"
      [attr.title]="tier().label + ' (' + percent() + '%). Estimated relevance to this job.'"
      [attr.aria-label]="tier().label + ', ' + percent() + '% estimated relevance'"
    >
      {{ tier().label }}
    </span>
  `,
})
export class RelevanceIndicator {
  readonly relevance = input.required<number>();
  readonly itemId = input.required<string>();

  private readonly clamped = computed(() => Math.max(0, Math.min(1, this.relevance())));
  protected readonly percent = computed(() => Math.round(this.clamped() * 100));
  protected readonly tier = computed(
    () => TIERS.find((tier) => this.clamped() >= tier.min) ?? TIERS[TIERS.length - 1],
  );
}
