import { NgTemplateOutlet } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, input, model, output, signal, TemplateRef } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideArrowLeft,
  lucideChevronRight,
  lucideCircleDot,
  lucidePencil,
  lucideSquareKanban,
  lucideTable,
  lucideTrash2,
  lucideX,
} from '@ng-icons/lucide';
import type { DatabaseProperty } from './database';

// Notion-style view settings: a panel docked to the right edge of the
// database (not the viewport), with menu rows that drill into sub-pages.
@Component({
  selector: 'app-database-view-settings',
  imports: [NgIcon, NgTemplateOutlet],
  providers: [
    provideIcons({
      lucideArrowLeft,
      lucideChevronRight,
      lucideCircleDot,
      lucidePencil,
      lucideSquareKanban,
      lucideTable,
      lucideTrash2,
      lucideX,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: {
    role: 'dialog',
    'aria-label': 'View settings',
    class:
      'absolute inset-y-0 right-0 z-20 flex w-72 max-w-full flex-col border-l bg-popover text-popover-foreground shadow-xl',
    '(keydown.escape)': 'close()',
  },
  template: `
    @if (editing(); as p) {
      <div class="flex items-center gap-2 px-3 pb-1 pt-3">
        <button type="button" class="settings-icon-btn" aria-label="Back" (click)="property.set(null); page.set('properties')">
          <ng-icon name="lucideArrowLeft" class="size-4" />
        </button>
        <h2 class="flex-1 text-sm font-semibold">Edit property</h2>
        <button type="button" class="settings-icon-btn" aria-label="Close view settings" (click)="close()">
          <ng-icon name="lucideX" class="size-4" />
        </button>
      </div>
      <div class="flex items-center gap-2 px-3 py-2">
        <ng-icon [name]="p.icon" class="size-4 text-muted-foreground" />
        <span class="min-w-0 flex-1 truncate rounded-md border bg-muted/40 px-2 py-1 text-sm">{{ p.name }}</span>
      </div>
      <div class="min-h-0 flex-1 overflow-y-auto">
        <ng-container *ngTemplateOutlet="propertyEditor()" />
      </div>
    } @else if (page() === 'properties') {
      <div class="flex items-center gap-2 px-3 pb-1 pt-3">
        <button type="button" class="settings-icon-btn" aria-label="Back" (click)="page.set('main')">
          <ng-icon name="lucideArrowLeft" class="size-4" />
        </button>
        <h2 class="flex-1 text-sm font-semibold">Edit properties</h2>
        <button type="button" class="settings-icon-btn" aria-label="Close view settings" (click)="close()">
          <ng-icon name="lucideX" class="size-4" />
        </button>
      </div>
      <div class="flex flex-col p-1">
        @for (p of properties(); track p.id) {
          <button type="button" class="settings-row" (click)="property.set(p.id)">
            <ng-icon [name]="p.icon" class="size-4 text-muted-foreground" />
            <span class="flex-1 text-left">{{ p.name }}</span>
            <ng-icon name="lucideChevronRight" class="size-4 text-muted-foreground" />
          </button>
        }
      </div>
    } @else if (page() === 'main') {
      <div class="flex items-center gap-2 px-3 pb-1 pt-3">
        <h2 class="flex-1 text-sm font-semibold">View settings</h2>
        <button type="button" class="settings-icon-btn" aria-label="Close view settings" (click)="close()">
          <ng-icon name="lucideX" class="size-4" />
        </button>
      </div>
      <div class="flex items-center gap-2 px-3 py-2">
        <span class="grid size-7 shrink-0 place-items-center rounded-md border">
          <ng-icon [name]="layoutIcon()" class="size-4" />
        </span>
        <span class="min-w-0 flex-1 truncate rounded-md border bg-muted/40 px-2 py-1 text-sm">{{ viewName() }}</span>
      </div>
      <div class="flex flex-col p-1">
        <button type="button" class="settings-row" (click)="page.set('layout')">
          <ng-icon [name]="layoutIcon()" class="size-4 text-muted-foreground" />
          <span class="flex-1 text-left">Layout</span>
          <span class="text-muted-foreground">{{ layout() === 'table' ? 'Table' : 'Board' }}</span>
          <ng-icon name="lucideChevronRight" class="size-4 text-muted-foreground" />
        </button>
        @if (properties().length) {
          <button type="button" class="settings-row" (click)="page.set('properties')">
            <ng-icon name="lucideCircleDot" class="size-4 text-muted-foreground" />
            <span class="flex-1 text-left">Edit properties</span>
            <ng-icon name="lucideChevronRight" class="size-4 text-muted-foreground" />
          </button>
        }
      </div>
      @if (editable()) {
        <div class="mt-auto flex flex-col border-t p-1">
          <button type="button" class="settings-row" (click)="rename.emit()">
            <ng-icon name="lucidePencil" class="size-4 text-muted-foreground" />
            <span class="flex-1 text-left">Rename or update view</span>
          </button>
          <button type="button" class="settings-row text-destructive" (click)="remove.emit()">
            <ng-icon name="lucideTrash2" class="size-4" />
            <span class="flex-1 text-left">Delete view</span>
          </button>
        </div>
      }
    } @else {
      <div class="flex items-center gap-2 px-3 pb-1 pt-3">
        <button type="button" class="settings-icon-btn" aria-label="Back" (click)="page.set('main')">
          <ng-icon name="lucideArrowLeft" class="size-4" />
        </button>
        <h2 class="flex-1 text-sm font-semibold">Layout</h2>
        <button type="button" class="settings-icon-btn" aria-label="Close view settings" (click)="close()">
          <ng-icon name="lucideX" class="size-4" />
        </button>
      </div>
      <div class="grid grid-cols-3 gap-2 p-3" role="group" aria-label="View layout">
        @for (option of layouts; track option.value) {
          <button
            type="button"
            class="flex flex-col items-center gap-1.5 rounded-md border px-2 py-3 text-xs text-muted-foreground hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring"
            [class.border-blue-500]="layout() === option.value"
            [class.text-foreground]="layout() === option.value"
            [attr.aria-pressed]="layout() === option.value"
            (click)="layoutChange.emit(option.value)"
          >
            <ng-icon [name]="option.icon" class="size-5" />
            {{ option.label }}
          </button>
        }
      </div>
    }
  `,
})
export class DatabaseViewSettings {
  readonly visible = model(false);
  readonly property = model<string | null>(null);
  readonly properties = input<readonly DatabaseProperty[]>([]);
  readonly propertyEditor = input<TemplateRef<unknown> | null>(null);
  readonly viewName = input.required<string>();
  readonly layout = input.required<'board' | 'table'>();
  readonly editable = input(false);
  readonly layoutChange = output<'board' | 'table'>();
  readonly rename = output<void>();
  readonly remove = output<void>();
  protected readonly page = signal<'main' | 'layout' | 'properties'>('main');
  protected readonly editing = computed(() => this.properties().find((p) => p.id === this.property()));
  protected readonly layouts = [
    { value: 'table', label: 'Table', icon: 'lucideTable' },
    { value: 'board', label: 'Board', icon: 'lucideSquareKanban' },
  ] as const;
  protected layoutIcon(): string {
    return this.layout() === 'table' ? 'lucideTable' : 'lucideSquareKanban';
  }
  protected close(): void {
    this.page.set('main');
    this.property.set(null);
    this.visible.set(false);
  }
}
