import { computed, DestroyRef, inject, Injectable, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Subject } from 'rxjs';

import { Api, etagOf, type Schemas } from '../core/api';
import { EventsService } from '../core/events.service';

export type JobStatus = Schemas['JobStatus'];
export type JobSummary = Schemas['JobSummary'];
export type Job = Schemas['Job'];
export type JobInput = Schemas['JobInput'];
export type JobView = Schemas['JobView'];
export type JobViewInput = Schemas['JobViewInput'];
export type JobFilters = Schemas['JobViewFilters'];
export type JobStage = Schemas['JobStage'];
export type JobActivity = Schemas['JobActivity'];
export type JobSort = Schemas['JobSort'];

export const STAGES: { value: JobStage; label: string }[] = [
  { value: 'saved', label: 'Saved' },
  { value: 'active', label: 'In process' },
  { value: 'offer', label: 'Offer' },
  { value: 'closed', label: 'Closed' },
];

/** Positions are base-62 keys compared bytewise, like COLLATE "C". */
export const byPosition = (a: { position: string; id: string }, b: { position: string; id: string }) =>
  a.position < b.position ? -1 : a.position > b.position ? 1 : a.id < b.id ? -1 : a.id > b.id ? 1 : 0;

/**
 * The user's board: statuses, every job summary and saved views, loaded
 * once and kept current from /v1/events. Lists filter on the client: a
 * person tracks hundreds of jobs, not millions.
 */
@Injectable({ providedIn: 'root' })
export class JobsStore {
  private readonly api = inject(Api).client;
  private readonly events = inject(EventsService);

  readonly statuses = signal<JobStatus[]>([]);
  readonly views = signal<JobView[]>([]);
  readonly includeArchived = signal(false);
  readonly loading = signal(true);
  readonly error = signal<string | null>(null);
  private readonly jobMap = signal(new Map<string, JobSummary>());

  /** Fires with a job id when its timeline changes. */
  readonly activityChanged = new Subject<string>();

  readonly jobs = computed(() => [...this.jobMap().values()]);
  readonly statusById = computed(() => new Map(this.statuses().map((s) => [s.id, s])));

  private loaded = false;

  constructor() {
    const destroy = inject(DestroyRef);
    this.events.resync.pipe(takeUntilDestroyed(destroy)).subscribe(() => {
      if (this.loaded) void this.load();
    });
    this.events.changes.pipe(takeUntilDestroyed(destroy)).subscribe((e) => {
      if (!this.loaded) return;
      const [kind] = e.topic.split('.');
      if (kind === 'job') {
        if (e.topic === 'job.deleted') this.remove(e.data.id);
        else {
          const cur = this.jobMap().get(e.data.id);
          if (!cur || (e.data.version ?? 0) > cur.version) void this.refreshJob(e.data.id);
        }
      } else if (kind === 'job_status') void this.loadStatuses();
      else if (kind === 'job_view') void this.loadViews();
      else if (kind === 'job_activity' && e.data.job_id) this.activityChanged.next(e.data.job_id);
    });
  }

  /** Loads everything the first time; later calls reload. */
  async ensureLoaded(): Promise<void> {
    if (!this.loaded) await this.load();
  }

  async load(): Promise<void> {
    this.loaded = true;
    this.loading.set(true);
    this.error.set(null);
    try {
      await Promise.all([this.loadStatuses(), this.loadViews(), this.loadJobs()]);
    } catch (e) {
      this.error.set((e as Error).message);
    } finally {
      this.loading.set(false);
    }
  }

  async setIncludeArchived(v: boolean): Promise<void> {
    if (v === this.includeArchived()) return;
    this.includeArchived.set(v);
    await this.loadJobs();
  }

  private async loadStatuses(): Promise<void> {
    this.statuses.set(Api.ok(await this.api.GET('/v1/job-statuses')));
  }

  private async loadViews(): Promise<void> {
    this.views.set(Api.ok(await this.api.GET('/v1/job-views')));
  }

  private async loadJobs(): Promise<void> {
    const map = new Map<string, JobSummary>();
    let cursor: string | undefined;
    do {
      const page = Api.ok(
        await this.api.GET('/v1/jobs', {
          params: { query: { sort: 'position', limit: 200, cursor, include_archived: this.includeArchived() } },
        }),
      );
      for (const j of page.items) map.set(j.id, j);
      cursor = page.next_cursor;
    } while (cursor);
    this.jobMap.set(map);
  }

  private async refreshJob(id: string): Promise<void> {
    const r = await this.api.GET('/v1/jobs/{id}', { params: { path: { id } } });
    if (r.response.status === 404) return this.remove(id);
    if (r.data) this.upsert(r.data);
  }

  upsert(j: JobSummary): void {
    if (j.archived && !this.includeArchived()) return this.remove(j.id);
    const cur = this.jobMap().get(j.id);
    if (cur && cur.version > j.version) return;
    const m = new Map(this.jobMap());
    m.set(j.id, summary(j));
    this.jobMap.set(m);
  }

  private remove(id: string): void {
    if (!this.jobMap().has(id)) return;
    const m = new Map(this.jobMap());
    m.delete(id);
    this.jobMap.set(m);
  }

  // Jobs

  async get(id: string): Promise<{ job: Job; etag: string }> {
    const r = await this.api.GET('/v1/jobs/{id}', { params: { path: { id } } });
    return { job: Api.ok(r), etag: etagOf(r) };
  }

