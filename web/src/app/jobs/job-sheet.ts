import { DatePipe, NgTemplateOutlet } from '@angular/common';
import { ChangeDetectionStrategy, Component, DestroyRef, effect, inject, input, model, output, signal, untracked } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideArchive,
  lucideArchiveRestore,
  lucideArrowRight,
  lucideEllipsis,
  lucideExternalLink,
  lucideMessageSquare,
  lucidePencil,
  lucidePlus,
  lucideTrash2,
} from '@ng-icons/lucide';

import { ZardAlertDialogImports } from '@/shared/components/alert-dialog';
import { ZardButtonComponent } from '@/shared/components/button';
import { ZardDropdownImports } from '@/shared/components/dropdown';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { ZardSheetImports } from '@/shared/components/sheet';
import { ZardSkeletonComponent } from '@/shared/components/skeleton';
import { ZardSonnerService } from '@/shared/components/sonner';
import { ZardTabsImports } from '@/shared/components/tabs';
import { ZardTextareaComponent } from '@/shared/components/textarea';

import { ApiError } from '../core/api';
import { EMPLOYMENT_TYPES, SALARY_PERIODS, STATUS_DOT, WORK_MODES } from './format';
import { type Job, type JobActivity, type JobInput, JobsStore } from './jobs.store';

/** What the sheet shows: an existing job, or a new one in a status. */
export type SheetTarget = { id: string } | { new: true; statusId?: string };

interface Form {
  status_id: string;
  title: string;
  company: string;
  url: string;
  location: string;
  work_mode: string;
  employment_type: string;
  salary_min: number | null;
  salary_max: number | null;
  salary_currency: string;
  salary_period: string;
  source: string;
  applied_on: string;
  description: string;
  notes: string;
}

const emptyForm = (statusId = ''): Form => ({
  status_id: statusId,
  title: '',
  company: '',
  url: '',
  location: '',
  work_mode: '',
  employment_type: '',
  salary_min: null,
  salary_max: null,
  salary_currency: '',
  salary_period: '',
  source: '',
  applied_on: '',
  description: '',
  notes: '',
});

