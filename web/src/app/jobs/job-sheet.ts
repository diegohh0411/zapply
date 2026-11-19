import { DatePipe, NgTemplateOutlet } from '@angular/common';
import {
  ChangeDetectionStrategy,
  ChangeDetectorRef,
  Component,
  DestroyRef,
  effect,
  inject,
  input,
  model,
  output,
  signal,
  viewChild,
  type ElementRef,
  untracked,
} from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideArrowLeft,
  lucideFileText,
  lucideMail,
  lucideHistory,
  lucideBriefcaseBusiness,
  lucideArchive,
  lucideArchiveRestore,
  lucideArrowRight,
  lucideEllipsis,
  lucideExternalLink,
  lucideMessageSquare,
  lucidePencil,
  lucidePlus,
  lucideTrash2,
  lucideX,
  lucideLink,
  lucideSparkles,
  lucideTriangleAlert,
} from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardDatePickerComponent } from '@/shared/components/date-picker';
import { ZardDialogComponent, ZardDialogImports } from '@/shared/components/dialog';
import { ZardDropdownImports } from '@/shared/components/dropdown';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { ZardSkeletonComponent } from '@/shared/components/skeleton';
import { ZardSonnerService } from '@/shared/components/sonner';
import { ZardTextareaComponent } from '@/shared/components/textarea';
import { ZardToggleGroupComponent } from '@/shared/components/toggle-group';
import { describeLink, readPosting } from './posting-link';

import { CvTab } from './cv/cv-tab';
import { MailTab } from './mail-tab';
import { StatusSelect } from './status-select';
import { AuthService } from '../core/auth.service';
import { ApiError } from '../core/api';
import { ConfirmService } from '../core/confirm.service';
import { EMPLOYMENT_TYPES, SALARY_PERIODS, WORK_MODES } from './format';
import {
  type Job,
  type JobActivity,
  type JobInput,
  JobsStore,
  type PostingRead,
} from './jobs.store';

export type JobTab = 'details' | 'cv' | 'mail' | 'activity';
const workspaceTabs = [
  { id: 'details', label: 'Details', icon: 'lucideBriefcaseBusiness' },
  { id: 'cv', label: 'Match & CV', icon: 'lucideFileText' },
  { id: 'mail', label: 'Mail', icon: 'lucideMail' },
  { id: 'activity', label: 'Activity', icon: 'lucideHistory' },
] as const;

/** What the workspace shows: an existing job, or a new one in a status. */
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

// Form dates are local YYYY-MM-DD strings; the date picker works with Date values.
function parseIsoDate(iso: string): Date | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(iso);
  return match ? new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3])) : null;
}

