import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideArrowDown, lucideArrowUp } from '@ng-icons/lucide';

import { ZardTableImports } from '@/shared/components/table';

import { ago, hostname, salary, STATUS_DOT } from './format';
import type { JobStatus, JobSummary } from './jobs.store';

type Key = 'title' | 'company' | 'status' | 'location' | 'applied_on' | 'updated_at';

@Component({
  selector: 'app-job-table',
  imports: [DatePipe, NgIcon, ...ZardTableImports],
  providers: [provideIcons({ lucideArrowUp, lucideArrowDown })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="px-4 pb-6">
      <div class="overflow-x-auto rounded-lg border">
        <table z-table>
          <thead z-table-header>
            <tr z-table-row>
              @for (c of cols; track c.key) {
                <th z-table-head [class]="c.class" [attr.aria-sort]="sortKey() === c.key ? (asc() ? 'ascending' : 'descending') : null">
                  <button type="button" class="inline-flex items-center gap-1 hover:text-foreground" (click)="sortBy(c.key)">
                    {{ c.label }}
                    @if (sortKey() === c.key) {
                      <ng-icon [name]="asc() ? 'lucideArrowUp' : 'lucideArrowDown'" class="size-3" />
                    }
                  </button>
                </th>
              }
              <th z-table-head class="hidden lg:table-cell">Salary</th>
            </tr>
          </thead>
          <tbody z-table-body>
            @for (j of rows(); track j.id) {
              <tr z-table-row class="cursor-pointer" [class.opacity-60]="j.archived" (click)="open.emit(j.id)" (keydown.enter)="open.emit(j.id)" tabindex="0">
                <td z-table-cell class="max-w-72 font-medium"><span class="line-clamp-1">{{ j.title }}</span></td>
                <td z-table-cell class="max-w-48 truncate">{{ j.company || host(j.url) }}</td>
                <td z-table-cell>
                  @if (statusOf(j); as s) {
                    <span class="inline-flex items-center gap-1.5 whitespace-nowrap">
                      <span class="size-2 rounded-full" [class]="dot[s.color]"></span>{{ s.name }}
                    </span>
                  }
                </td>
                <td z-table-cell class="hidden max-w-40 truncate md:table-cell">{{ j.location }}</td>
                <td z-table-cell class="hidden whitespace-nowrap md:table-cell">{{ j.applied_on ? (j.applied_on | date: 'mediumDate') : '' }}</td>
                <td z-table-cell class="whitespace-nowrap text-muted-foreground">{{ since(j.updated_at) }}</td>
                <td z-table-cell class="hidden whitespace-nowrap lg:table-cell">{{ pay(j) }}</td>
              </tr>
            } @empty {
              <tr z-table-row>
                <td z-table-cell colspan="7" class="py-10 text-center text-muted-foreground">No jobs match.</td>
              </tr>
            }
          </tbody>
        </table>
      </div>
    </div>
  `,
})
export class JobTable {
  readonly statuses = input.required<JobStatus[]>();
  readonly jobs = input.required<JobSummary[]>();
  readonly open = output<string>();

  protected readonly dot = STATUS_DOT;
  protected readonly host = hostname;
  protected readonly pay = salary;
  protected readonly sortKey = signal<Key>('updated_at');
  protected readonly asc = signal(false);

  protected readonly cols: { key: Key; label: string; class?: string }[] = [
    { key: 'title', label: 'Title' },
    { key: 'company', label: 'Company' },
    { key: 'status', label: 'Status' },
    { key: 'location', label: 'Location', class: 'hidden md:table-cell' },
    { key: 'applied_on', label: 'Applied', class: 'hidden md:table-cell' },
    { key: 'updated_at', label: 'Updated' },
  ];

  private readonly statusIndex = computed(() => new Map(this.statuses().map((s, i) => [s.id, { s, i }])));

  protected readonly rows = computed(() => {
    const key = this.sortKey();
    const dir = this.asc() ? 1 : -1;
    const idx = this.statusIndex();
    const val = (j: JobSummary): string | number => {
      switch (key) {
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