@Component({
  selector: 'app-job-sheet',
  imports: [
    DatePipe,
    NgTemplateOutlet,
    FormsModule,
    NgIcon,
    ZardButtonComponent,
    ZardInputComponent,
    ZardTextareaComponent,
    ZardSkeletonComponent,
    ...ZardSheetImports,
    ...ZardSelectImports,
    ...ZardTabsImports,
    ...ZardDropdownImports,
    ...ZardAlertDialogImports,
  ],
  providers: [
    provideIcons({
      lucideExternalLink,
      lucideEllipsis,
      lucideArchive,
      lucideArchiveRestore,
      lucideTrash2,
      lucideMessageSquare,
      lucideArrowRight,
      lucidePlus,
      lucidePencil,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './job-sheet.html',
})
export class JobSheet {
  protected readonly store = inject(JobsStore);
  private readonly toast = inject(ZardSonnerService);

  readonly target = input<SheetTarget | null>(null);
  readonly visible = model(false);
  readonly opened = output<string>(); // a job id to switch to (duplicates)

  protected readonly dot = STATUS_DOT;
  protected readonly workModes = WORK_MODES;
  protected readonly employmentTypes = EMPLOYMENT_TYPES;
  protected readonly salaryPeriods = SALARY_PERIODS;

  protected form: Form = emptyForm();
  protected readonly job = signal<Job | null>(null);
  protected readonly isNew = signal(false);
  protected readonly loading = signal(false);
  protected readonly saving = signal(false);
  protected readonly dirty = signal(false);
  protected readonly errors = signal<Record<string, string>>({});
  protected readonly activities = signal<JobActivity[]>([]);
  protected readonly confirmDelete = signal(false);
  protected note = '';
  protected readonly editingNote = signal<string | null>(null);
  protected noteEdit = '';
  private etag = '';

  constructor() {
    effect(() => {
      const t = this.target();
      untracked(() => void this.load(t));
    });
    this.store.activityChanged.pipe(takeUntilDestroyed(inject(DestroyRef))).subscribe((jobId) => {
      if (jobId === this.job()?.id) void this.loadActivities(jobId);
    });
    // Another tab or device saved this job: refresh unless there are local edits.
    effect(() => {
      const cur = this.job();
      if (!cur) return;
      const live = this.store.jobs().find((j) => j.id === cur.id);
      if (live && live.version > cur.version && !untracked(this.dirty)) untracked(() => void this.load({ id: cur.id }, true));
    });
  }

  private async load(t: SheetTarget | null, quiet = false): Promise<void> {
    this.errors.set({});
    this.dirty.set(false);
    this.confirmDelete.set(false);
    this.editingNote.set(null);
    if (!t) return;
    if ('new' in t) {
      this.isNew.set(true);
      this.job.set(null);
      this.activities.set([]);
      this.form = emptyForm(t.statusId ?? this.store.statuses()[0]?.id ?? '');
      return;
    }
    this.isNew.set(false);
    if (!quiet) this.loading.set(true);
    try {
      const { job, etag } = await this.store.get(t.id);
      this.job.set(job);
      this.etag = etag;
      this.form = toForm(job);
      await this.loadActivities(t.id);
    } catch (e) {
      this.toast.error('Could not load the job', { description: (e as Error).message });
      this.visible.set(false);
    } finally {
      this.loading.set(false);
    }
  }

  private async loadActivities(id: string): Promise<void> {
    this.activities.set(await this.store.activities(id));
  }

  protected touch(): void {
    this.dirty.set(true);
  }

  protected async save(): Promise<void> {
    this.saving.set(true);
    this.errors.set({});
    try {
      const input = toInput(this.form);
      if (this.isNew()) {
        const job = await this.store.create(input);
        this.toast.success('Job saved');
        this.opened.emit(job.id);
      } else {
        const cur = this.job()!;
        const { job, etag } = await this.store.save(cur.id, this.etag, input);
        this.job.set(job);
        this.etag = etag;
        this.form = toForm(job);
        this.dirty.set(false);
        this.toast.success('Saved');
      }
    } catch (e) {
      this.handleError(e);
    } finally {
      this.saving.set(false);
    }
  }

  /** Status changes from the header apply at once, like a drag on the board. */
  protected async changeStatus(statusId: string): Promise<void> {
    if (this.isNew()) {
      this.form.status_id = statusId;
      return;
    }
    const cur = this.job();
    if (!cur || statusId === cur.status_id) return;
    try {
      await this.store.move(cur.id, statusId);
      await this.load({ id: cur.id }, true);
    } catch (e) {
      this.handleError(e);
    }
  }

  protected async archive(archived: boolean): Promise<void> {
    const cur = this.job();
    if (!cur) return;
    try {
      const job = await this.store.setArchived(cur.id, archived);
      this.job.set(job);
      this.toast.success(archived ? 'Archived' : 'Restored');
      await this.load({ id: cur.id }, true);
    } catch (e) {
      this.handleError(e);
    }
  }

  protected async remove(): Promise<void> {
    const cur = this.job();
    if (!cur) return;
    try {
      await this.store.delete(cur.id);
      this.confirmDelete.set(false);
      this.visible.set(false);
      this.toast.success('Job deleted');
    } catch (e) {
      this.handleError(e);
    }
  }

  protected async addNote(): Promise<void> {
    const cur = this.job();
    const body = this.note.trim();
    if (!cur || !body) return;
    try {
      await this.store.addNote(cur.id, body);
      this.note = '';
      await this.loadActivities(cur.id);
    } catch (e) {
      this.handleError(e);
    }
  }

  protected startEdit(a: JobActivity): void {
    this.editingNote.set(a.id);
    this.noteEdit = a.body;
  }

  protected async saveNote(a: JobActivity): Promise<void> {
    const cur = this.job();
    if (!cur) return;
    try {
      await this.store.updateNote(cur.id, a.id, this.noteEdit.trim());
      this.editingNote.set(null);
      await this.loadActivities(cur.id);
    } catch (e) {
      this.handleError(e);
    }
  }

  protected async deleteNote(a: JobActivity): Promise<void> {
    const cur = this.job();
    if (!cur) return;
    try {
      await this.store.deleteNote(cur.id, a.id);
      await this.loadActivities(cur.id);
    } catch (e) {
      this.handleError(e);
    }
  }

  protected statusName(id: string): string {
    return this.store.statusById().get(id)?.name ?? '';
  }

  private handleError(e: unknown): void {
    if (e instanceof ApiError) {
      const existing = (e.problem as { existing_id?: string } | undefined)?.existing_id;
      if (e.status === 409 && existing) {
        this.toast.warning('You already saved this posting', {
          action: { label: 'Open it', onClick: () => this.opened.emit(existing) },
        });
        this.errors.set({ url: 'Already saved' });
        return;
      }
      if (e.status === 412) {
        this.toast.warning('This job changed elsewhere', { description: 'Reloaded the latest version; re-apply your edits.' });
        const cur = this.job();
        if (cur) void this.load({ id: cur.id }, true);
        return;
      }
      if (e.fieldErrors.length) {
        this.errors.set(Object.fromEntries(e.fieldErrors.map((f) => [f.field, f.message])));
        this.toast.error('Check the highlighted fields');
        return;
      }
    }
    this.toast.error((e as Error).message);
  }
}

function toForm(j: Job): Form {
  return {
    status_id: j.status_id,
    title: j.title,
    company: j.company,
    url: j.url,
    location: j.location,
    work_mode: j.work_mode ?? '',
    employment_type: j.employment_type ?? '',
    salary_min: j.salary_min ?? null,
    salary_max: j.salary_max ?? null,
    salary_currency: j.salary_currency,
    salary_period: j.salary_period ?? '',
    source: j.source,
    applied_on: j.applied_on ?? '',
    description: j.description,
    notes: j.notes,
  };
}

function toInput(f: Form): JobInput {
  const num = (v: number | null) => (v === null || (v as unknown) === '' || Number.isNaN(+v) ? undefined : Math.round(+v));
  return {
    status_id: f.status_id || undefined,
    title: f.title,
    company: f.company,
    url: f.url,
    location: f.location,
    work_mode: (f.work_mode || undefined) as JobInput['work_mode'],
    employment_type: (f.employment_type || undefined) as JobInput['employment_type'],
    salary_min: num(f.salary_min),
    salary_max: num(f.salary_max),
    salary_currency: f.salary_currency,
    salary_period: (f.salary_period || undefined) as JobInput['salary_period'],
    source: f.source,
    applied_on: f.applied_on || undefined,
    description: f.description,
    notes: f.notes,
  };
}
