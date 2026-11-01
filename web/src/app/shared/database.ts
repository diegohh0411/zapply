import {
  ChangeDetectionStrategy,
  Component,
  computed,
  ElementRef,
  inject,
  input,
  output,
  signal,
  viewChild,
} from '@angular/core';
import { NgTemplateOutlet } from '@angular/common';
import { TemplateRef } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideCopy,
  lucideLink,
  lucidePencil,
  lucidePlus,
  lucideSettings2,
  lucideSquareKanban,
  lucideTable,
  lucideTrash2,
} from '@ng-icons/lucide';
import { ZardSonnerService } from './components/sonner';
import { ZardContextMenuDirective, ZardContextMenuImports } from './components/context-menu';
import { ZardDropdownImports } from './components/dropdown';
import { ZardButtonComponent } from './components/button';
import { DatabaseViewSettings } from './database-view-settings';

export interface DatabaseProperty {
  id: string;
  name: string;
  icon: string;
}

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
    ...ZardContextMenuImports,
    ...ZardDropdownImports,
    DatabaseViewSettings,
  ],
  providers: [provideIcons({
      lucideCopy,
      lucideLink,
      lucidePencil,
      lucidePlus,
      lucideSettings2,
      lucideSquareKanban,
      lucideTable,
      lucideTrash2,
    }),
  ],
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
            @if (renamingId() === v.id) {
              <span class="view-tab view-tab-active">
                <ng-icon [name]="v.layout === 'table' ? 'lucideTable' : 'lucideSquareKanban'" class="size-4" />
                <input
                  #renameInput
                  class="w-32 bg-transparent outline-none"
                  aria-label="View name"
                  [value]="v.name"
                  (keydown.enter)="commitRename(v, renameInput.value)"
                  (keydown.escape)="renamingId.set(null)"
                  (blur)="commitRename(v, renameInput.value)"
                />
              </span>
            } @else {
              <button
                type="button"
                class="view-tab"
                z-context-menu
                #tabTrigger="zContextMenu"
                [zContextMenuTriggerFor]="tabMenu"
                [class.view-tab-active]="activeView()?.id === v.id"
                [attr.aria-current]="activeView()?.id === v.id ? 'page' : null"
                (contextmenu)="menuView.set(v)"
                (click)="activeView()?.id === v.id ? openMenu(v, tabTrigger, $event) : select(v.id)"
              >
                <ng-icon [name]="v.layout === 'table' ? 'lucideTable' : 'lucideSquareKanban'" class="size-4" />
                {{ v.name }}
              </button>
            }
          }
          <z-dropdown-menu>
            <button dropdown-trigger type="button" class="view-tab-add" aria-label="Add view">
              <ng-icon name="lucidePlus" class="size-4" />
            </button>
            <z-dropdown-menu-label>Add a new view</z-dropdown-menu-label>
            <z-dropdown-menu-item (click)="addView.emit('table')">
              <ng-icon name="lucideTable" /> Table
            </z-dropdown-menu-item>
            <z-dropdown-menu-item (click)="addView.emit('board')">
              <ng-icon name="lucideSquareKanban" /> Board
            </z-dropdown-menu-item>
          </z-dropdown-menu>
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
            (click)="toggleSettings()"
          >
            <ng-icon name="lucideSettings2" />
          </button>
          <button
            z-button
            zSize="sm"
            type="button"
            class="bg-blue-600 text-white hover:bg-blue-700"
            (click)="add.emit()"
          >
            {{ addLabel() }} <ng-icon name="lucidePlus" />
          </button>
        </div>
      </div>
      <ng-content select="[databaseFilters]" />
    </header>
    <section class="min-h-0 flex-1 overflow-auto pt-2">
      <ng-container *ngTemplateOutlet="content(); context: { $implicit: layout() }" />
    </section>
    <z-dropdown-menu-content #tabMenu="zDropdownMenuContent" class="w-52">
      @if (menuView(); as v) {
        <z-dropdown-menu-item (click)="startRename(v)"><ng-icon name="lucidePencil" /> Rename</z-dropdown-menu-item>
        <z-dropdown-menu-item (click)="editView(v)"><ng-icon name="lucideSettings2" /> Edit view</z-dropdown-menu-item>
        <z-dropdown-menu-item (click)="duplicateView.emit(v.id)"><ng-icon name="lucideCopy" /> Duplicate</z-dropdown-menu-item>
        <z-dropdown-menu-item (click)="copyLink(v)"><ng-icon name="lucideLink" /> Copy link to view</z-dropdown-menu-item>
        <z-dropdown-menu-separator />
        <z-dropdown-menu-item zType="destructive" (click)="remove.emit(v.id)"><ng-icon name="lucideTrash2" /> Delete view</z-dropdown-menu-item>
      }
    </z-dropdown-menu-content>
    @if (viewSettingsOpen()) {
      <app-database-view-settings
        [(visible)]="viewSettingsOpen"
        [(property)]="editingProperty"
        [properties]="properties()"
        [propertyEditor]="propertyEditor()"
        [viewName]="activeView()?.name ?? defaultViewName()"
        [layout]="layout()"
        [editable]="!!activeView()"
        (layoutChange)="layoutChange.emit($event)"
        (rename)="save.emit()"
        (remove)="remove.emit(activeView()!.id)"
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
  readonly addLabel = input('New');
  /** Editable properties; propertyEditor renders the one being edited. */
  readonly properties = input<readonly DatabaseProperty[]>([]);
  readonly propertyEditor = input<TemplateRef<unknown> | null>(null);
  readonly add = output<void>();
  readonly viewChange = output<string | null>();
  readonly layoutChange = output<'board' | 'table'>();
  readonly save = output<void>();
  readonly remove = output<string>();
  readonly addView = output<'board' | 'table'>();
  readonly renameView = output<{ id: string; name: string }>();
  readonly duplicateView = output<string>();
  protected readonly menuView = signal<DatabaseView | null>(null);
  protected readonly renamingId = signal<string | null>(null);
  private readonly toast = inject(ZardSonnerService);
  private readonly renameInput = viewChild<ElementRef<HTMLInputElement>>('renameInput');
  protected readonly viewSettingsOpen = signal(false);
  protected readonly editingProperty = signal<string | null>(null);
  protected readonly activeView = computed(() =>
    this.views().find((v) => v.id === this.activeViewId()),
  );
  /** Opens a property's editor in the view settings panel. */
  editProperty(id: string): void {
    this.editingProperty.set(id);
    this.viewSettingsOpen.set(true);
  }
  protected toggleSettings(): void {
    this.editingProperty.set(null);
    this.viewSettingsOpen.update((v) => !v);
  }
  /** Clicking the active tab opens its menu below it, like Notion. */
  protected openMenu(v: DatabaseView, trigger: ZardContextMenuDirective, event: MouseEvent): void {
    this.menuView.set(v);
    trigger.open(event);
  }
  protected startRename(v: DatabaseView): void {
    this.renamingId.set(v.id);
    setTimeout(() => this.renameInput()?.nativeElement.select());
  }
  protected commitRename(v: DatabaseView, name: string): void {
    if (this.renamingId() !== v.id) return;
    this.renamingId.set(null);
    const trimmed = name.trim();
    if (trimmed && trimmed !== v.name) this.renameView.emit({ id: v.id, name: trimmed });
  }
  protected editView(v: DatabaseView): void {
    this.select(v.id);
    this.viewSettingsOpen.set(true);
  }
  protected async copyLink(v: DatabaseView): Promise<void> {
    const url = new URL(location.href);
    url.searchParams.set('view', v.id);
    await navigator.clipboard.writeText(url.toString());
    this.toast.success('Copied link to view');
  }
  protected select(id: string | null): void {
    this.viewChange.emit(id);
  }
}
