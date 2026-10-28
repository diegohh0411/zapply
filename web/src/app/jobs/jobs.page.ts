import {
  ChangeDetectionStrategy,
  Component,
  computed,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucidePlus, lucideSearch, lucideSettings2, lucideX } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardDialogImports } from '@/shared/components/dialog';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSkeletonComponent } from '@/shared/components/skeleton';
import { ZardSonnerService } from '@/shared/components/sonner';
import { ZardSwitchComponent } from '@/shared/components/switch';

import { ConfirmService } from '../core/confirm.service';
import { type BoardMove, JobBoard } from './job-board';
import { JobSheet, type SheetTarget } from './job-sheet';
import { JobTable } from './job-table';
import {
  type JobFilters,
  type JobStage,
  type JobSummary,
  type JobView,
  type JobViewInput,
  JobsStore,
  STAGES,
} from './jobs.store';
import { Database } from '../shared/database';
import { StatusesDialog } from './statuses-dialog';

type Layout = 'board' | 'table';

interface ViewState {
  layout: Layout;
  filters: JobFilters;
}

const DEFAULT_STATE: ViewState = { layout: 'board', filters: {} };

@Component({
  selector: 'app-jobs-page',
  imports: [
    FormsModule,
    NgIcon,
    ZardButtonComponent,
    ZardInputComponent,
    ZardSwitchComponent,
    ZardSkeletonComponent,
    ...ZardDialogImports,
    JobBoard,
    JobTable,
    JobSheet,
    StatusesDialog,
    Database,
  ],
  providers: [
    provideIcons({
      lucidePlus,
      lucideSearch,
      lucideSettings2,
      lucideX,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './jobs.page.html',
})
export class JobsPage {
  protected readonly store = inject(JobsStore);
  private readonly router = inject(Router);
  private readonly toast = inject(ZardSonnerService);
  private readonly confirm = inject(ConfirmService);

  /** ?job=<id> opens a job; ?job=new a blank one. ?view=<id> selects a saved view. */
  readonly job = input<string>();
  readonly view = input<string>();

  protected readonly stages = STAGES;
  protected readonly state = signal<ViewState>(DEFAULT_STATE);
  protected readonly sheetTarget = signal<SheetTarget | null>(null);
  protected readonly sheetOpen = signal(false);
  protected readonly statusesOpen = signal(false);
  protected readonly saveOpen = signal(false);
  protected viewName = '';
  private newInStatus: string | undefined;

  protected readonly activeView = computed(
    () => this.store.views().find((v) => v.id === this.view()) ?? null,
  );

  /** Whether the current filters differ from the selected view (or from "All jobs"). */
  protected readonly modified = computed(() => {
    const v = this.activeView();
    const base: ViewState = v ? { layout: v.layout, filters: v.filters } : DEFAULT_STATE;
    return JSON.stringify(normalize(base)) !== JSON.stringify(normalize(this.state()));
  });

  protected readonly effectiveFilters = computed(() => {
    const f = this.state().filters;
    return { ...f, status_ids: f.status_ids?.filter((id) => this.store.statusById().has(id)) };
  });

  /** Columns shown on the board: those matching the stage and status filters. */
  protected readonly columns = computed(() => {
    const f = this.effectiveFilters();
    return this.store
      .statuses()
      .filter(
        (s) =>
          (!f.stages?.length || f.stages.includes(s.stage)) &&
          (!f.status_ids?.length || f.status_ids.includes(s.id)),
      );
  });

  protected readonly filtered = computed(() => {
    const shown = new Set(this.columns().map((s) => s.id));
    return filterJobs(this.store.jobs(), this.effectiveFilters()).filter((j) =>
      shown.has(j.status_id),
    );
  });

  protected readonly counts = computed(() => {
    const by = this.store.statusById();
    const c = { total: 0, active: 0, offer: 0 };
    for (const j of this.store.jobs()) {
      if (j.archived) continue;
      c.total++;
      const st = by.get(j.status_id)?.stage;
      if (st === 'active') c.active++;
      if (st === 'offer') c.offer++;
    }
    return c;
  });

  constructor() {
    void this.store.ensureLoaded();

    // Apply a saved view when it's selected (or its definition changes).
    effect(() => {
      const v = this.activeView();
      untracked(() => {
        this.state.set(v ? { layout: v.layout, filters: { ...v.filters } } : DEFAULT_STATE);
        void this.store.setIncludeArchived(!!this.state().filters.include_archived);
      });
    });

    // The sheet follows ?job=.
    effect(() => {
      const id = this.job();
      untracked(() => {
        if (!id) {
          this.sheetOpen.set(false);
          return;
        }
        this.sheetTarget.set(id === 'new' ? { new: true, statusId: this.newInStatus } : { id });
        this.sheetOpen.set(true);
      });
    });
  }

  // Filters

  protected setQ(q: string): void {
    this.patch({ q: q || undefined });
  }

  protected toggleStage(s: JobStage): void {
    const cur = this.state().filters.stages ?? [];
    const next = cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s];
    this.patch({ stages: next.length ? next : undefined });
  }

  protected hasStage(s: JobStage): boolean {
    return this.state().filters.stages?.includes(s) ?? false;
  }

  protected setArchived(v: boolean): void {
    this.patch({ include_archived: v || undefined });
    void this.store.setIncludeArchived(v);
  }

  protected setLayout(layout: Layout): void {
    this.state.update((s) => ({ ...s, layout }));
  }

  protected clearFilters(): void {
    this.state.update((s) => ({ ...s, filters: {} }));
    void this.store.setIncludeArchived(false);
  }

  private patch(f: Partial<JobFilters>): void {
    this.state.update((s) => ({ ...s, filters: { ...s.filters, ...f } }));
  }

  // Views

  protected selectViewId(id: string | null): void {
    this.selectView(this.store.views().find((v) => v.id === id) ?? null);
  }

  protected selectView(v: JobView | null): void {
    void this.router.navigate([], {
      queryParams: { view: v?.id ?? null },
      queryParamsHandling: 'merge',
    });
  }

  protected openSave(): void {
    this.viewName = this.activeView()?.name ?? '';
    this.saveOpen.set(true);
  }

  protected async saveAsNew(): Promise<void> {
    const name = this.viewName.trim();
    if (!name) return;
    try {
      const v = await this.store.createView(this.viewInput(name));
      this.saveOpen.set(false);
      this.selectView(v);
      this.toast.success(`Saved view “${name}”`);
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }

  protected async updateView(): Promise<void> {
    const v = this.activeView();
    if (!v) return;
    try {
      await this.store.updateView(v.id, this.viewInput(this.viewName.trim() || v.name));
      this.saveOpen.set(false);
      this.toast.success('View updated');
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }

  protected async deleteView(): Promise<void> {
    const v = this.activeView();
    if (!v) return;
    if (
      !(await this.confirm.ask({
        title: `Delete the view “${v.name}”?`,
        description: 'Only the saved view goes; your jobs stay.',
      }))
    )
      return;
    try {
      await this.store.deleteView(v.id);
      this.selectView(null);
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }

  private viewInput(name: string): JobViewInput {
    const s = this.state();
    return { name, layout: s.layout, filters: normalize(s).filters, sort: '-updated_at' };
  }

  // Jobs

  protected open(id: string): void {
    void this.router.navigate([], { queryParams: { job: id }, queryParamsHandling: 'merge' });
  }

  protected newJob(statusId?: string): void {
    this.newInStatus = statusId;
    if (this.job() === 'new') this.sheetTarget.set({ new: true, statusId });
    this.open('new');
  }

  protected sheetVisible(v: boolean): void {
    this.sheetOpen.set(v);
    if (!v)
      void this.router.navigate([], { queryParams: { job: null }, queryParamsHandling: 'merge' });
  }

  protected async move(m: BoardMove): Promise<void> {
    try {
      await this.store.move(m.jobId, m.statusId, m.afterId, m.beforeId);
    } catch (e) {
      this.toast.error('Could not move the job', { description: (e as Error).message });
    }
  }
}

function filterJobs(jobs: JobSummary[], f: JobFilters): JobSummary[] {
  const q = f.q?.trim().toLowerCase();
  const statusIds = f.status_ids?.length ? new Set(f.status_ids) : null;
  return jobs.filter(
    (j) =>
      (f.include_archived || !j.archived) &&
      (!statusIds || statusIds.has(j.status_id)) &&
      (!q || j.title.toLowerCase().includes(q) || j.company.toLowerCase().includes(q)),
  );
}

/** Drops empty filter values so equal states compare equal. */
function normalize(s: ViewState): ViewState {
  const f: JobFilters = {};
  if (s.filters.q?.trim()) f.q = s.filters.q.trim();
  if (s.filters.stages?.length) f.stages = [...s.filters.stages].sort();
  if (s.filters.status_ids?.length) f.status_ids = [...s.filters.status_ids].sort();
  if (s.filters.include_archived) f.include_archived = true;
  return { layout: s.layout, filters: f };
}
