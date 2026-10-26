import { ChangeDetectionStrategy, Component, computed, inject, model, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideChevronDown, lucideChevronUp, lucidePlus, lucideTrash2 } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardDialogImports } from '@/shared/components/dialog';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { ZardSonnerService } from '@/shared/components/sonner';

import { ApiError } from '../core/api';
import { STATUS_COLORS, STATUS_DOT } from './format';
import { type JobStage, type JobStatus, JobsStore, STAGES } from './jobs.store';

/** Rename, recolor, reorder, add and delete board columns. */
@Component({
  selector: 'app-statuses-dialog',
  imports: [FormsModule, NgIcon, ZardButtonComponent, ZardInputComponent, ...ZardDialogImports, ...ZardSelectImports],
  providers: [provideIcons({ lucideChevronUp, lucideChevronDown, lucideTrash2, lucidePlus })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <z-dialog [(zVisible)]="visible" zWidth="min(640px, 95vw)">
      <z-dialog-header>
        <z-dialog-title>Board columns</z-dialog-title>
        <z-dialog-description>
          The stage tells zapply what a column means, whatever you call it: <em>saved</em> (not applied yet),
          <em>in process</em>, <em>offer</em> or <em>closed</em>.
        </z-dialog-description>
      </z-dialog-header>

      <ul class="flex max-h-[55vh] flex-col gap-2 overflow-y-auto py-1">
        @for (s of store.statuses(); track s.id; let i = $index, last = $last) {
          <li class="flex flex-wrap items-center gap-2 rounded-lg border p-2">
            <div class="flex flex-col">
              <button z-button zType="ghost" zSize="icon-xs" type="button" [zDisabled]="i === 0" (click)="up(i)" aria-label="Move up"><ng-icon name="lucideChevronUp" /></button>
              <button z-button zType="ghost" zSize="icon-xs" type="button" [zDisabled]="last" (click)="down(i)" aria-label="Move down"><ng-icon name="lucideChevronDown" /></button>
            </div>
            <div class="flex gap-1" role="radiogroup" [attr.aria-label]="s.name + ' color'">
              @for (c of colors; track c) {
                <button
                  type="button"
                  role="radio"
                  [attr.aria-checked]="s.color === c"
                  [attr.aria-label]="c"
                  class="size-4 rounded-full ring-offset-2 ring-offset-background"
                  [class]="dot[c]"
                  [class.ring-2]="s.color === c"
                  [class.ring-foreground]="s.color === c"
                  (click)="update(s, { color: c })"
                ></button>
              }
            </div>
            <input
              z-input
              class="h-8 min-w-32 flex-1"
              [ngModel]="s.name"
              (change)="rename(s, $any($event.target).value)"
              [attr.aria-label]="'Name of ' + s.name"
            />
            <z-select class="w-32" [zValue]="s.stage" (zSelectionChange)="update(s, { stage: $any($event) })" aria-label="Stage">
              @for (st of stages; track st.value) {
                <z-select-item [zValue]="st.value">{{ st.label }}</z-select-item>
              }
            </z-select>
            <button z-button zType="ghost" zSize="icon-sm" type="button" (click)="askDelete(s)" [attr.aria-label]="'Delete ' + s.name">
              <ng-icon name="lucideTrash2" />
            </button>
            @if (deleting()?.id === s.id) {
              <div class="flex w-full flex-wrap items-center gap-2 rounded-md bg-muted p-2 text-sm">
                @if (count(s) > 0) {
                  <span>Move its {{ count(s) }} job(s) to</span>
                  <z-select class="w-44" [(zValue)]="moveTo">
                    @for (o of others(); track o.id) {
                      <z-select-item [zValue]="o.id">{{ o.name }}</z-select-item>
                    }
                  </z-select>
                } @else {
                  <span>Delete “{{ s.name }}”?</span>
                }
                <span class="flex-1"></span>
                <button z-button zType="ghost" zSize="sm" type="button" (click)="deleting.set(null)">Cancel</button>
                <button z-button zType="destructive" zSize="sm" type="button" (click)="remove(s)">Delete</button>
              </div>
            }
          </li>
        }
      </ul>

      <form class="flex flex-wrap items-center gap-2 border-t pt-3" (ngSubmit)="add()">
        <input z-input class="h-8 min-w-40 flex-1" name="name" [(ngModel)]="newName" placeholder="New column, e.g. Take-home" aria-label="New column name" />
        <z-select class="w-32" name="stage" [(ngModel)]="newStage" aria-label="New column stage">
          @for (st of stages; track st.value) {
            <z-select-item [zValue]="st.value">{{ st.label }}</z-select-item>
          }
        </z-select>
        <button z-button zSize="sm" type="submit" [zDisabled]="!newName.trim()"><ng-icon name="lucidePlus" /> Add</button>
      </form>
    </z-dialog>
  `,
})
export class StatusesDialog {
  protected readonly store = inject(JobsStore);
  private readonly toast = inject(ZardSonnerService);

  readonly visible = model(false);

  protected readonly dot = STATUS_DOT;
  protected readonly colors = STATUS_COLORS;
  protected readonly stages = STAGES;
  protected newName = '';
  protected newStage: JobStage = 'active';
  protected readonly deleting = signal<JobStatus | null>(null);
  protected moveTo = '';
  protected readonly others = computed(() => this.store.statuses().filter((s) => s.id !== this.deleting()?.id));

  protected count(s: JobStatus): number {
    return this.store.jobs().filter((j) => j.status_id === s.id).length;
  }

  protected rename(s: JobStatus, name: string): void {
    if (name.trim() && name.trim() !== s.name) void this.update(s, { name: name.trim() });
  }

  protected async update(s: JobStatus, patch: Partial<Pick<JobStatus, 'name' | 'color' | 'stage'>>): Promise<void> {
    await this.run(() => this.store.updateStatus(s.id, { name: s.name, color: s.color, stage: s.stage, ...patch }));
  }

  protected async up(i: number): Promise<void> {
    const list = this.store.statuses();
    await this.run(() => this.store.moveStatus(list[i].id, undefined, list[i - 1].id));
  }

  protected async down(i: number): Promise<void> {
    const list = this.store.statuses();
    await this.run(() => this.store.moveStatus(list[i].id, list[i + 1].id));
  }

  protected async add(): Promise<void> {
    await this.run(async () => {
      await this.store.createStatus({ name: this.newName.trim(), stage: this.newStage });
      this.newName = '';
    });
  }

  protected askDelete(s: JobStatus): void {
    this.deleting.set(s);
    this.moveTo = this.others()[0]?.id ?? '';
  }

  protected async remove(s: JobStatus): Promise<void> {
    await this.run(async () => {
      await this.store.deleteStatus(s.id, this.count(s) > 0 ? this.moveTo : undefined);
      this.deleting.set(null);
    });
  }

  private async run(fn: () => Promise<void>): Promise<void> {
    try {
      await fn();
    } catch (e) {
      const msg = e instanceof ApiError && e.fieldErrors.length ? e.fieldErrors.map((f) => f.message).join('; ') : (e as Error).message;
      this.toast.error(msg);
    }
  }
}
