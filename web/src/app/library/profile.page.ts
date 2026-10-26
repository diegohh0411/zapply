import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucidePlus, lucideTrash2 } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSkeletonComponent } from '@/shared/components/skeleton';
import { ZardSonnerService } from '@/shared/components/sonner';
import { ZardTextareaComponent } from '@/shared/components/textarea';

import { Api, ApiError, etagOf, type Schemas } from '../core/api';
import { fieldMap, onChanges } from './live';

@Component({
  selector: 'app-profile-page',
  imports: [FormsModule, NgIcon, ZardButtonComponent, ZardInputComponent, ZardTextareaComponent, ZardSkeletonComponent],
  providers: [provideIcons({ lucidePlus, lucideTrash2 })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-3xl px-4 py-6">
      <header class="mb-6">
        <h1 class="text-xl font-semibold tracking-tight">Profile</h1>
        <p class="text-sm text-muted-foreground">The header of every CV zapply makes for you.</p>
      </header>

      @if (loading()) {
        <div class="flex flex-col gap-3"><z-skeleton class="h-9" /><z-skeleton class="h-9" /><z-skeleton class="h-32" /></div>
      } @else {
        <form class="grid grid-cols-1 gap-4 sm:grid-cols-2" (ngSubmit)="save()" (input)="dirty.set(true)">
          <label class="field"><span>Full name</span><input z-input name="full_name" [(ngModel)]="p.full_name" autocomplete="name" /></label>
          <label class="field"><span>Headline</span><input z-input name="headline" [(ngModel)]="p.headline" placeholder="Backend engineer" /></label>
          <label class="field">
            <span>Email</span><input z-input name="email" type="email" [(ngModel)]="p.email" autocomplete="email" />
            @if (errors()['email']; as e) { <small class="err">{{ e }}</small> }
          </label>
          <label class="field"><span>Phone</span><input z-input name="phone" [(ngModel)]="p.phone" autocomplete="tel" /></label>
          <label class="field sm:col-span-2"><span>Location</span><input z-input name="location" [(ngModel)]="p.location" /></label>
          <label class="field sm:col-span-2">
            <span>Summary</span>
            <textarea z-textarea name="summary" rows="5" [(ngModel)]="p.summary"></textarea>
          </label>

          <fieldset class="flex flex-col gap-2 sm:col-span-2">
            <legend class="mb-1.5 text-sm font-medium">Links</legend>
            @for (l of p.links; track $index; let i = $index) {
              <div class="flex gap-2">
                <input z-input class="w-36" [name]="'label' + i" [(ngModel)]="l.label" placeholder="GitHub" aria-label="Link label" />
                <input z-input class="flex-1" [name]="'url' + i" [(ngModel)]="l.url" placeholder="https://" aria-label="Link URL" />
                <button z-button zType="ghost" zSize="icon" type="button" (click)="removeLink(i)" aria-label="Remove link"><ng-icon name="lucideTrash2" /></button>
              </div>
              @if (errors()['links[' + i + '].url']; as e) { <small class="err">{{ e }}</small> }
            }
            <button z-button zType="outline" zSize="sm" type="button" class="self-start" (click)="addLink()"><ng-icon name="lucidePlus" /> Add link</button>
          </fieldset>

          <div class="flex items-center justify-end gap-3 sm:col-span-2">
            @if (dirty()) { <span class="text-xs text-muted-foreground">Unsaved changes</span> }
            <button z-button type="submit" [zLoading]="saving()" [zDisabled]="!dirty()">Save profile</button>
          </div>
        </form>
      }
    </div>
  `,
})
export class ProfilePage {
  private readonly api = inject(Api).client;
  private readonly toast = inject(ZardSonnerService);

  protected p: Schemas['Profile'] = blank();
  protected readonly loading = signal(true);
  protected readonly saving = signal(false);
  protected readonly dirty = signal(false);
  protected readonly errors = signal<Record<string, string>>({});
  private etag = '"0"';

  constructor() {
    void this.load();
    onChanges(['profile.'], () => {
      if (!this.dirty()) void this.load();
    });
  }

  private async load(): Promise<void> {
    try {
      const r = await this.api.GET('/v1/profile');
      this.p = structuredClone(Api.ok(r));
      this.etag = etagOf(r);
    } catch (e) {
      this.toast.error((e as Error).message);
    } finally {
      this.loading.set(false);
    }
  }

  protected addLink(): void {
    this.p.links = [...(this.p.links ?? []), { label: '', url: '' }];
    this.dirty.set(true);
  }

  protected removeLink(i: number): void {
    this.p.links = (this.p.links ?? []).filter((_, j) => j !== i);
    this.dirty.set(true);
  }

  protected async save(): Promise<void> {
    this.saving.set(true);
    this.errors.set({});
    try {
      const { version: _v, updated_at: _u, ...body } = this.p;
      const r = await this.api.PUT('/v1/profile', { params: { header: { 'If-Match': this.etag } }, body });
      this.p = structuredClone(Api.ok(r));
      this.etag = etagOf(r);
      this.dirty.set(false);
      this.toast.success('Profile saved');
    } catch (e) {
      if (e instanceof ApiError && e.status === 412) {
        this.toast.warning('Your profile changed elsewhere; reloaded it.');
        this.dirty.set(false);
        await this.load();
      } else {
        this.errors.set(fieldMap(e));
        this.toast.error((e as Error).message);
      }
    } finally {
      this.saving.set(false);
    }
  }
}

function blank(): Schemas['Profile'] {
  return { full_name: '', headline: '', email: '', phone: '', location: '', links: [], summary: '', version: 0 };
}