function formatIsoDate(value: Date | Date[] | null): string {
  const date = Array.isArray(value) ? value[0] : value;
  if (!date) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

@Component({
  selector: 'app-job-sheet',
  imports: [
    CvTab,
    MailTab,
    StatusSelect,
    DatePipe,
    NgTemplateOutlet,
    FormsModule,
    NgIcon,
    ZardButtonComponent,
    ZardInputComponent,
    ZardTextareaComponent,
    ZardSkeletonComponent,
    ...ZardSelectImports,
    ZardDatePickerComponent,
    ZardToggleGroupComponent,
    ...ZardDropdownImports,
    ...ZardDialogImports,
  ],
  providers: [
    provideIcons({
      lucideArrowLeft,
      lucideFileText,
      lucideMail,
      lucideHistory,
      lucideBriefcaseBusiness,
      lucideExternalLink,
      lucideEllipsis,
      lucideArchive,
      lucideArchiveRestore,
      lucideTrash2,
      lucideMessageSquare,
      lucideArrowRight,
      lucidePlus,
      lucidePencil,
      lucideX,
      lucideLink,
      lucideSparkles,
      lucideTriangleAlert,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './job-sheet.html',
  styleUrl: './job-sheet.css',
})
export class JobSheet {
  private readonly auth = inject(AuthService);
  protected readonly store = inject(JobsStore);
  private readonly toast = inject(ZardSonnerService);
  private readonly confirm = inject(ConfirmService);

  readonly target = input<SheetTarget | null>(null);
  readonly visible = model(false);
  readonly tab = input<string>();
  readonly tabChange = output<JobTab>();
  protected readonly tabs = workspaceTabs;
  private readonly navigation = viewChild<ElementRef<HTMLElement>>('workspaceNavigation');
  protected readonly activeTab = signal<JobTab>('details');
  protected readonly visitedTabs = signal<ReadonlySet<JobTab>>(new Set(['details']));
  readonly opened = output<string>(); // a job id to switch to (duplicates)

  protected readonly workModes = WORK_MODES;
  protected readonly employmentTypes = EMPLOYMENT_TYPES;
  protected readonly salaryPeriods = SALARY_PERIODS;
  protected readonly workModeItems = WORK_MODES.map((o) => ({ value: o.value, label: o.label }));
  private readonly cdr = inject(ChangeDetectorRef);

  /** New jobs start from the posting link, then a short review. */
  protected readonly step = signal<'link' | 'review'>('link');
  /** Fields the posting filled in, marked in the review so they get checked. */
  protected readonly filled = signal<ReadonlySet<string>>(new Set());

  protected linkHint(): string {
    return describeLink(this.form.url);
  }

  protected duplicate(): { id: string; title: string } | null {
    const url = this.form.url.trim();
    if (!url) return null;
    const hit = this.store.jobs().find((j) => j.url === url);
    return hit ? { id: hit.id, title: hit.title } : null;
  }

  protected statusStage(id: string): string {
    return this.store.statusById().get(id)?.stage ?? '';
  }

  /** The server is reading the posting's page (Continue shows a spinner). */
  protected readonly reading = signal(false);
  /** Why the page couldn't be read, shown on the review step. */
  protected readonly readProblem = signal('');
  /** Values the last read filled: a new link replaces them, but not edits made since. */
  private autoFilled: Record<string, unknown> = {};

  protected enterByHand(): void {
    this.readProblem.set('');
    this.step.set('review');
  }

  protected async continueFromLink(): Promise<void> {
    if (this.reading()) return;
    const generation = this.generation;
    const url = this.form.url.trim();
    this.readProblem.set('');
    this.errors.set({});
    let page: PostingRead | null = null;
    if (url) {
      this.reading.set(true);
      try {
        page = await this.store.readPosting(url);
      } catch (e) {
        if (generation !== this.generation) return;
        // 503: no scraper on this server, so the link and text are all there is.
        if (!(e instanceof ApiError && e.status === 503)) {
          this.readProblem.set(
            e instanceof ApiError && e.fieldErrors.length
              ? e.fieldErrors[0].message
              : 'Couldn’t read the page right now.',
          );
        }
      } finally {
        if (generation === this.generation) this.reading.set(false);
      }
      if (generation !== this.generation) return;
    }

    const form = this.form as unknown as Record<string, unknown>;
    const blank = emptyForm() as unknown as Record<string, unknown>;
    for (const [key, value] of Object.entries(this.autoFilled)) {
      if (form[key] === value) form[key] = blank[key];
    }
    const filled = new Set<string>();
    const autoFilled: Record<string, unknown> = {};
    const fill = (key: string, value: unknown) => {
      if (value === undefined || value === null || value === '') return;
      if (form[key] !== '' && form[key] !== null) return;
      form[key] = value;
      filled.add(key);
      autoFilled[key] = value;
    };
    const { description, ...fields } = page ?? { description: '' };
    fill('description', description);
    for (const [key, value] of Object.entries(readPosting(url, this.form.description, fields))) {
      fill(key, value);
    }
    this.autoFilled = autoFilled;
    this.filled.set(filled);
    this.step.set('review');
    this.cdr.markForCheck();
  }

  protected form: Form = emptyForm();
  protected readonly job = signal<Job | null>(null);
  protected readonly isNew = signal(false);
  protected readonly captureOpen = signal(false);
  private closing = false;
  protected readonly loading = signal(false);
  protected readonly saving = signal(false);
  protected readonly dirty = signal(false);
  protected readonly errors = signal<Record<string, string>>({});
  protected readonly activities = signal<JobActivity[]>([]);
  protected readonly activityError = signal('');
  protected readonly activityLoading = signal(false);
  protected note = '';
  protected readonly editingNote = signal<string | null>(null);
  protected noteEdit = '';
  private etag = '';
  private generation = 0;
  private activitiesRequest = 0;

  constructor() {
    // Statuses can arrive after the sheet opens: start new jobs in the first "saved" status.
    effect(() => {
      const statuses = this.store.statuses();
      if (!this.isNew() || !statuses.length) return;
      untracked(() => {
        if (this.form.status_id) return;
        this.form.status_id = (statuses.find((s) => s.stage === 'saved') ?? statuses[0]).id;
        this.cdr.markForCheck();
      });
    });
    effect(() => {
      this.captureOpen.set(this.visible() && this.isNew());
    });
    effect(() => {
      const tab = this.tab();
      const isNew = this.isNew();
      untracked(() => this.selectTab(isNew ? 'details' : this.validTab(tab), false));
    });
    let identity = this.auth.identity();
    effect(() => {
      const next = this.auth.identity();
      if (identity === next) return;
      identity = next;
      untracked(() => {
        this.generation++;
        this.activitiesRequest++;
        this.job.set(null);
        this.form = emptyForm();
        this.activities.set([]);
        this.activityError.set('');
        this.activityLoading.set(false);
        this.errors.set({});
        this.dirty.set(false);
        this.saving.set(false);
        this.loading.set(false);
        this.activeTab.set('details');
        this.visitedTabs.set(new Set(['details']));
        this.isNew.set(false);
        this.editingNote.set(null);
        this.note = '';
        this.noteEdit = '';
        this.etag = '';
        this.visible.set(false);
      });
    });
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
      if (live && live.version > cur.version && !untracked(this.dirty))
        untracked(() => void this.load({ id: cur.id }, true));
    });
  }

  private async load(t: SheetTarget | null, quiet = false): Promise<void> {
    const generation = ++this.generation;
    this.activitiesRequest++;
    if (!quiet) {
      this.note = '';
      this.noteEdit = '';
      this.activeTab.set(this.validTab(this.tab()));
      this.visitedTabs.set(new Set(['details', this.activeTab()]));
      this.errors.set({});
      this.dirty.set(false);
      this.editingNote.set(null);
      this.saving.set(false);
      this.job.set(null);
      this.activities.set([]);
      this.activityError.set('');
      this.activityLoading.set(false);
      this.loading.set(false);
    }
    if (!t) return;
    if ('new' in t) {
      this.isNew.set(true);
      this.step.set('link');
      this.filled.set(new Set());
      this.readProblem.set('');
      this.autoFilled = {};
      this.selectTab('details', false);
      this.form = emptyForm(
        t.statusId ??
          this.store.statuses().find((s) => s.stage === 'saved')?.id ??
          this.store.statuses()[0]?.id ??
          '',
      );
      return;
    }
    this.isNew.set(false);
    if (!quiet) this.loading.set(true);
    try {
      const { job, etag } = await this.store.get(t.id);
      if (generation !== this.generation) return;
      this.applyJob(job, etag, quiet);
      await this.loadActivities(t.id);
    } catch (e) {
      if (generation !== this.generation) return;
      this.toast.error('Could not load the job', { description: (e as Error).message });
      this.visible.set(false);
    } finally {
      if (generation === this.generation) this.loading.set(false);
    }
  }

  /** Merge server metadata while keeping fields edited against our baseline. */
  private applyJob(job: Job, etag: string, preserveDraft: boolean): void {
    const cur = this.job();
    if (cur && cur.id === job.id && cur.version > job.version) return;
    const next = toForm(job);
    if (preserveDraft && this.dirty() && cur) {
      const baseline = toForm(cur);
      for (const key of Object.keys(next) as (keyof Form)[]) {
        if (this.form[key] !== baseline[key]) Object.assign(next, { [key]: this.form[key] });
      }
    } else {
      this.dirty.set(false);
    }
    this.job.set(job);
    this.etag = etag;
    this.form = next;
  }

  private async loadActivities(id: string): Promise<void> {
    const generation = this.generation;
    const seq = ++this.activitiesRequest;
    this.activityLoading.set(true);
    this.activityError.set('');
    const current = () =>
      generation === this.generation && seq === this.activitiesRequest && this.job()?.id === id;
    try {
      const rows = await this.store.activities(id);
      if (current()) this.activities.set(rows);
    } catch (e) {
      if (current())
        this.activityError.set(e instanceof Error ? e.message : 'Could not load activity.');
    } finally {
      if (current()) this.activityLoading.set(false);
    }
  }

  protected refreshActivity(): void {
    const id = this.job()?.id;
    if (id) void this.loadActivities(id);
  }

  private validTab(tab: string | undefined): JobTab {
    // Keep existing bookmarks to the old Match tab working.
    if (tab === 'match') return 'cv';
    return this.tabs.find((t) => t.id === tab)?.id ?? 'details';
  }

  protected selectTab(tab: JobTab | 'match', notify = true): void {
    tab = this.validTab(tab);
    if (this.isNew() && tab !== 'details') return;
    this.activeTab.set(tab);
    this.visitedTabs.update((tabs) => new Set([...tabs, tab]));
    if (notify) {
      this.tabChange.emit(tab);
      this.navigation()?.nativeElement.scrollIntoView({ block: 'start' });
    }
  }

  protected tabKeydown(event: KeyboardEvent, tab: JobTab): void {
    const index = this.tabs.findIndex((t) => t.id === tab);
    const last = this.tabs.length - 1;
    const next =
      event.key === 'Home'
        ? 0
        : event.key === 'End'
          ? last
          : event.key === 'ArrowRight'
            ? (index + 1) % this.tabs.length
            : event.key === 'ArrowLeft'
              ? (index + last) % this.tabs.length
              : -1;
    if (next < 0) return;
    event.preventDefault();
    this.selectTab(this.tabs[next].id);
    const nav = (event.currentTarget as HTMLElement).parentElement;
    nav?.querySelectorAll<HTMLButtonElement>('[role="tab"]')[next]?.focus();
  }

  protected touch(): void {
    this.dirty.set(true);
  }

  private appliedDateCache: { iso: string; date: Date | null } = { iso: '', date: null };
  // Memoized so the picker keeps the same Date between change detection passes.
  protected appliedDate(iso: string): Date | null {
    if (this.appliedDateCache.iso !== iso) this.appliedDateCache = { iso, date: parseIsoDate(iso) };
    return this.appliedDateCache.date;
  }

  protected setAppliedOn(value: Date | Date[] | null): void {
    this.form.applied_on = formatIsoDate(value);
    this.touch();
  }

  protected async close(): Promise<void> {
    if (this.saving() || this.closing) return;
    this.closing = true;
    const generation = this.generation;
    try {
      if (
        this.dirty() &&
        !(await this.confirm.ask({
          title: this.isNew() ? 'Discard this job draft?' : 'Discard unsaved changes?',
          description: 'Your unsaved changes will be lost.',
          okText: 'Discard',
        }))
      )
        return;
      if (generation === this.generation) this.visible.set(false);
    } finally {
      this.closing = false;
    }
  }

  protected async captureVisibility(open: boolean, dialog: ZardDialogComponent): Promise<void> {
    if (open) return;
    await this.close();
    // Escape uses the dialog's internal model; restore it if closing was cancelled.
    dialog.zVisible.set(this.visible() && this.isNew());
  }

  protected captureKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      (event.target as HTMLElement).closest('form')?.requestSubmit();
    }
  }

  protected propertyErrors(): [string, string][] {
    return Object.entries(this.errors()).filter(
      ([key]) => !['title', 'company', 'url', 'description'].includes(key),
    );
  }

  protected hasPropertyErrors(): boolean {
    return Object.keys(this.errors()).some(
      (key) => !['title', 'company', 'url', 'description', 'form'].includes(key),
    );
  }

  protected async save(): Promise<void> {
    if (this.saving()) return;
    if (!this.form.title.trim()) {
      this.errors.set({ title: 'Enter a job title.' });
      return;
    }
    const generation = this.generation;
    this.saving.set(true);
    this.errors.set({});
    try {
      const input = toInput(this.form);
      if (this.isNew()) {
        const job = await this.store.create(input);
        if (generation !== this.generation) return;
        this.toast.success('Job saved');
        this.opened.emit(job.id);
      } else {
        const cur = this.job()!;
        const { job, etag } = await this.store.save(cur.id, this.etag, input);
        if (generation !== this.generation) return;
        this.job.set(job);
        this.etag = etag;
        this.form = toForm(job);
        this.dirty.set(false);
        this.toast.success('Saved');
      }
    } catch (e) {
      if (generation === this.generation) this.handleError(e);
    } finally {
      if (generation === this.generation) this.saving.set(false);
    }
  }

  /** Status changes from the header apply at once, like a drag on the board. */
  protected async changeStatus(statusId: string): Promise<void> {
    const generation = this.generation;
    if (this.isNew()) {
      this.touch();
      this.form.status_id = statusId;
      return;
    }
    const cur = this.job();
    if (!cur || statusId === cur.status_id) return;
    try {
      const job = await this.store.move(cur.id, statusId);
      if (generation !== this.generation) return;
      this.applyJob(job, `"${job.version}"`, true);
      await this.loadActivities(cur.id);
    } catch (e) {
      if (generation === this.generation) this.handleError(e);
    }
  }

  protected async archive(archived: boolean): Promise<void> {
    const generation = this.generation;
    const cur = this.job();
    if (!cur) return;
    try {
      const job = await this.store.setArchived(cur.id, archived);
      if (generation !== this.generation) return;
      this.applyJob(job, `"${job.version}"`, true);
      this.toast.success(archived ? 'Archived' : 'Restored');
    } catch (e) {
      if (generation === this.generation) this.handleError(e);
    }
  }

  protected async remove(): Promise<void> {
    const generation = this.generation;
    const cur = this.job();
    if (!cur) return;
    const ok = await this.confirm.ask({
      title: `Delete “${cur.title}”?`,
      description: 'Its activity goes too. Archive it instead to keep the history.',
    });
    if (!ok || generation !== this.generation) return;
    try {
      await this.store.delete(cur.id);
      if (generation !== this.generation) return;
      this.visible.set(false);
      this.toast.success('Job deleted');
    } catch (e) {
      if (generation === this.generation) this.handleError(e);
    }
  }

  protected async addNote(): Promise<void> {
    const generation = this.generation;
    const cur = this.job();
    const body = this.note.trim();
    if (!cur || !body) return;
    try {
      await this.store.addNote(cur.id, body);
      if (generation !== this.generation) return;
      this.note = '';
      await this.loadActivities(cur.id);
    } catch (e) {
      if (generation === this.generation) this.handleError(e);
    }
  }

  protected startEdit(a: JobActivity): void {
    this.editingNote.set(a.id);
    this.noteEdit = a.body;
  }

  protected async saveNote(a: JobActivity): Promise<void> {
    const generation = this.generation;
    const cur = this.job();
    if (!cur) return;
    try {
      await this.store.updateNote(cur.id, a.id, this.noteEdit.trim());
      if (generation !== this.generation) return;
      this.editingNote.set(null);
      await this.loadActivities(cur.id);
    } catch (e) {
      if (generation === this.generation) this.handleError(e);
    }
  }

  protected async deleteNote(a: JobActivity): Promise<void> {
    const generation = this.generation;
    const cur = this.job();
    if (!cur) return;
    if (
      !(await this.confirm.ask({
        title: 'Delete this note?',
        description: a.body.length > 120 ? a.body.slice(0, 120) + '…' : a.body,
      }))
    )
      return;
    if (generation !== this.generation) return;
    try {
      await this.store.deleteNote(cur.id, a.id);
      await this.loadActivities(cur.id);
    } catch (e) {
      if (generation === this.generation) this.handleError(e);
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
        this.saving.set(false);
        this.toast.warning('This job changed elsewhere', {
          description: 'Latest version loaded; your unsaved edits are kept for review.',
        });
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
  const num = (v: number | null) =>
    v === null || (v as unknown) === '' || Number.isNaN(+v) ? undefined : Math.round(+v);
  return {
    status_id: f.status_id || undefined,
    title: f.title.trim(),
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
