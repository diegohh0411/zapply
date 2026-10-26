import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideGitMerge, lucidePencil, lucidePlus, lucideTrash2 } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardDialogImports } from '@/shared/components/dialog';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { ZardSonnerService } from '@/shared/components/sonner';

import { Api, type Schemas } from '../core/api';
import { onChanges } from './live';

type Skill = Schemas['Skill'];
type SkillType = Schemas['SkillType'];

const TYPES: { value: SkillType; label: string }[] = [
  { value: 'hard', label: 'Skills' },
  { value: 'tool', label: 'Tools' },
  { value: 'soft', label: 'Soft skills' },
  { value: 'language', label: 'Languages' },
];
const LEVELS = ['beginner', 'intermediate', 'advanced', 'expert'];
const CEFR = ['A1', 'A2', 'B1', 'B2', 'C1', 'C2', 'native'];

@Component({
  selector: 'app-skills-page',
  imports: [FormsModule, NgIcon, ZardButtonComponent, ZardInputComponent, ...ZardSelectImports, ...ZardDialogImports],
  providers: [provideIcons({ lucidePlus, lucidePencil, lucideTrash2, lucideGitMerge })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-5xl px-4 py-6">
      <header class="mb-6">
        <h1 class="text-xl font-semibold tracking-tight">Skills</h1>
        <p class="text-sm text-muted-foreground">Created as you tag experience. Merge duplicates so matching counts them once.</p>
      </header>

      <form class="mb-6 flex flex-wrap gap-2" (ngSubmit)="add()">
        <input z-input class="min-w-48 flex-1" name="name" [(ngModel)]="newName" placeholder="Add a skill, e.g. PostgreSQL" aria-label="Skill name" />
        <z-select class="w-40" name="type" [(ngModel)]="newType" aria-label="Type">
          @for (t of types; track t.value) { <z-select-item [zValue]="t.value">{{ t.label }}</z-select-item> }
        </z-select>
        <button z-button type="submit" [zDisabled]="!newName.trim()"><ng-icon name="lucidePlus" /> Add</button>
      </form>

      <div class="grid gap-6 md:grid-cols-2">
        @for (g of groups(); track g.type.value) {
          <section>
            <h2 class="mb-2 text-sm font-medium text-muted-foreground">{{ g.type.label }} <span class="tabular-nums">({{ g.skills.length }})</span></h2>
            <ul class="flex flex-wrap gap-2">
              @for (s of g.skills; track s.id) {
                <li class="group inline-flex items-center gap-1 rounded-full border bg-card py-1 pl-3 pr-1 text-sm">
                  {{ s.name }}
                  @if (s.proficiency) { <span class="text-xs text-muted-foreground">· {{ s.proficiency }}</span> }
                  <button type="button" class="rounded-full p-1 text-muted-foreground hover:bg-muted hover:text-foreground" (click)="edit(s)" [attr.aria-label]="'Edit ' + s.name">
                    <ng-icon name="lucidePencil" class="size-3" />
                  </button>
                </li>
              } @empty {
                <li class="text-sm text-muted-foreground">None yet.</li>
              }
            </ul>
          </section>
        }
      </div>
    </div>

    <z-dialog [(zVisible)]="dialogOpen" zWidth="min(460px, 95vw)">
      @if (editing(); as s) {
        <z-dialog-header><z-dialog-title>Edit skill</z-dialog-title></z-dialog-header>
        <form class="flex flex-col gap-4" (ngSubmit)="save(s)">
          <label class="field"><span>Name</span><input z-input name="ename" [(ngModel)]="form.name" /></label>
          <div class="grid grid-cols-2 gap-3">
            <div class="field">
              <span>Type</span>
              <z-select name="etype" [(ngModel)]="form.type" (zSelectionChange)="form.proficiency = ''">
                @for (t of types; track t.value) { <z-select-item [zValue]="t.value">{{ t.label }}</z-select-item> }
              </z-select>
            </div>
            <div class="field">
              <span>Proficiency</span>
              <z-select name="eprof" [(ngModel)]="form.proficiency" zPlaceholder="—">
                <z-select-item zValue="">—</z-select-item>
                @for (l of form.type === 'language' ? cefr : levels; track l) { <z-select-item [zValue]="l">{{ l }}</z-select-item> }
              </z-select>
            </div>
          </div>
          <div class="field">
            <span>Merge into another skill</span>
            <div class="flex gap-2">
              <z-select class="flex-1" name="into" [(ngModel)]="mergeInto" zPlaceholder="Choose a skill">
                @for (o of others(); track o.id) { <z-select-item [zValue]="o.id">{{ o.name }}</z-select-item> }
              </z-select>
              <button z-button zType="outline" type="button" [zDisabled]="!mergeInto" (click)="merge(s)"><ng-icon name="lucideGitMerge" /> Merge</button>
            </div>
            <small class="text-xs text-muted-foreground">Re-links everything tagged “{{ s.name }}”, then deletes it.</small>
          </div>
          <z-dialog-footer class="gap-2 sm:justify-between">
            <button z-button zType="destructive" type="button" (click)="remove(s)"><ng-icon name="lucideTrash2" /> Delete</button>
            <button z-button type="submit">Save</button>
          </z-dialog-footer>
        </form>
      }
    </z-dialog>
  `,
})
export class SkillsPage {
  private readonly api = inject(Api).client;
  private readonly toast = inject(ZardSonnerService);

  protected readonly types = TYPES;
  protected readonly levels = LEVELS;
  protected readonly cefr = CEFR;
  protected readonly skills = signal<Skill[]>([]);
  protected readonly groups = computed(() =>
    TYPES.map((type) => ({
      type,
      skills: this.skills()
        .filter((s) => s.type === type.value)
        .sort((a, b) => a.name.localeCompare(b.name)),
    })),
  );
  protected newName = '';
  protected newType: SkillType = 'hard';
  protected readonly editing = signal<Skill | null>(null);
  protected readonly dialogOpen = signal(false);
  protected form: { name: string; type: SkillType; proficiency: string } = { name: '', type: 'hard', proficiency: '' };
  protected mergeInto = '';
  protected readonly others = computed(() => this.skills().filter((s) => s.id !== this.editing()?.id));

  constructor() {
    void this.load();
    onChanges(['skill.'], () => void this.load());
  }

  private async load(): Promise<void> {
    try {
      this.skills.set(Api.ok(await this.api.GET('/v1/skills')));
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }

  protected async add(): Promise<void> {
    await this.run(async () => {
      Api.ok(await this.api.POST('/v1/skills', { body: { name: this.newName.trim(), type: this.newType } }));
      this.newName = '';
    });
  }

  protected edit(s: Skill): void {
    this.form = { name: s.name, type: s.type, proficiency: s.proficiency };
    this.mergeInto = '';
    this.editing.set(s);
    this.dialogOpen.set(true);
  }

  protected async save(s: Skill): Promise<void> {
    await this.run(async () => {
      Api.ok(await this.api.PATCH('/v1/skills/{id}', { params: { path: { id: s.id } }, body: this.form }));
      this.dialogOpen.set(false);
    });
  }

  protected async merge(s: Skill): Promise<void> {
    await this.run(async () => {
      Api.ok(await this.api.POST('/v1/skills/{id}/merge', { params: { path: { id: s.id } }, body: { into: this.mergeInto } }));
      this.dialogOpen.set(false);
      this.toast.success('Merged');
    });
  }

  protected async remove(s: Skill): Promise<void> {
    await this.run(async () => {
      Api.ok(await this.api.DELETE('/v1/skills/{id}', { params: { path: { id: s.id } } }));
      this.dialogOpen.set(false);
    });
  }

  private async run(fn: () => Promise<void>): Promise<void> {
    try {
      await fn();
      await this.load();
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }
}
