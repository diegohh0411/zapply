import { ChangeDetectionStrategy, Component, computed, input, output } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideCheck } from '@ng-icons/lucide';
import { ZardDropdownImports } from '@/shared/components/dropdown';
import { STAGES, type JobStatus } from './jobs.store';
import { StatusPill } from './status-pill';

@Component({
  selector: 'app-status-select',
  imports: [StatusPill, NgIcon, ...ZardDropdownImports],
  providers: [provideIcons({ lucideCheck })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <z-dropdown-menu class="w-56">
      <button dropdown-trigger type="button" class="rounded-md p-1 -m-1 hover:bg-accent focus-visible:outline-2 focus-visible:outline-ring" aria-label="Change status">
        @if (selected(); as status) { <app-status-pill [status]="status" /> }
        @else { <span class="text-sm text-muted-foreground">Select status</span> }
      </button>
      @for (group of groups(); track group.label) {
        <div class="px-2 pb-1 pt-2 text-xs font-medium text-muted-foreground">{{ group.label }}</div>
        @for (status of group.statuses; track status.id) {
          <z-dropdown-menu-item (click)="selectionChange.emit(status.id)">
            <app-status-pill [status]="status" />
            @if (status.id === value()) { <ng-icon name="lucideCheck" class="ml-auto size-4 text-muted-foreground" /> }
          </z-dropdown-menu-item>
        }
      }
    </z-dropdown-menu>
  `,
})
export class StatusSelect {
  readonly statuses = input.required<JobStatus[]>();
  readonly value = input('');
  readonly selectionChange = output<string>();
  protected readonly selected = computed(() => this.statuses().find((s) => s.id === this.value()));
  protected readonly groups = computed(() => STAGES.map((stage) => ({
    label: stage.label,
    statuses: this.statuses().filter((s) => s.stage === stage.value),
  })).filter((group) => group.statuses.length));
}
