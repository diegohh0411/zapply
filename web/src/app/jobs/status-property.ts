import { CdkDrag, type CdkDragDrop, CdkDragHandle, CdkDropList, CdkDropListGroup } from '@angular/cdk/drag-drop';
import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideArrowLeft, lucideCheck, lucideChevronRight, lucideGripVertical, lucidePlus, lucideTrash2 } from '@ng-icons/lucide';

import { ZardSelectImports } from '@/shared/components/select';
import { ZardSonnerService } from '@/shared/components/sonner';

import { ApiError } from '../core/api';
import { STATUS_COLORS } from './format';
import { type JobStage, type JobStatus, JobsStore, STAGES } from './jobs.store';
import { StatusPill } from './status-pill';

/**
 * Notion's Status property editor: options sit in fixed groups, each with its
 * own "+"; drag to reorder or regroup; click an option to rename, recolor or
 * delete it. The groups are zapply's stages, which drive its automation.
 */
@Component({
  selector: 'app-status-property',
  imports: [CdkDropListGroup, CdkDropList, CdkDrag, CdkDragHandle, FormsModule, NgIcon, StatusPill, ...ZardSelectImports],
  providers: [provideIcons({ lucideArrowLeft, lucideCheck, lucideChevronRight, lucideGripVertical, lucidePlus, lucideTrash2 })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (editing(); as s) {
      <div class="flex flex-col gap-1 p-2">
        <button type="button" class="settings-row text-muted-foreground" (click)="close()">
          <ng-icon name="lucideArrowLeft" class="size-4" />
          <span class="flex-1 text-left">Options</span>
        </button>
        <input
          class="rounded-md border bg-muted/40 px-2 py-1 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
          aria-label="Option name"
          [value]="s.name"
          (change)="rename(s, $any($event.target).value)"
          (keydown.enter)="$any($event.target).blur()"
        />
        <button type="button" class="settings-row text-destructive" (click)="askDelete()">
          <ng-icon name="lucideTrash2" class="size-4" />
          <span class="flex-1 text-left">Delete</span>
        </button>
        @if (deleting()) {
          <div class="flex flex-col gap-2 rounded-md bg-muted p-2 text-sm">
            @if (count(s) > 0) {
              <span>Move its {{ count(s) }} {{ count(s) === 1 ? 'job' : 'jobs' }} to</span>
              <z-select [(zValue)]="moveTo">
                @for (o of others(); track o.id) {
                  <z-select-item [zValue]="o.id">{{ o.name }}</z-select-item>
                }
              </z-select>
            } @else {
              <span>Delete “{{ s.name }}”?</span>
            }
            <div class="flex justify-end gap-1">
              <button type="button" class="settings-row w-auto" (click)="deleting.set(false)">Cancel</button>
              <button type="button" class="settings-row w-auto text-destructive" (click)="remove(s)">Delete</button>
            </div>
          </div>
        }
        <div class="px-2 pb-1 pt-2 text-xs font-medium text-muted-foreground">Colors</div>
        @for (c of colors; track c) {
          <button type="button" class="settings-row" [attr.aria-pressed]="s.color === c" (click)="update(s, { color: c })">
            <app-status-pill class="capitalize" [status]="{ name: c, color: c }" />
            @if (s.color === c) { <ng-icon name="lucideCheck" class="ml-auto size-4" /> }
          </button>
        }
      </div>
    } @else {
      <div class="flex flex-col p-2" cdkDropListGroup>
        <div class="px-2 pb-1 text-xs font-medium text-muted-foreground">Options</div>
        @for (g of groups(); track g.value) {
          <section [attr.aria-label]="g.label">
            <div class="flex items-center justify-between px-2 pt-2 text-xs font-medium text-muted-foreground">
              {{ g.label }}
              <button type="button" class="settings-icon-btn" [attr.aria-label]="'Add an option to ' + g.label" (click)="startAdd(g.value)">
                <ng-icon name="lucidePlus" class="size-4" />
              </button>
            </div>
            @if (adding() === g.value) {
              <input
                #addInput
                class="mx-2 my-1 w-[calc(100%-1rem)] rounded-md border bg-muted/40 px-2 py-1 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
                aria-label="New option name"
                placeholder="Type a name"
                [ngModel]="''"
                (keydown.enter)="add(g.value, addInput.value)"
                (keydown.escape)="$event.stopPropagation(); adding.set(null)"
                (blur)="add(g.value, addInput.value)"
              />
            }
            <div class="min-h-6" cdkDropList [cdkDropListData]="g" (cdkDropListDropped)="dropped($event)">
              @for (s of g.statuses; track s.id) {
                <div cdkDrag [cdkDragData]="s" class="flex items-center rounded-md bg-popover hover:bg-muted">
                  <button type="button" cdkDragHandle class="cursor-grab p-1 text-muted-foreground" [attr.aria-label]="'Drag ' + s.name">
                    <ng-icon name="lucideGripVertical" class="size-4" />
                  </button>
                  <button type="button" class="flex min-w-0 flex-1 items-center py-1.5 pr-2" [attr.aria-label]="'Edit ' + s.name" (click)="editingId.set(s.id)">
                    <app-status-pill [status]="s" />
                    <ng-icon name="lucideChevronRight" class="ml-auto size-4 shrink-0 text-muted-foreground" />
                  </button>
                </div>
              }
            </div>
          </section>
        }
      </div>
    }
  `,
})
export class StatusProperty {
  protected readonly store = inject(JobsStore);
  private readonly toast = inject(ZardSonnerService);

  protected readonly colors = STATUS_COLORS;
  protected readonly editingId = signal<string | null>(null);
  protected readonly adding = signal<JobStage | null>(null);
  protected readonly deleting = signal(false);
  protected moveTo = '';

  protected readonly editing = computed(() => this.store.statusById().get(this.editingId() ?? ''));
  protected readonly others = computed(() => this.store.statuses().filter((s) => s.id !== this.editingId()));
  protected readonly groups = computed(() =>
    STAGES.map((g) => ({ ...g, statuses: this.store.statuses().filter((s) => s.stage === g.value) })),
  );

  protected close(): void {
    this.deleting.set(false);
    this.editingId.set(null);
  }

  protected count(s: JobStatus): number {
    return this.store.jobs().filter((j) => j.status_id === s.id).length;
  }

  protected rename(s: JobStatus, name: string): void {
    if (name.trim() && name.trim() !== s.name) void this.update(s, { name: name.trim() });
  }

  protected update(s: JobStatus, patch: Partial<Pick<JobStatus, 'name' | 'color' | 'stage'>>): Promise<void> {
    return this.run(() => this.store.updateStatus(s.id, { name: s.name, color: s.color, stage: s.stage, ...patch }));
  }

  protected startAdd(stage: JobStage): void {
    this.adding.set(stage);
    setTimeout(() => document.querySelector<HTMLInputElement>('app-status-property input')?.focus());
  }

  /** Runs on Enter and on the blur that follows it; only the first one adds. */
  protected async add(stage: JobStage, value: string): Promise<void> {
    if (this.adding() !== stage) return;
    this.adding.set(null);
    const name = value.trim();
    if (!name) return;
    const last = this.groups().find((g) => g.value === stage)?.statuses.at(-1);
    await this.run(async () => {
      const created = await this.store.createStatus({ name, stage });
      // The API appends to the end of the whole list; keep it inside its group.
      if (last) await this.store.moveStatus(created.id, last.id);
    });
  }

  protected async dropped(e: CdkDragDrop<ReturnType<StatusProperty['groups']>[number]>): Promise<void> {
    if (e.previousContainer === e.container && e.previousIndex === e.currentIndex) return;
    const s = e.item.data as JobStatus;
    const target = e.container.data.statuses.filter((x) => x.id !== s.id);
    const prev = target[e.currentIndex - 1];
    const next = target[e.currentIndex];
    await this.run(async () => {
      if (s.stage !== e.container.data.value) {
        await this.store.updateStatus(s.id, { name: s.name, color: s.color, stage: e.container.data.value });
      }
      if (prev || next) await this.store.moveStatus(s.id, prev?.id, prev ? undefined : next?.id);
    });
  }

  protected askDelete(): void {
    this.moveTo = this.others()[0]?.id ?? '';
    this.deleting.set(true);
  }

  protected async remove(s: JobStatus): Promise<void> {
    await this.run(async () => {
      await this.store.deleteStatus(s.id, this.count(s) > 0 ? this.moveTo : undefined);
      this.close();
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
