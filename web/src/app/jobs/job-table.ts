import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideArrowDown, lucideArrowUp, lucideType, lucideBuilding2, lucideCircleDot, lucideMapPin, lucideCalendarDays, lucideClock, lucideSparkles, lucideBanknote, lucideFileText, lucidePlus } from '@ng-icons/lucide';

import { ZardTableImports } from '@/shared/components/table';

import { ago, hostname, salary } from './format';
import type { JobStatus, JobSummary } from './jobs.store';
import { StatusPill } from './status-pill';

type Key = 'score' | 'title' | 'company' | 'status' | 'location' | 'applied_on' | 'updated_at';

@Component({
  selector: 'app-job-table',
  imports: [DatePipe, NgIcon, StatusPill, ...ZardTableImports],
  providers: [provideIcons({ lucideArrowUp, lucideArrowDown, lucideType, lucideBuilding2, lucideCircleDot, lucideMapPin, lucideCalendarDays, lucideClock, lucideSparkles, lucideBanknote, lucideFileText, lucidePlus })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="px-4 pb-6">
      <div class="overflow-x-auto">
        <table z-table class="min-w-[800px] border-collapse">
          <thead z-table-header>
            <tr z-table-row>
              @for (c of cols; track c.key) {
                <th z-table-head [class]="c.class" [attr.aria-sort]="sortKey() === c.key ? (asc() ? 'ascending' : 'descending') : null">
                  <button type="button" class="inline-flex items-center gap-1 hover:text-foreground" (click)="sortBy(c.key)">
                    <ng-icon [name]="c.icon" class="size-4 text-muted-foreground" />
                    {{ c.label }}
                    @if (sortKey() === c.key) {
                      <ng-icon [name]="asc() ? 'lucideArrowUp' : 'lucideArrowDown'" class="size-3" />
                    }
                  </button>
                </th>
              }
              <th z-table-head class="hidden lg:table-cell"><span class="inline-flex items-center gap-1"><ng-icon name="lucideBanknote" class="size-4" /> Salary</span></th>
            </tr>
          </thead>
          <tbody z-table-body>
            @for (j of rows(); track j.id) {
              <tr z-table-row class="group cursor-pointer" [class.opacity-60]="j.archived" (click)="open.emit(j.id)" (keydown.enter)="open.emit(j.id)" tabindex="0">
                <td z-table-cell class="max-w-72 font-medium"><span class="flex items-center gap-2"><ng-icon name="lucideFileText" class="size-4 shrink-0 text-muted-foreground" /><span class="truncate">{{ j.title }}</span><span class="ml-auto hidden rounded bg-muted px-1.5 py-0.5 text-xs font-normal text-muted-foreground group-hover:inline group-focus-within:inline">OPEN</span></span></td>
                <td z-table-cell class="max-w-48 truncate">{{ j.company || host(j.url) }}</td>
                <td z-table-cell>
                  @if (statusOf(j); as s) {
                    <app-status-pill [status]="s" />
                  }
                </td>
                <td z-table-cell class="hidden max-w-40 truncate md:table-cell">{{ j.location }}</td>
                <td z-table-cell class="hidden whitespace-nowrap md:table-cell">{{ j.applied_on ? (j.applied_on | date: 'mediumDate') : '' }}</td>
                <td z-table-cell class="whitespace-nowrap text-muted-foreground">{{ since(j.updated_at) }}</td>
                <td z-table-cell [class.opacity-50]="j.match?.stale">{{ j.match?.score?.toFixed(0) ?? j.match?.status ?? '—' }}{{ j.match?.stale ? ' · stale' : '' }}</td>
                <td z-table-cell class="hidden whitespace-nowrap lg:table-cell">{{ pay(j) }}</td>
              </tr>
            } @empty {
              <tr z-table-row>
                <td z-table-cell colspan="8" class="py-10 text-center text-muted-foreground">No jobs match.</td>
              </tr>
            }
          </tbody>
        </table>
        <button type="button" class="flex w-full items-center gap-2 border-b px-2 py-2 text-left text-sm text-muted-foreground hover:bg-muted/50 hover:text-foreground" (click)="add.emit()"><ng-icon name="lucidePlus" class="size-4" /> New page</button>
      </div>
    </div>
  `,
  styles: `
    th, td { border-right: 1px solid var(--border); }
    th:last-child, td:last-child { border-right: 0; }
    tbody tr:last-child { border-bottom: 1px solid var(--border); }
  `,
})
export class JobTable {
  readonly statuses = input.required<JobStatus[]>();
  readonly jobs = input.required<JobSummary[]>();
  readonly open = output<string>();
  readonly add = output<void>();

  protected readonly host = hostname;
  protected readonly pay = salary;
  protected readonly sortKey = signal<Key>('updated_at');
  protected readonly asc = signal(false);

  protected readonly cols: { key: Key; label: string; icon: string; class?: string }[] = [
    { key: 'title', icon: 'lucideType', label: 'Title' },
    { key: 'company', icon: 'lucideBuilding2', label: 'Company' },
    { key: 'status', icon: 'lucideCircleDot', label: 'Status' },
    { key: 'location', icon: 'lucideMapPin', label: 'Location', class: 'hidden md:table-cell' },
    { key: 'applied_on', icon: 'lucideCalendarDays', label: 'Applied', class: 'hidden md:table-cell' },
    { key: 'updated_at', icon: 'lucideClock', label: 'Updated' },
    { key: 'score', icon: 'lucideSparkles', label: 'Match' },
  ];

  private readonly statusIndex = computed(() => new Map(this.statuses().map((s, i) => [s.id, { s, i }])));

  protected readonly rows = computed(() => {
    const key = this.sortKey();
    const dir = this.asc() ? 1 : -1;
    const idx = this.statusIndex();
    const val = (j: JobSummary): string | number => {
      switch (key) {
        case 'score': return j.match?.score ?? -1;
        case 'status':
          return idx.get(j.status_id)?.i ?? 99;
        case 'company':
          return (j.company || hostname(j.url)).toLowerCase();
        case 'title':
        case 'location':
          return j[key].toLowerCase();
        default:
          return j[key] ?? '';
      }
    };
    return [...this.jobs()].sort((a, b) => {
      const x = val(a);
      const y = val(b);
      return (x < y ? -1 : x > y ? 1 : 0) * dir;
    });
  });

  protected statusOf(j: JobSummary): JobStatus | undefined {
    return this.statusIndex().get(j.status_id)?.s;
  }

  protected sortBy(key: Key): void {
    if (this.sortKey() === key) this.asc.update((v) => !v);
    else {
      this.sortKey.set(key);
      this.asc.set(key !== 'updated_at' && key !== 'applied_on');
    }
  }

  protected since(iso: string): string {
    const a = ago(iso);
    return a === 'today' ? 'today' : a + ' ago';
  }
}
