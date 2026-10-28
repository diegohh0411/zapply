import { ChangeDetectionStrategy, Component, input, model, output } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideSquareKanban, lucideTable } from '@ng-icons/lucide';
import { ZardSheetImports } from './components/sheet';

@Component({
  selector: 'app-database-view-settings',
  imports: [NgIcon, ...ZardSheetImports],
  providers: [provideIcons({ lucideSquareKanban, lucideTable })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <z-sheet [(zVisible)]="visible" zSide="right" zWidth="min(360px, 100vw)">
      <z-sheet-header class="border-b pb-4 pr-10">
        <z-sheet-title>View settings</z-sheet-title>
        <z-sheet-description>{{ viewName() }}</z-sheet-description>
      </z-sheet-header>
      <div class="py-5">
        <h3 class="mb-3 text-sm font-medium">Layout</h3>
        <div class="grid grid-cols-2 gap-3" role="group" aria-label="View layout">
          @for (option of layouts; track option.value) {
            <button
              type="button"
              class="flex flex-col items-center gap-3 rounded-lg border p-5 text-sm hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring"
              [class.bg-muted]="layout() === option.value"
              [class.border-primary]="layout() === option.value"
              [attr.aria-pressed]="layout() === option.value"
              (click)="layoutChange.emit(option.value)"
            >
              <ng-icon [name]="option.icon" class="size-6" />
              {{ option.label }}
            </button>
          }
        </div>
      </div>
    </z-sheet>
  `,
})
export class DatabaseViewSettings {
  readonly visible = model(false);
  readonly viewName = input.required<string>();
  readonly layout = input.required<'board' | 'table'>();
  readonly layoutChange = output<'board' | 'table'>();
  protected readonly layouts = [
    { value: 'board', label: 'Board', icon: 'lucideSquareKanban' },
    { value: 'table', label: 'Table', icon: 'lucideTable' },
  ] as const;
}
