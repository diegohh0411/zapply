import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucidePencil, lucidePlus, lucideTrash2 } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { ZardSonnerService } from '@/shared/components/sonner';
import { ZardTableImports } from '@/shared/components/table';

import { Api, type Schemas } from '../core/api';
import { onChanges } from './live';

type Org = Schemas['Organization'];
type OrgInput = Schemas['OrganizationInput'];

const TYPES: Schemas['OrganizationType'][] = ['company', 'education', 'nonprofit', 'government', 'client', 'other'];

@Component({
  selector: 'app-organizations-page',
  imports: [FormsModule, NgIcon, ZardButtonComponent, ZardInputComponent, ...ZardSelectImports, ...ZardTableImports],
  providers: [provideIcons({ lucidePlus, lucidePencil, lucideTrash2 })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-5xl px-4 py-6">
      <header class="mb-6">
        <h1 class="text-xl font-semibold tracking-tight">Organizations</h1>
        <p class="text-sm text-muted-foreground">Employers, schools and clients your experience links to. Renaming one updates every linked entry.</p>
      </header>

      <form class="mb-4 grid grid-cols-1 gap-2 rounded-lg border p-3 sm:grid-cols-[2fr_1fr_2fr_1.5fr_auto]" (ngSubmit)="save()">
        <input z-input name="name" [(ngModel)]="draft.name" placeholder="Name" aria-label="Name" required />
        <z-select name="type" [(ngModel)]="draft.type" aria-label="Type">
          @for (t of types; track t) { <z-select-item [zValue]="t">{{ t }}</z-select-item> }
        </z-select>
        <input z-input name="website" [(ngModel)]="draft.website" placeholder="Website" aria-label="Website" />
        <input z-input name="location" [(ngModel)]="draft.location" placeholder="Location" aria-label="Location" />
        <div class="flex gap-2">
          @if (editing()) {
            <button z-button zType="ghost" type="button" (click)="reset()">Cancel</button>
          }
          <button z-button type="submit" [zDisabled]="!draft.name.trim()">
            @if (editing()) { Save } @else { <ng-icon name="lucidePlus" /> Add }
          </button>
        </div>
      </form>

      <div class="overflow-x-auto rounded-lg border">
        <table z-table>
          <thead z-table-header>
            <tr z-table-row>
              <th z-table-head>Name</th><th z-table-head>Type</th><th z-table-head class="hidden md:table-cell">Website</th>
              <th z-table-head class="hidden md:table-cell">Location</th><th z-table-head class="w-24"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody z-table-body>
            @for (o of orgs(); track o.id) {
              <tr z-table-row>
                <td z-table-cell class="font-medium">{{ o.name }}</td>
                <td z-table-cell class="capitalize">{{ o.type }}</td>
                <td z-table-cell class="hidden max-w-56 truncate md:table-cell">
                  @if (o.website) { <a class="hover:underline" [href]="o.website" target="_blank" rel="noopener noreferrer">{{ o.website }}</a> }
                </td>
                <td z-table-cell class="hidden md:table-cell">{{ o.location }}</td>
                <td z-table-cell class="text-right">
                  <button z-button zType="ghost" zSize="icon-sm" type="button" (click)="edit(o)" [attr.aria-label]="'Edit ' + o.name"><ng-icon name="lucidePencil" /></button>
                  <button z-button zType="ghost" zSize="icon-sm" type="button" (click)="remove(o)" [attr.aria-label]="'Delete ' + o.name"><ng-icon name="lucideTrash2" /></button>
                </td>
              </tr>
            } @empty {
              <tr z-table-row><td z-table-cell colspan="5" class="py-10 text-center text-muted-foreground">No organizations yet.</td></tr>
            }
          </tbody>
        </table>
      </div>
    </div>
  `,
})
export class OrganizationsPage {
  private readonly api = inject(Api).client;
  private readonly toast = inject(ZardSonnerService);

  protected readonly types = TYPES;
  protected readonly orgs = signal<Org[]>([]);
  protected readonly editing = signal<string | null>(null);
  protected draft: Required<OrgInput> = blank();

  constructor() {
    void this.load();
    onChanges(['organization.'], () => void this.load());
  }

  private async load(): Promise<void> {
    try {
      this.orgs.set(Api.ok(await this.api.GET('/v1/organizations')));
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }

  protected edit(o: Org): void {
    this.editing.set(o.id);
    this.draft = { name: o.name, type: o.type, website: o.website, location: o.location };
  }

  protected reset(): void {
    this.editing.set(null);
    this.draft = blank();
  }

  protected async save(): Promise<void> {
    try {
      const id = this.editing();
      if (id) Api.ok(await this.api.PATCH('/v1/organizations/{id}', { params: { path: { id } }, body: this.draft }));
      else Api.ok(await this.api.POST('/v1/organizations', { body: this.draft }));
      this.reset();
      await this.load();
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }

  protected async remove(o: Org): Promise<void> {
    try {
      Api.ok(await this.api.DELETE('/v1/organizations/{id}', { params: { path: { id: o.id } } }));
      await this.load();
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }
}

function blank(): Required<OrgInput> {
  return { name: '', type: 'company', website: '', location: '' };
}
