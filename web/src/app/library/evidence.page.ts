import { NgTemplateOutlet } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideArchive, lucideArchiveRestore, lucideEllipsis, lucidePencil, lucidePlus, lucideTrash2 } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardDropdownImports } from '@/shared/components/dropdown';
import { ZardSkeletonComponent } from '@/shared/components/skeleton';
import { ZardSonnerService } from '@/shared/components/sonner';
import { ZardSwitchComponent } from '@/shared/components/switch';

import { Api, type Schemas } from '../core/api';
import { EvidenceSheet, type EvidenceTarget, humanize } from './evidence-sheet';
import { onChanges } from './live';

type Evidence = Schemas['Evidence'];
type Category = Schemas['Category'];

const CATEGORIES: { value: Category; label: string; hint: string }[] = [
  { value: 'paid', label: 'Paid work', hint: 'Jobs, freelance, businesses, paid internships' },
  { value: 'unpaid', label: 'Unpaid', hint: 'Volunteering, personal projects, career breaks' },
  { value: 'education', label: 'Education', hint: 'Degrees, courses, certifications, licenses' },
];


@Component({
  selector: 'app-evidence-page',
  imports: [NgTemplateOutlet, NgIcon, ZardButtonComponent, ZardSkeletonComponent, ZardSwitchComponent, ...ZardDropdownImports, EvidenceSheet],
  providers: [provideIcons({ lucidePlus, lucidePencil, lucideEllipsis, lucideArchive, lucideArchiveRestore, lucideTrash2 })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-4xl px-4 py-6">
      <header class="mb-4 flex flex-wrap items-end gap-3">
        <div class="mr-auto">
          <h1 class="text-xl font-semibold tracking-tight">Experience</h1>
          <p class="text-sm text-muted-foreground">Everything you can truthfully claim. Each CV picks from here.</p>
        </div>
        <label class="flex items-center gap-2 text-sm text-muted-foreground">
          <z-switch zSize="sm" [zChecked]="archived()" (zCheckedChange)="setArchived($event)" /> Archived
        </label>
        <button z-button type="button" (click)="create()"><ng-icon name="lucidePlus" /> Add</button>
      </header>

      <nav class="mb-4 flex gap-1 border-b" aria-label="Category">
        @for (c of categories; track c.value) {
          <button
            type="button"
            class="-mb-px border-b-2 px-3 py-2 text-sm"
            [class]="category() === c.value ? 'border-brand font-medium text-foreground' : 'border-transparent text-muted-foreground hover:text-foreground'"
            [attr.aria-current]="category() === c.value ? 'page' : null"
            (click)="category.set(c.value)"
          >
            {{ c.label }} <span class="tabular-nums text-xs text-muted-foreground">{{ countIn(c.value) }}</span>
          </button>
        }
      </nav>

      @if (loading()) {
        <div class="flex flex-col gap-3"><z-skeleton class="h-24" /><z-skeleton class="h-24" /></div>
      } @else {
        <ul class="flex flex-col gap-3">
          @for (e of tree(); track e.item.id) {
            <li class="rounded-xl border bg-card p-4" [class.opacity-60]="e.item.archived">
              <ng-container *ngTemplateOutlet="row; context: { $implicit: e.item }" />
              @if (e.children.length) {
                <ul class="mt-3 flex flex-col gap-2 border-l pl-4">
                  @for (c of e.children; track c.id) {
                    <li [class.opacity-60]="c.archived"><ng-container *ngTemplateOutlet="row; context: { $implicit: c }" /></li>
                  }
                </ul>
              }
            </li>
          } @empty {
            <li class="rounded-xl border border-dashed p-10 text-center text-sm text-muted-foreground">
              Nothing here yet. {{ hint() }}.
              <div class="mt-3"><button z-button zType="outline" type="button" (click)="create()"><ng-icon name="lucidePlus" /> Add the first one</button></div>
            </li>
          }
        </ul>
      }
    </div>

    <ng-template #row let-e>
      <div class="flex items-start gap-3">
        <div class="min-w-0 flex-1">
          <p class="text-xs uppercase tracking-wide text-muted-foreground">
            {{ humanize(e.kind) }}@if (orgName(e); as o) { · {{ o }} }
          </p>
          <p class="font-medium leading-snug">{{ e.title }}</p>
          <p class="text-sm text-muted-foreground">{{ dates(e) }}@if (e.location) { · {{ e.location }} }</p>
          @if (e.highlights.length) {
            <ul class="mt-2 list-disc pl-5 text-sm">
              @for (h of e.highlights; track h.id) { <li>{{ h.text }}</li> }
            </ul>
          }
          @if (e.skills.length) {
            <div class="mt-2 flex flex-wrap gap-1">
              @for (s of e.skills; track s) { <span class="rounded-full bg-muted px-2 py-0.5 text-xs">{{ s }}</span> }
            </div>
          }
        </div>
        <z-dropdown-menu>
          <button dropdown-trigger z-button zType="ghost" zSize="icon-sm" type="button" [attr.aria-label]="'Actions for ' + e.title">
            <ng-icon name="lucideEllipsis" />
          </button>
          <z-dropdown-menu-item (click)="edit(e)"><ng-icon name="lucidePencil" /> Edit</z-dropdown-menu-item>
          @if (e.form === 'engagement' && !e.parent_id) {
            <z-dropdown-menu-item (click)="create(e)"><ng-icon name="lucidePlus" /> Add project, award…</z-dropdown-menu-item>
          }
          <z-dropdown-menu-item (click)="archive(e)">
            <ng-icon [name]="e.archived ? 'lucideArchiveRestore' : 'lucideArchive'" /> {{ e.archived ? 'Restore' : 'Archive' }}
          </z-dropdown-menu-item>
          <z-dropdown-menu-separator />
          <z-dropdown-menu-item zType="destructive" (click)="remove(e)"><ng-icon name="lucideTrash2" /> Delete</z-dropdown-menu-item>
        </z-dropdown-menu>
      </div>
    </ng-template>

    <app-evidence-sheet [target]="target()" [kinds]="kinds()" [organizations]="orgs()" [(visible)]="sheetOpen" (saved)="load()" />
  `,
})
export class EvidencePage {
  private readonly api = inject(Api).client;
  private readonly toast = inject(ZardSonnerService);

  protected readonly categories = CATEGORIES;
  protected readonly humanize = humanize;
  protected readonly category = signal<Category>('paid');
  protected readonly archived = signal(false);
  protected readonly loading = signal(true);
  protected readonly items = signal<Evidence[]>([]);
  protected readonly kinds = signal<Schemas['Kind'][]>([]);
  protected readonly orgs = signal<Schemas['Organization'][]>([]);
  protected readonly target = signal<EvidenceTarget | null>(null);
  protected readonly sheetOpen = signal(false);

  protected readonly hint = computed(() => CATEGORIES.find((c) => c.value === this.category())!.hint);
  private readonly orgById = computed(() => new Map(this.orgs().map((o) => [o.id, o.name])));

  /** Top-level entries of the category with their children, in library order. */
  protected readonly tree = computed(() => {
    const list = this.items().filter((e) => e.category === this.category());
    const children = new Map<string, Evidence[]>();
    for (const e of list) if (e.parent_id) children.set(e.parent_id, [...(children.get(e.parent_id) ?? []), e]);
    return list.filter((e) => !e.parent_id).map((item) => ({ item, children: children.get(item.id) ?? [] }));
  });

  constructor() {
    void this.load();
    void this.loadCatalog();
    onChanges(['evidence.', 'skill.'], () => void this.load());
    onChanges(['organization.'], () => void this.loadCatalog());
  }

  protected async load(): Promise<void> {
    try {
      const all: Evidence[] = [];
      let cursor: string | undefined;
      do {
        const page = Api.ok(
          await this.api.GET('/v1/evidences', { params: { query: { limit: 200, cursor, include_archived: this.archived() } } }),
        );
        all.push(...page.items);
        cursor = page.next_cursor;
      } while (cursor);
      this.items.set(all); // ordered by category, then position
    } catch (e) {
      this.toast.error((e as Error).message);
    } finally {
      this.loading.set(false);
    }
  }

  private async loadCatalog(): Promise<void> {
    try {
      const [kinds, orgs] = await Promise.all([this.api.GET('/v1/kinds'), this.api.GET('/v1/organizations')]);
      this.kinds.set(Api.ok(kinds));
      this.orgs.set(Api.ok(orgs));
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }

  protected countIn(c: Category): number {
    return this.items().filter((e) => e.category === c && !e.parent_id).length;
  }

  protected orgName(e: Evidence): string {
    return e.organization_id ? (this.orgById().get(e.organization_id) ?? '') : '';
  }

  protected dates(e: Evidence): string {
    const fmt = (d?: string) => {
      if (!d) return '';
      const [y, m] = d.split('-');
      return m ? new Date(+y, +m - 1).toLocaleDateString(undefined, { month: 'short', year: 'numeric' }) : y;
    };
    if (e.form !== 'engagement') return fmt(e.start) + (e.end ? ' – ' + fmt(e.end) : '');
    return `${fmt(e.start)} – ${e.is_current ? 'present' : fmt(e.end) || '?'}`;
  }

  protected setArchived(v: boolean): void {
    this.archived.set(v);
    void this.load();
  }

  protected create(parent?: Evidence): void {
    this.target.set({ new: true, category: this.category(), parent });
    this.sheetOpen.set(true);
  }

  protected edit(e: Evidence): void {
    this.target.set({ id: e.id });
    this.sheetOpen.set(true);
  }

  protected async archive(e: Evidence): Promise<void> {
    const path = e.archived ? '/v1/evidences/{id}/unarchive' : '/v1/evidences/{id}/archive';
    try {
      Api.ok(await this.api.POST(path, { params: { path: { id: e.id } } }));
      await this.load();
    } catch (err) {
      this.toast.error((err as Error).message);
    }
  }

  protected async remove(e: Evidence): Promise<void> {
    const kids = this.items().filter((x) => x.parent_id === e.id).length;
    if (!confirm(kids ? `Delete “${e.title}” and its ${kids} nested entries?` : `Delete “${e.title}”?`)) return;
    try {
      Api.ok(await this.api.DELETE('/v1/evidences/{id}', { params: { path: { id: e.id }, query: { cascade: kids > 0 } } }));
      await this.load();
    } catch (err) {
      this.toast.error((err as Error).message);
    }
  }
}
