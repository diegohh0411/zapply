import { ChangeDetectionStrategy, Component, computed, input, output, signal } from '@angular/core';
import { NgTemplateOutlet } from '@angular/common';
import { TemplateRef } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideEllipsis } from '@ng-icons/lucide';
import { ZardButtonComponent } from './components/button';
import { ZardDropdownImports } from './components/dropdown';
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
    ...ZardDropdownImports,
    DatabaseViewSettings,
  ],
  providers: [provideIcons({ lucideEllipsis })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'flex h-full min-h-0 flex-col' },
  template: `
    <header class="flex flex-col gap-3 border-b px-4 pb-3 pt-4">
      <ng-content select="[databaseHeader]" />
      <nav class="-mb-1 flex items-center gap-1 overflow-x-auto" aria-label="Saved views">
        <button
          type="button"
          class="view-tab"
          [class.view-tab-active]="!activeView()"
          [attr.aria-current]="!activeView() ? 'page' : null"
          (click)="select(null)"
        >
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
            {{ v.name }}
          </button>
        }
        @if (modified()) {
          <button
            z-button
            zType="ghost"
            zSize="sm"
            type="button"
            class="ml-1 text-brand"
            (click)="save.emit()"
          >
            {{ activeView() ? 'Save changes…' : 'Save as view…' }}
          </button>
        }
        @if (activeView(); as v) {
          <z-dropdown-menu class="ml-auto">
            <button
              dropdown-trigger
              z-button
              zType="ghost"
              zSize="icon-sm"
              type="button"
              [attr.aria-label]="'Options for view ' + v.name"
            >
              <ng-icon name="lucideEllipsis" />
            </button>
            <z-dropdown-menu-item (click)="save.emit()">Rename or update…</z-dropdown-menu-item>
            <z-dropdown-menu-item zType="destructive" (click)="remove.emit()"
              >Delete view</z-dropdown-menu-item
            >
          </z-dropdown-menu>
        }
      </nav>

      <ng-content select="[databaseFilters]" />
    </header>
    <section class="min-h-0 flex-1 overflow-auto pt-2">
      <ng-container *ngTemplateOutlet="content(); context: { $implicit: layout() }" />
    </section>
    <app-database-view-settings
      [(visible)]="viewSettingsOpen"
      [viewName]="activeView()?.name ?? defaultViewName()"
      [layout]="layout()"
      (layoutChange)="layoutChange.emit($event)"
    />
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
    this.viewSettingsOpen.set(true);
  }
}