  async create(input: JobInput): Promise<Job> {
    const job = Api.ok(await this.api.POST('/v1/jobs', { body: input }));
    this.upsert(job);
    return job;
  }

  async save(id: string, etag: string, input: JobInput): Promise<{ job: Job; etag: string }> {
    const r = await this.api.PUT('/v1/jobs/{id}', { params: { path: { id }, header: { 'If-Match': etag } }, body: input });
    const job = Api.ok(r);
    this.upsert(job);
    return { job, etag: etagOf(r) };
  }

  /**
   * Drag and drop: shows the move at once, then applies the server's
   * position (or reloads if it failed).
   */
  async move(id: string, statusId: string, after?: string, before?: string): Promise<void> {
    const cur = this.jobMap().get(id);
    if (cur) {
      const optimistic = { ...cur, status_id: statusId, position: guessPosition(this.column(statusId, id), after, before) };
      const m = new Map(this.jobMap());
      m.set(id, optimistic);
      this.jobMap.set(m);
    }
    try {
      const job = Api.ok(
        await this.api.POST('/v1/jobs/{id}/move', {
          params: { path: { id } },
          body: { status_id: statusId, after_id: after, before_id: before },
        }),
      );
      const m = new Map(this.jobMap());
      m.set(id, summary(job));
      this.jobMap.set(m);
    } catch (e) {
      await this.loadJobs();
      throw e;
    }
  }

  async setArchived(id: string, archived: boolean): Promise<Job> {
    const path = archived ? '/v1/jobs/{id}/archive' : '/v1/jobs/{id}/unarchive';
    const job = Api.ok(await this.api.POST(path, { params: { path: { id } } }));
    this.upsert(job);
    return job;
  }

  async delete(id: string): Promise<void> {
    Api.ok(await this.api.DELETE('/v1/jobs/{id}', { params: { path: { id } } }));
    this.remove(id);
  }

  /** A status's jobs in board order, optionally without one job. */
  column(statusId: string, except?: string): JobSummary[] {
    return this.jobs()
      .filter((j) => j.status_id === statusId && j.id !== except)
      .sort(byPosition);
  }

  // Activity

  async activities(jobId: string): Promise<JobActivity[]> {
    return Api.ok(await this.api.GET('/v1/jobs/{id}/activities', { params: { path: { id: jobId } } }));
  }

  async addNote(jobId: string, body: string, occurredAt?: string): Promise<void> {
    Api.ok(
      await this.api.POST('/v1/jobs/{id}/activities', {
        params: { path: { id: jobId } },
        body: { body, occurred_at: occurredAt },
      }),
    );
  }

  async updateNote(jobId: string, id: string, body: string): Promise<void> {
    Api.ok(
      await this.api.PATCH('/v1/jobs/{id}/activities/{activity_id}', {
        params: { path: { id: jobId, activity_id: id } },
        body: { body },
      }),
    );
  }

  async deleteNote(jobId: string, id: string): Promise<void> {
    Api.ok(await this.api.DELETE('/v1/jobs/{id}/activities/{activity_id}', { params: { path: { id: jobId, activity_id: id } } }));
  }

  // Statuses

  async createStatus(body: Schemas['JobStatusInput']): Promise<void> {
    Api.ok(await this.api.POST('/v1/job-statuses', { body }));
    await this.loadStatuses();
  }

  async updateStatus(id: string, body: Schemas['JobStatusInput']): Promise<void> {
    Api.ok(await this.api.PATCH('/v1/job-statuses/{id}', { params: { path: { id } }, body }));
    await this.loadStatuses();
  }

  async moveStatus(id: string, after?: string, before?: string): Promise<void> {
    Api.ok(await this.api.POST('/v1/job-statuses/{id}/move', { params: { path: { id } }, body: { after_id: after, before_id: before } }));
    await this.loadStatuses();
  }

  async deleteStatus(id: string, moveTo?: string): Promise<void> {
    Api.ok(await this.api.DELETE('/v1/job-statuses/{id}', { params: { path: { id }, query: { move_to: moveTo } } }));
    await Promise.all([this.loadStatuses(), this.loadJobs()]);
  }

  // Views

  async createView(body: JobViewInput): Promise<JobView> {
    const v = Api.ok(await this.api.POST('/v1/job-views', { body }));
    await this.loadViews();
    return v;
  }

  async updateView(id: string, body: JobViewInput): Promise<void> {
    Api.ok(await this.api.PUT('/v1/job-views/{id}', { params: { path: { id } }, body }));
    await this.loadViews();
  }

  async deleteView(id: string): Promise<void> {
    Api.ok(await this.api.DELETE('/v1/job-views/{id}', { params: { path: { id } } }));
    await this.loadViews();
  }
}

function summary(j: JobSummary | Job): JobSummary {
  const { description: _d, notes: _n, ...rest } = j as Job;
  return rest;
}

/**
 * A local key that sorts right after the drop target's predecessor, until
 * the server answers with a real one. For lo < hi, lo + "\0" is always
 * strictly between them (base-62 keys never contain NUL).
 */
function guessPosition(col: JobSummary[], after?: string, before?: string): string {
  const i = after ? col.findIndex((j) => j.id === after) : before ? col.findIndex((j) => j.id === before) - 1 : -1;
  return (i >= 0 ? col[i].position : '') + '\u0000';
}
