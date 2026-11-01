import { computed, DestroyRef, effect, untracked, inject, Injectable, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Subject } from 'rxjs';

import { Api, etagOf, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
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
export const byPosition = (
  a: { position: string; id: string },
  b: { position: string; id: string },
) =>
  a.position < b.position
    ? -1
    : a.position > b.position
      ? 1
      : a.id < b.id
        ? -1
        : a.id > b.id
          ? 1
          : 0;

/**
 * The user's board: statuses, every job summary and saved views, loaded
 * once and kept current from /v1/events. Lists filter on the client: a
 * person tracks hundreds of jobs, not millions.
 */
@Injectable({ providedIn: 'root' })
export class JobsStore {
  private readonly auth = inject(AuthService);
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
  private requested = false;
  private identity: string | null | undefined;
  private sessionGeneration = 0;
  private jobsRequest = 0;
  private statusesRequest = 0;
  private viewsRequest = 0;
  private revision = 0;
  private readonly changed = new Map<string, number>();
  private readonly versions = new Map<string, number>();
  private readonly deleted = new Set<string>();

  private syncSession(): void {
    const identity = this.auth.identity();
    if (identity === this.identity) return;
    this.identity = identity;
    this.sessionGeneration++;
    this.jobsRequest++;
    this.statusesRequest++;
    this.viewsRequest++;
    this.loaded = false;
    this.changed.clear();
    this.versions.clear();
    this.deleted.clear();
    this.jobMap.set(new Map());
    this.statuses.set([]);
    this.views.set([]);
    this.includeArchived.set(false);
    this.error.set(null);
    this.loading.set(false);
  }

  /** Reject old-account responses before any caller can apply them. */
  private async inSession<T>(request: () => Promise<T>): Promise<T> {
    this.syncSession();
    const generation = this.sessionGeneration;
    const result = await request();
    this.syncSession();
    if (generation !== this.sessionGeneration)
      throw new Error('Sign-in changed; request discarded');
    return result;
  }

  constructor() {
    this.syncSession();
    effect(() => {
      this.auth.identity();
      untracked(() => {
        this.syncSession();
        if (this.identity && this.requested && !this.loaded) void this.load();
      });
    });
    const destroy = inject(DestroyRef);
    this.events.resync.pipe(takeUntilDestroyed(destroy)).subscribe(() => {
      if (this.loaded) void this.load();
    });
    this.events.changes.pipe(takeUntilDestroyed(destroy)).subscribe((e) => {
      if (!this.loaded) return;
      const [kind] = e.topic.split('.');
      if (kind === 'match_run' || ['evidence','skill','organization'].includes(kind)) { void this.loadJobs(); }
      else if (kind === 'job') {
        if (e.topic === 'job.deleted') {
          this.deleted.add(e.data.id);
          this.remove(e.data.id);
        } else {
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
    this.requested = true;
    this.syncSession();
    if (!this.loaded) await this.load();
  }

  async load(): Promise<void> {
    this.syncSession();
    const generation = this.sessionGeneration;
    this.loaded = true;
    this.loading.set(true);
    this.error.set(null);
    try {
      await Promise.all([this.loadStatuses(), this.loadViews(), this.loadJobs()]);
    } catch (e) {
      if (generation === this.sessionGeneration) this.error.set((e as Error).message);
    } finally {
      if (generation === this.sessionGeneration) this.loading.set(false);
    }
  }

  async setIncludeArchived(v: boolean): Promise<void> {
    if (v === this.includeArchived()) return;
    this.includeArchived.set(v);
    await this.loadJobs();
  }

  private async loadStatuses(): Promise<void> {
    const seq = ++this.statusesRequest;
    const rows = Api.ok(await this.inSession(() => this.api.GET('/v1/job-statuses')));
    if (seq === this.statusesRequest) this.statuses.set(rows);
  }

  private async loadViews(): Promise<void> {
    const seq = ++this.viewsRequest;
    const rows = Api.ok(await this.inSession(() => this.api.GET('/v1/job-views')));
    if (seq === this.viewsRequest) this.views.set(rows);
  }

  private async loadJobs(): Promise<void> {
    const seq = ++this.jobsRequest;
    const revision = this.revision;
    const includeArchived = this.includeArchived();
    const map = new Map<string, JobSummary>();
    let cursor: string | undefined;
    do {
      const page = Api.ok(
        await this.inSession(() =>
          this.api.GET('/v1/jobs', {
            params: {
              query: { sort: 'position', limit: 200, cursor, include_archived: includeArchived },
            },
          }),
        ),
      );
      for (const j of page.items) map.set(j.id, j);
      cursor = page.next_cursor;
    } while (cursor);
    if (seq !== this.jobsRequest) return;
    // Preserve changes (including removals) received while paging the snapshot.
    for (const [id, changedAt] of this.changed) {
      if (
        changedAt > revision ||
        (map.has(id) && map.get(id)!.version < (this.versions.get(id) ?? 0))
      ) {
        const current = this.jobMap().get(id);
        if (current && (includeArchived || !current.archived)) map.set(id, current);
        else map.delete(id);
      }
    }
    for (const id of this.deleted) map.delete(id);
    for (const j of map.values()) this.versions.set(j.id, j.version);
    this.jobMap.set(map);
  }

  private async refreshJob(id: string): Promise<void> {
    const r = await this.inSession(() =>
      this.api.GET('/v1/jobs/{id}', { params: { path: { id } } }),
    );
    if (r.response.status === 404) return this.remove(id);
    if (r.data) this.upsert(r.data);
  }

  upsert(j: JobSummary): void {
    if (this.deleted.has(j.id) || (this.versions.get(j.id) ?? 0) > j.version) return;
    this.versions.set(j.id, j.version);
    this.changed.set(j.id, ++this.revision);
    if (j.archived && !this.includeArchived()) return this.remove(j.id);
    const m = new Map(this.jobMap());
    m.set(j.id, summary(j));
    this.jobMap.set(m);
  }

  private remove(id: string): void {
    this.changed.set(id, ++this.revision);
    if (!this.jobMap().has(id)) return;
    const m = new Map(this.jobMap());
    m.delete(id);
    this.jobMap.set(m);
  }

  // Jobs

  async get(id: string): Promise<{ job: Job; etag: string }> {
    const r = await this.inSession(() =>
      this.api.GET('/v1/jobs/{id}', { params: { path: { id } } }),
    );
    return { job: Api.ok(r), etag: etagOf(r) };
  }

  async create(input: JobInput): Promise<Job> {
    const job = Api.ok(await this.inSession(() => this.api.POST('/v1/jobs', { body: input })));
    this.upsert(job);
    return job;
  }

  async save(id: string, etag: string, input: JobInput): Promise<{ job: Job; etag: string }> {
    const r = await this.inSession(() =>
      this.api.PUT('/v1/jobs/{id}', {
        params: { path: { id }, header: { 'If-Match': etag } },
        body: input,
      }),
    );
    const job = Api.ok(r);
    this.upsert(job);
    return { job, etag: etagOf(r) };
  }

  /**
   * Drag and drop: shows the move at once, then applies the server's
   * position (or reloads if it failed).
   */
  async move(id: string, statusId: string, after?: string, before?: string): Promise<Job> {
    this.syncSession();
    const generation = this.sessionGeneration;
    const cur = this.jobMap().get(id);
    if (cur) {
      const optimistic = {
        ...cur,
        status_id: statusId,
        position: guessPosition(this.column(statusId, id), after, before),
      };
      const m = new Map(this.jobMap());
      m.set(id, optimistic);
      this.jobMap.set(m);
      this.changed.set(id, ++this.revision);
    }
    try {
      const job = Api.ok(
        await this.inSession(() =>
          this.api.POST('/v1/jobs/{id}/move', {
            params: { path: { id } },
            body: { status_id: statusId, after_id: after, before_id: before },
          }),
        ),
      );
      this.upsert(job);
      return job;
    } catch (e) {
      if (generation === this.sessionGeneration) await this.loadJobs();
      throw e;
    }
  }

  async setArchived(id: string, archived: boolean): Promise<Job> {
    const path = archived ? '/v1/jobs/{id}/archive' : '/v1/jobs/{id}/unarchive';
    const job = Api.ok(
      await this.inSession(() => this.api.POST(path, { params: { path: { id } } })),
    );
    this.upsert(job);
    return job;
  }

  async delete(id: string): Promise<void> {
    Api.ok(
      await this.inSession(() => this.api.DELETE('/v1/jobs/{id}', { params: { path: { id } } })),
    );
    this.deleted.add(id);
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
    return Api.ok(
      await this.inSession(() =>
        this.api.GET('/v1/jobs/{id}/activities', { params: { path: { id: jobId } } }),
      ),
    );
  }

  async addNote(jobId: string, body: string, occurredAt?: string): Promise<void> {
    Api.ok(
      await this.inSession(() =>
        this.api.POST('/v1/jobs/{id}/activities', {
          params: { path: { id: jobId } },
          body: { body, occurred_at: occurredAt },
        }),
      ),
    );
  }

  async updateNote(jobId: string, id: string, body: string): Promise<void> {
    Api.ok(
      await this.inSession(() =>
        this.api.PATCH('/v1/jobs/{id}/activities/{activity_id}', {
          params: { path: { id: jobId, activity_id: id } },
          body: { body },
        }),
      ),
    );
  }

  async deleteNote(jobId: string, id: string): Promise<void> {
    Api.ok(
      await this.inSession(() =>
        this.api.DELETE('/v1/jobs/{id}/activities/{activity_id}', {
          params: { path: { id: jobId, activity_id: id } },
        }),
      ),
    );
  }

  // Statuses

  async createStatus(body: Schemas['JobStatusInput']): Promise<JobStatus> {
    const created = Api.ok(await this.inSession(() => this.api.POST('/v1/job-statuses', { body })));
    await this.loadStatuses();
    return created;
  }

  async updateStatus(id: string, body: Schemas['JobStatusInput']): Promise<void> {
    Api.ok(
      await this.inSession(() =>
        this.api.PATCH('/v1/job-statuses/{id}', { params: { path: { id } }, body }),
      ),
    );
    await this.loadStatuses();
  }

  async moveStatus(id: string, after?: string, before?: string): Promise<void> {
    Api.ok(
      await this.inSession(() =>
        this.api.POST('/v1/job-statuses/{id}/move', {
          params: { path: { id } },
          body: { after_id: after, before_id: before },
        }),
      ),
    );
    await this.loadStatuses();
  }

  async deleteStatus(id: string, moveTo?: string): Promise<void> {
    Api.ok(
      await this.inSession(() =>
        this.api.DELETE('/v1/job-statuses/{id}', {
          params: { path: { id }, query: { move_to: moveTo } },
        }),
      ),
    );
    await Promise.all([this.loadStatuses(), this.loadJobs()]);
  }

  // Views

  async createView(body: JobViewInput): Promise<JobView> {
    const v = Api.ok(await this.inSession(() => this.api.POST('/v1/job-views', { body })));
    await this.loadViews();
    return v;
  }

  async updateView(id: string, body: JobViewInput): Promise<void> {
    Api.ok(
      await this.inSession(() =>
        this.api.PUT('/v1/job-views/{id}', { params: { path: { id } }, body }),
      ),
    );
    await this.loadViews();
  }

  async deleteView(id: string): Promise<void> {
    Api.ok(
      await this.inSession(() =>
        this.api.DELETE('/v1/job-views/{id}', { params: { path: { id } } }),
      ),
    );
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
  const i = after
    ? col.findIndex((j) => j.id === after)
    : before
      ? col.findIndex((j) => j.id === before) - 1
      : -1;
  return (i >= 0 ? col[i].position : '') + '\u0000';
}
