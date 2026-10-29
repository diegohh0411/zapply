import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';
import { NgTemplateOutlet } from '@angular/common';
import { TemplateRef } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucidePlus, lucideSettings2, lucideSquareKanban, lucideTable } from '@ng-icons/lucide';
import { ZardButtonComponent } from './components/button';
import { DatabaseViewSettings } from './database-view-settings';

export interface DatabaseView {
  id: string;
  name: string;
  layout: 'board' | 'table';
}

@Component({
  selector: 'app-database',
  imports: [
    NgTemplateOutlet,
    NgIcon,
    ZardButtonComponent,
    DatabaseViewSettings,
  ],
  providers: [provideIcons({ lucidePlus, lucideSettings2, lucideSquareKanban, lucideTable })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'relative flex h-full min-h-0 flex-col overflow-hidden' },
  template: `
    <header class="flex flex-col gap-3 px-4 pt-4">
      <ng-content select="[databaseHeader]" />
      <div class="flex items-center gap-2 border-b">
        <nav class="flex min-w-0 flex-1 items-center gap-0.5 overflow-x-auto" aria-label="Saved views">
          <button
            type="button"
            class="view-tab"
            [class.view-tab-active]="!activeView()"
            [attr.aria-current]="!activeView() ? 'page' : null"
            (click)="select(null)"
          >
            <ng-icon [name]="!activeView() && layout() === 'table' ? 'lucideTable' : 'lucideSquareKanban'" class="size-4" />
            {{ defaultViewName() }}
          </button>
          @for (v of views(); track v.id) {
            <button
              type="button"
              class="view-tab"
              [class.view-tab-active]="activeView()?.id === v.id"
              [attr.aria-current]="activeView()?.id === v.id ? 'page' : null"
              (click)="select(v.id)"
            >
              <ng-icon [name]="v.layout === 'table' ? 'lucideTable' : 'lucideSquareKanban'" class="size-4" />
              {{ v.name }}
            </button>
          }
          <button type="button" class="view-tab-add" aria-label="Add view" (click)="save.emit()">
            <ng-icon name="lucidePlus" class="size-4" />
          </button>
        </nav>
        <div class="flex shrink-0 items-center gap-1 pb-1">
          @if (modified()) {
            <button
              type="button"
              class="rounded-md px-2 py-1 text-sm text-muted-foreground hover:bg-muted hover:text-foreground"
              (click)="save.emit()"
            >
              {{ activeView() ? 'Save changes' : 'Save as view' }}
            </button>
          }
          <button
            z-button
            zType="ghost"
            zSize="icon-sm"
            type="button"
            aria-label="View settings"
            [attr.aria-expanded]="viewSettingsOpen()"
            [class.bg-muted]="viewSettingsOpen()"
            (click)="viewSettingsOpen.set(!viewSettingsOpen())"
          >
            <ng-icon name="lucideSettings2" />
          </button>
        </div>
      </div>
      <ng-content select="[databaseFilters]" />
    </header>
    <section class="min-h-0 flex-1 overflow-auto pt-2">
      <ng-container *ngTemplateOutlet="content(); context: { $implicit: layout() }" />
    </section>
    @if (viewSettingsOpen()) {
      <app-database-view-settings
        [(visible)]="viewSettingsOpen"
        [viewName]="activeView()?.name ?? defaultViewName()"
        [layout]="layout()"
        [editable]="!!activeView()"
        (layoutChange)="layoutChange.emit($event)"
        (rename)="save.emit()"
        (remove)="remove.emit()"
      />
    }
  `,
})
export class Database {
  readonly views = input.required<readonly DatabaseView[]>();
  readonly activeViewId = input<string | null>(null);
  readonly defaultViewName = input('All items');
  readonly layout = input.required<'board' | 'table'>();
  readonly modified = input(false);
  readonly content = input.required<TemplateRef<unknown>>();
  readonly viewChange = output<string | null>();
  readonly layoutChange = output<'board' | 'table'>();
  readonly save = output<void>();
  readonly remove = output<void>();
  protected readonly viewSettingsOpen = signal(false);
  protected readonly activeView = computed(() =>
    this.views().find((v) => v.id === this.activeViewId()),
  );
  protected select(id: string | null): void {
    this.viewChange.emit(id);
  }
}
