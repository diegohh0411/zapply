import {
  ChangeDetectionStrategy,
  Component,
  computed,
  input,
  output,
  ViewEncapsulation,
} from '@angular/core';

import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideRotateCw, lucideTriangleAlert } from '@ng-icons/lucide';
import type { ClassValue } from 'clsx';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardSkeletonComponent } from '@/shared/components/skeleton';
import { mergeClasses } from '@/shared/utils/merge-classes';

export type ZardPageWidth = 'narrow' | 'default' | 'full';

const widths: Record<ZardPageWidth, string> = {
  narrow: 'mx-auto max-w-[56rem] px-6',
  default: 'mx-auto max-w-[72rem] px-6',
  full: 'px-4',
};

/** The state of what a page shows, usually straight from an injectQuery result. */
export interface ZardPageState {
  /** Nothing to show yet: the first load, with nothing cached. */
  pending: boolean;
  /** Refreshing data that is already on screen. */
  fetching?: boolean;
  /** Why loading failed. With data on screen it shows above it; without, instead of it. */
  error?: unknown;
  /** Loaded, and there is nothing to list. */
  empty?: boolean;
}

/** Adapts an injectQuery result to a page state. */
export function pageState(q: {
  isPending(): boolean;
  isFetching(): boolean;
  error(): unknown;
  data(): unknown;
}, empty?: (data: never) => boolean): ZardPageState {
  const data = q.data();
  return {
    pending: q.isPending() && !q.error(),
    fetching: q.isFetching(),
    error: q.error(),
    empty: data !== undefined && !!empty?.(data as never),
  };
}

/**
 * The layout of a signed-in route: header (title, description, actions), an
 * optional toolbar, and a body that handles loading the same way everywhere.
 *
 * - First load, nothing cached: the [skeleton] slot, or a default skeleton.
 * - Refresh with data on screen: a thin bar at the top; nothing moves.
 * - Error: inline with a retry; stale data stays visible under it.
 * - Empty: the [empty] slot, only once a load has finished.
 *
 * Slots: [actions], [toolbar], [skeleton], [empty], and the default content.
 */
@Component({
  selector: 'z-page',
  imports: [NgIcon, ZardButtonComponent, ZardSkeletonComponent],
  providers: [provideIcons({ lucideRotateCw, lucideTriangleAlert })],
  template: `
    <div
      aria-hidden="true"
      class="pointer-events-none sticky top-0 z-10 h-0.5 overflow-hidden"
    >
      @if (busy()) {
        <div class="h-full w-1/3 animate-[z-page-progress_1.1s_ease-in-out_infinite] bg-primary/60"></div>
      }
    </div>
    <div [class]="classes()" [attr.aria-busy]="state().pending || state().fetching || null">
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div class="min-w-0 max-w-[38rem]">
          <h1 class="text-xl font-semibold tracking-tight">{{ zTitle() }}</h1>
          @if (zDescription()) {
            <p class="mt-1 text-sm text-muted-foreground">{{ zDescription() }}</p>
          }
          <ng-content select="[subtitle]" />
        </div>
        <div class="flex flex-wrap items-center gap-2 empty:hidden">
          <ng-content select="[actions]" />
        </div>
      </header>

      <div class="mt-5 empty:hidden">
        <ng-content select="[toolbar]" />
      </div>

      @if (errorText(); as message) {
        <div
          role="alert"
          class="mt-5 flex items-start justify-between gap-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive"
        >
          <p class="flex items-start gap-2">
            <ng-icon name="lucideTriangleAlert" class="mt-0.5 shrink-0" aria-hidden="true" />
            {{ message }}
          </p>
          <button z-button zType="ghost" zSize="sm" (click)="retry.emit()">
            <ng-icon name="lucideRotateCw" />Retry
          </button>
        </div>
      }

      <!-- Content stays in the DOM so its own state (open forms) survives a refetch. -->
      <div [class.hidden]="state().pending">
        <ng-content />
        @if (state().empty) {
          <ng-content select="[empty]" />
        }
      </div>

      @if (state().pending) {
        <div class="mt-6" role="status" aria-label="Loading">
          <ng-content select="[skeleton]" />
          <div class="z-page-default space-y-3">
            @for (row of rows(); track $index) {
              <z-skeleton class="h-16 w-full" />
            }
          </div>
        </div>
      }
    </div>
  `,
  styles: `
    @keyframes z-page-progress {
      from { transform: translateX(-100%); }
      to { transform: translateX(300%); }
    }
    z-page:has([skeleton]) .z-page-default { display: none; }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
  encapsulation: ViewEncapsulation.None,
  host: { class: 'block' },
  exportAs: 'zPage',
})
export class ZardPageComponent {
  readonly zTitle = input.required<string>();
  readonly zDescription = input('');
  readonly zWidth = input<ZardPageWidth>('narrow');
  readonly zState = input<ZardPageState>({ pending: false });
  /** Rows in the default skeleton. */
  readonly zSkeletonRows = input(3);
  readonly class = input<ClassValue>('');

  readonly retry = output<void>();

  protected readonly state = this.zState;
  protected readonly rows = computed(() => Array.from({ length: this.zSkeletonRows() }));
  protected readonly busy = computed(() => !!this.state().fetching && !this.state().pending);
  protected readonly errorText = computed(() => {
    const e = this.state().error;
    if (!e) return '';
    return e instanceof Error ? e.message : 'Something went wrong loading this page.';
  });
  protected readonly classes = computed(() =>
    mergeClasses(widths[this.zWidth()], 'pb-16 pt-7', this.class()),
  );
}
