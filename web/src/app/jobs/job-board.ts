import { CdkDrag, type CdkDragDrop, CdkDragPlaceholder, CdkDropList, CdkDropListGroup } from '@angular/cdk/drag-drop';
import { ChangeDetectionStrategy, Component, computed, input, output } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideArchive, lucideEllipsis, lucideMapPin, lucidePlus, lucideWallet } from '@ng-icons/lucide';

import { ZardDropdownImports } from '@/shared/components/dropdown';

import { ago, hostname, salary } from './format';
import { StatusPill } from './status-pill';
import { byPosition, type JobStatus, type JobSummary } from './jobs.store';

export interface BoardMove {
  jobId: string;
  statusId: string;
  afterId?: string;
  beforeId?: string;
}

@Component({
  selector: 'app-job-board',
  imports: [CdkDropListGroup, CdkDropList, CdkDrag, CdkDragPlaceholder, NgIcon, StatusPill, ...ZardDropdownImports],
  providers: [provideIcons({ lucidePlus, lucideMapPin, lucideWallet, lucideArchive, lucideEllipsis })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="flex h-full gap-3 overflow-x-auto p-4 pt-2" cdkDropListGroup>
      @for (col of columns(); track col.status.id) {
        <section class="flex w-72 shrink-0 flex-col rounded-xl bg-muted/50" [attr.aria-label]="col.status.name">
          <header class="flex items-center gap-2 px-3 pb-1 pt-3">
            <h2 class="min-w-0"><app-status-pill [status]="col.status" /></h2>
            <span class="text-xs tabular-nums text-muted-foreground">{{ col.jobs.length }}</span>
            <span class="ml-auto flex shrink-0 items-center">
            <z-dropdown-menu>
              <button dropdown-trigger type="button" class="rounded-md p-1 text-muted-foreground hover:bg-background hover:text-foreground" [attr.aria-label]="col.status.name + ' options'">
                <ng-icon name="lucideEllipsis" class="size-4" />
              </button>
              <z-dropdown-menu-item (click)="editProperty.emit()">Edit property</z-dropdown-menu-item>
            </z-dropdown-menu>
            <button
              type="button"
              class="rounded-md p-1 text-muted-foreground hover:bg-background hover:text-foreground"
              (click)="add.emit(col.status.id)"
              [attr.aria-label]="'Add a job to ' + col.status.name"
            >
              <ng-icon name="lucidePlus" class="size-4" />
            </button>
            </span>
          </header>

          <div
            class="flex min-h-24 flex-1 flex-col gap-2 overflow-y-auto p-2"
            cdkDropList
            [id]="col.status.id"
            [cdkDropListData]="col.jobs"
            (cdkDropListDropped)="dropped($event, col.status.id)"
          >
            @for (job of col.jobs; track job.id) {
              <button
                type="button"
                cdkDrag
                [cdkDragData]="job"
                class="group flex w-full cursor-grab flex-col gap-1.5 rounded-lg border bg-card p-3 text-left shadow-xs transition-shadow hover:shadow-md active:cursor-grabbing focus-visible:outline-2 focus-visible:outline-ring"
                [class.opacity-60]="job.archived"
                (click)="open.emit(job.id)"
              >
                <div class="flex items-start gap-2">
                  <span class="min-w-0 flex-1">
                    <span class="line-clamp-2 text-sm font-medium leading-snug">{{ job.title }}</span>
                    @if (job.company || job.url) {
                      <span class="block truncate text-xs text-muted-foreground">{{ job.company || host(job.url) }}</span>
                    }
                  </span>
                  @if (job.archived) {
                    <ng-icon name="lucideArchive" class="size-3.5 shrink-0 text-muted-foreground" aria-label="Archived" />
                  }
                </div>
                @if (job.location || pay(job)) {
                  <span class="flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted-foreground">
                    @if (job.location) {
                      <span class="inline-flex items-center gap-1"><ng-icon name="lucideMapPin" class="size-3" />{{ job.location }}</span>
                    }
                    @if (pay(job)) {
                      <span class="inline-flex items-center gap-1"><ng-icon name="lucideWallet" class="size-3" />{{ pay(job) }}</span>
                    }
                  </span>
                }
                @if (job.match; as m) { <span class="self-start rounded-full border px-2 py-0.5 text-xs" [class.opacity-50]="m.stale">Match {{ m.score === null || m.score === undefined ? m.status : m.score.toFixed(0) + '/100' }}{{ m.stale ? ' · stale' : '' }}</span> }
                <span class="text-[11px] text-muted-foreground/80">In this column {{ since(job.status_changed_at) }}</span>
                <div *cdkDragPlaceholder class="h-16 rounded-lg border-2 border-dashed border-brand/40 bg-brand/5"></div>
              </button>
            } @empty {
              <p class="px-1 py-6 text-center text-xs text-muted-foreground">Drop jobs here</p>
            }
          </div>
        </section>
      }
    </div>
  `,
  styles: `
    .cdk-drag-preview {
      box-shadow: 0 10px 30px -10px rgb(0 0 0 / 0.35);
      transform: rotate(1.5deg);
    }
    .cdk-drag-animating,
    .cdk-drop-list-dragging .cdk-drag:not(.cdk-drag-placeholder) {
      transition: transform 200ms cubic-bezier(0, 0, 0.2, 1);
    }
  `,
})
export class JobBoard {
  readonly statuses = input.required<JobStatus[]>();
  readonly jobs = input.required<JobSummary[]>();
  readonly open = output<string>();
  readonly add = output<string>();
  readonly editProperty = output<void>();
  readonly move = output<BoardMove>();

  protected readonly host = hostname;
  protected readonly pay = salary;

  protected readonly columns = computed(() => {
    const by = new Map<string, JobSummary[]>();
    for (const j of this.jobs()) {
      const list = by.get(j.status_id) ?? [];
      list.push(j);
      by.set(j.status_id, list);
    }
    return this.statuses().map((status) => ({ status, jobs: (by.get(status.id) ?? []).sort(byPosition) }));
  });

  protected since(iso: string): string {
    const a = ago(iso);
    return a === 'today' ? 'since today' : 'for ' + a;
  }

  protected dropped(e: CdkDragDrop<JobSummary[]>, statusId: string): void {
    const job = e.item.data as JobSummary;
    if (e.previousContainer === e.container && e.previousIndex === e.currentIndex) return;
    const target = e.container.data.filter((j) => j.id !== job.id);
    const prev = target[e.currentIndex - 1];
    const next = target[e.currentIndex];
    this.move.emit({ jobId: job.id, statusId, afterId: prev?.id, beforeId: prev ? undefined : next?.id });
  }
}
