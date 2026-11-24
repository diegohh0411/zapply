import { computed, DestroyRef, effect, untracked, inject, Injectable, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { injectQuery, QueryClient } from '@tanstack/angular-query';

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
export type PostingRead = Schemas['PostingRead'];
export type PostingExtraction = Schemas['PostingExtraction'];

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

/** Query keys. Each carries the member's identity (see core/query.ts). */
export const jobsKey = (identity: string | null, includeArchived?: boolean) =>
  includeArchived === undefined
    ? (['jobs', identity] as const)
    : (['jobs', identity, { includeArchived }] as const);
/** One job in full, with the ETag a save must send back. Reusable by any page. */
export const jobKey = (identity: string | null, id: string) => ['job', identity, id] as const;
export const jobActivitiesKey = (identity: string | null, id: string) =>
  ['job-activities', identity, id] as const;
export const jobStatusesKey = (identity: string | null) => ['job-statuses', identity] as const;
export const jobViewsKey = (identity: string | null) => ['job-views', identity] as const;

export interface CachedJob {
  job: Job;
  etag: string;
}

/**
 * The user's board: statuses, every job summary and saved views, held in the
 * TanStack Query cache so the page shows them at once on return and refreshes
 * in the background. Live events invalidate the keys they touch. Lists filter
 * on the client: a person tracks hundreds of jobs, not millions.
 */
@Injectable({ providedIn: 'root' })
export class JobsStore {
  private readonly auth = inject(AuthService);
  private readonly api = inject(Api).client;
  private readonly client = inject(QueryClient);
  private readonly events = inject(EventsService);

  readonly includeArchived = signal(false);
  /** Nothing loads until a page asks for the board. */
  private readonly requested = signal(false);
  private readonly enabled = computed(() => this.requested() && !!this.auth.identity());

  private readonly statusesQuery = injectQuery(() => ({
    queryKey: jobStatusesKey(this.auth.identity()),
    queryFn: async () => Api.ok(await this.api.GET('/v1/job-statuses')),
    enabled: this.enabled(),
  }));
  private readonly viewsQuery = injectQuery(() => ({
    queryKey: jobViewsKey(this.auth.identity()),
    queryFn: async () => Api.ok(await this.api.GET('/v1/job-views')),
    enabled: this.enabled(),
  }));
  private readonly jobsQuery = injectQuery(() => {
    const identity = this.auth.identity();
    const includeArchived = this.includeArchived();
    return {
      queryKey: jobsKey(identity, includeArchived),
      queryFn: () => this.fetchJobs(includeArchived),
      enabled: this.enabled(),
      // Keep the board up while the archived toggle loads, but never show
      // another member's rows.
      placeholderData: (prev: JobSummary[] | undefined, q?: { queryKey: readonly unknown[] }) =>
        q?.queryKey[1] === identity ? prev : undefined,
    };
  });

  readonly statuses = computed(() => this.statusesQuery.data() ?? []);
  readonly views = computed(() => this.viewsQuery.data() ?? []);
  readonly jobs = computed(() => this.jobsQuery.data() ?? []);
  readonly statusById = computed(() => new Map(this.statuses().map((s) => [s.id, s])));
  readonly loading = computed(
    () =>
      this.enabled() &&
      (this.statusesQuery.isPending() || this.viewsQuery.isPending() || this.jobsQuery.isPending()),
  );
  /** Only when there is nothing cached to show; a failed background refresh keeps the board. */
  readonly error = computed(() => {
    for (const q of [this.statusesQuery, this.viewsQuery, this.jobsQuery] as const) {
      const e = q.error();
      if (e && q.data() === undefined) return e.message;
    }
    return null;
  });

  constructor() {
    let identity = this.auth.identity();
    effect(() => {
      const next = this.auth.identity();
      if (next !== identity) untracked(() => this.includeArchived.set(false));
      identity = next;
    });
    this.events.changes.pipe(takeUntilDestroyed(inject(DestroyRef))).subscribe((e) => {
      const id = this.auth.identity();
      const [kind] = e.topic.split('.');
      if (kind === 'match_run' || ['evidence', 'skill', 'organization'].includes(kind)) {
        void this.client.invalidateQueries({ queryKey: jobsKey(id) });
      } else if (kind === 'job') {
        if (e.topic === 'job.deleted') void this.forget(id, e.data.id);
        else {
          const cur = this.jobs().find((j) => j.id === e.data.id);
          if (!cur || (e.data.version ?? 0) > cur.version) {
            void this.client.invalidateQueries({ queryKey: jobsKey(id) });
            void this.client.invalidateQueries({ queryKey: jobKey(id, e.data.id) });
          }
        }
      } else if (kind === 'job_status')
        void this.client.invalidateQueries({ queryKey: jobStatusesKey(id) });
      else if (kind === 'job_view')
        void this.client.invalidateQueries({ queryKey: jobViewsKey(id) });
      else if (kind === 'job_activity' && e.data.job_id)
        void this.client.invalidateQueries({ queryKey: jobActivitiesKey(id, e.data.job_id) });
    });
  }

  /** Starts loading the board; cached data shows at once and refreshes in the background. */
  ensureLoaded(): void {
    this.requested.set(true);
  }

  setIncludeArchived(v: boolean): void {
    this.includeArchived.set(v);
  }

  /** Reject old-account responses before any caller can apply them. */
  private async inSession<T>(request: () => Promise<T>): Promise<T> {
    const identity = this.auth.identity();
    const result = await request();
    if (identity !== this.auth.identity()) throw new Error('Sign-in changed; request discarded');
    return result;
  }

  private async fetchJobs(includeArchived: boolean): Promise<JobSummary[]> {
    const rows: JobSummary[] = [];
    let cursor: string | undefined;
    do {
      const page = Api.ok(
        await this.api.GET('/v1/jobs', {
          params: {
            query: { sort: 'position', limit: 200, cursor, include_archived: includeArchived },
          },
        }),
      );
      rows.push(...page.items);
      cursor = page.next_cursor;
    } while (cursor);
    return rows;
  }

  /**
   * Edits every cached job list of this member in place. In-flight list
   * fetches are canceled first so an older snapshot can't land on top.
   */
  private async editLists(
    identity: string | null,
    edit: (rows: JobSummary[], includeArchived: boolean) => JobSummary[],
  ): Promise<void> {
    await this.client.cancelQueries({ queryKey: jobsKey(identity) });
    for (const [key, rows] of this.client.getQueriesData<JobSummary[]>({
      queryKey: jobsKey(identity),
    })) {
      const includeArchived = !!(key[2] as { includeArchived?: boolean } | undefined)
        ?.includeArchived;
      if (rows) this.client.setQueryData(key, edit(rows, includeArchived));
      else void this.client.invalidateQueries({ queryKey: key, exact: true });
    }
  }

  /** Puts a saved job in every cached list (unless a newer copy is there). */
  private async upsert(identity: string | null, j: JobSummary | Job, etag?: string): Promise<void> {
    const s = summary(j);
    await this.editLists(identity, (rows, includeArchived) => {
      const i = rows.findIndex((r) => r.id === s.id);
      if (i >= 0 && rows[i].version > s.version) return rows;
      const rest = rows.filter((r) => r.id !== s.id);
      return s.archived && !includeArchived ? rest : [...rest, s];
    });
    const key = jobKey(identity, j.id);
    if (etag && 'description' in j) this.client.setQueryData<CachedJob>(key, { job: j, etag });
    else void this.client.invalidateQueries({ queryKey: key });
  }

  private async forget(identity: string | null, id: string): Promise<void> {
    await this.editLists(identity, (rows) => rows.filter((r) => r.id !== id));
    this.client.removeQueries({ queryKey: jobKey(identity, id) });
    this.client.removeQueries({ queryKey: jobActivitiesKey(identity, id) });
  }

  // Jobs

  /** The cached copy of a job, if any page loaded it. */
  cached(id: string): CachedJob | undefined {
    return this.client.getQueryData<CachedJob>(jobKey(this.auth.identity(), id));
  }

  /** Fetches a job fresh and caches it under its per-job key. */
  async get(id: string): Promise<CachedJob> {
    const identity = this.auth.identity();
    return this.inSession(() =>
      this.client.fetchQuery({
        queryKey: jobKey(identity, id),
        queryFn: async () => {
          const r = await this.api.GET('/v1/jobs/{id}', { params: { path: { id } } });
          return { job: Api.ok(r), etag: etagOf(r) };
        },
        staleTime: 0,
      }),
    );
  }

  /**
   * Reads a posting's page through the server's scraper, and starts an
   * extraction of its text (or the pasted text) when one is configured.
   * Nothing is saved.
   */
  async readPosting(url: string, text: string): Promise<PostingRead> {
    const body = text.trim() ? { url, text } : { url };
    return Api.ok(await this.inSession(() => this.api.POST('/v1/postings/read', { body })));
  }

  /** What the extraction model read from a posting so far. */
  async postingExtraction(id: string): Promise<PostingExtraction> {
    return Api.ok(
      await this.inSession(() =>
        this.api.GET('/v1/postings/extractions/{id}', { params: { path: { id } } }),
      ),
    );
  }

  async create(input: JobInput): Promise<Job> {
    const identity = this.auth.identity();
    const r = await this.inSession(() => this.api.POST('/v1/jobs', { body: input }));
    const job = Api.ok(r);
    await this.upsert(identity, job, etagOf(r));
    return job;
  }

  async save(id: string, etag: string, input: JobInput): Promise<CachedJob> {
    const identity = this.auth.identity();
    const r = await this.inSession(() =>
      this.api.PUT('/v1/jobs/{id}', {
        params: { path: { id }, header: { 'If-Match': etag } },
        body: input,
      }),
    );
    const job = Api.ok(r);
    await this.upsert(identity, job, etagOf(r));
    return { job, etag: etagOf(r) };
  }

  /**
   * Drag and drop: shows the move at once, then applies the server's
   * position (or refetches if it failed).
   */
  async move(id: string, statusId: string, after?: string, before?: string): Promise<Job> {
    const identity = this.auth.identity();
    const position = guessPosition(this.column(statusId, id), after, before);
    await this.editLists(identity, (rows) =>
      rows.map((r) => (r.id === id ? { ...r, status_id: statusId, position } : r)),
    );
    try {
      const job = Api.ok(
        await this.inSession(() =>
          this.api.POST('/v1/jobs/{id}/move', {
            params: { path: { id } },
            body: { status_id: statusId, after_id: after, before_id: before },
          }),
        ),
      );
      await this.upsert(identity, job);
      return job;
    } catch (e) {
      void this.client.invalidateQueries({ queryKey: jobsKey(identity) });
      throw e;
    }
  }

  async setArchived(id: string, archived: boolean): Promise<Job> {
    const identity = this.auth.identity();
    const path = archived ? '/v1/jobs/{id}/archive' : '/v1/jobs/{id}/unarchive';
    const job = Api.ok(
      await this.inSession(() => this.api.POST(path, { params: { path: { id } } })),
    );
    await this.upsert(identity, job);
    return job;
  }

  async delete(id: string): Promise<void> {
    const identity = this.auth.identity();
    Api.ok(
      await this.inSession(() => this.api.DELETE('/v1/jobs/{id}', { params: { path: { id } } })),
    );
    await this.forget(identity, id);
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

  private refresh(...keys: (readonly unknown[])[]): Promise<unknown> {
    return Promise.all(keys.map((queryKey) => this.client.invalidateQueries({ queryKey })));
  }

  async createStatus(body: Schemas['JobStatusInput']): Promise<JobStatus> {
    const identity = this.auth.identity();
    const created = Api.ok(await this.inSession(() => this.api.POST('/v1/job-statuses', { body })));
    await this.refresh(jobStatusesKey(identity));
    return created;
  }

  async updateStatus(id: string, body: Schemas['JobStatusInput']): Promise<void> {
    const identity = this.auth.identity();
    Api.ok(
      await this.inSession(() =>
        this.api.PATCH('/v1/job-statuses/{id}', { params: { path: { id } }, body }),
      ),
    );
    await this.refresh(jobStatusesKey(identity));
  }

  async moveStatus(id: string, after?: string, before?: string): Promise<void> {
    const identity = this.auth.identity();
    Api.ok(
      await this.inSession(() =>
        this.api.POST('/v1/job-statuses/{id}/move', {
          params: { path: { id } },
          body: { after_id: after, before_id: before },
        }),
      ),
    );
    await this.refresh(jobStatusesKey(identity));
  }

  async deleteStatus(id: string, moveTo?: string): Promise<void> {
    const identity = this.auth.identity();
    Api.ok(
      await this.inSession(() =>
        this.api.DELETE('/v1/job-statuses/{id}', {
          params: { path: { id }, query: { move_to: moveTo } },
        }),
      ),
    );
    await this.refresh(jobStatusesKey(identity), jobsKey(identity));
  }

  // Views

  async createView(body: JobViewInput): Promise<JobView> {
    const identity = this.auth.identity();
    const v = Api.ok(await this.inSession(() => this.api.POST('/v1/job-views', { body })));
    await this.refresh(jobViewsKey(identity));
    return v;
  }

  async updateView(id: string, body: JobViewInput): Promise<void> {
    const identity = this.auth.identity();
    Api.ok(
      await this.inSession(() =>
        this.api.PUT('/v1/job-views/{id}', { params: { path: { id } }, body }),
      ),
    );
    await this.refresh(jobViewsKey(identity));
  }

  async deleteView(id: string): Promise<void> {
    const identity = this.auth.identity();
    Api.ok(
      await this.inSession(() =>
        this.api.DELETE('/v1/job-views/{id}', { params: { path: { id } } }),
      ),
    );
    await this.refresh(jobViewsKey(identity));
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
