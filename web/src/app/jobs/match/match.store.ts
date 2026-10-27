import { effect, inject, Injectable, signal, untracked, DestroyRef } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Api, ApiError, type Schemas } from '../../core/api';
import { AuthService } from '../../core/auth.service';
import { EventsService } from '../../core/events.service';
export type MatchRun = Schemas['MatchRun'];
@Injectable({ providedIn: 'root' })
export class MatchStore {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  readonly runs = signal(new Map<string, MatchRun>());
  readonly estimates = signal(new Map<string, Schemas['MatchEstimate']>());
  readonly errors = signal(new Map<string, string>());
  private identity = this.auth.identity();
  private generation = 0;
  private requests = new Map<string, number>();
  private watched = new Set<string>();
  constructor() {
    effect(() => { this.auth.identity(); untracked(() => this.sync()); });
    const destroy = inject(DestroyRef);
    this.events.changes.pipe(takeUntilDestroyed(destroy)).subscribe(e => {
      this.sync();
      if (e.topic === 'match_run.changed' && e.data.job_id && this.watched.has(e.data.job_id)) void this.load(e.data.job_id);
      if (['evidence.', 'skill.', 'organization.', 'job.'].some(p => e.topic.startsWith(p)))
        for (const id of this.watched) void this.load(id);
    });
    this.events.resync.pipe(takeUntilDestroyed(destroy)).subscribe(() => { for (const id of this.watched) void this.load(id); });
  }
  private sync(): void {
    const identity = this.auth.identity();
    if (identity === this.identity) return;
    this.identity = identity; this.generation++; this.requests.clear(); this.watched.clear();
    this.runs.set(new Map()); this.estimates.set(new Map()); this.errors.set(new Map());
  }
  watch(id: string): void { this.sync(); this.watched.add(id); void this.load(id); void this.estimate(id); }
  unwatch(id: string): void { this.watched.delete(id); }
  async load(id: string): Promise<void> {
    this.sync(); const generation = this.generation, seq = (this.requests.get(id) ?? 0) + 1;
    this.requests.set(id, seq);
    const current = () => { this.sync(); return generation === this.generation && seq === this.requests.get(id); };
    try {
      const page = Api.ok(await this.api.GET('/v1/jobs/{id}/match-runs', { params: { path: { id }, query: { limit: 1 } } }));
      if (!current()) return;
      if (!page.items.length) { this.runs.update(m => { const n = new Map(m); n.delete(id); return n; }); return; }
      const run = Api.ok(await this.api.GET('/v1/match-runs/{id}', { params: { path: { id: page.items[0].id } } }));
      if (!current()) return;
      this.runs.update(m => new Map(m).set(id, run)); this.errors.update(m => { const n = new Map(m); n.delete(id); return n; });
    } catch (e) { if (current()) this.errors.update(m => new Map(m).set(id, (e as Error).message)); }
  }
  async estimate(id: string): Promise<void> {
    this.sync(); const generation = this.generation;
    try { const value = Api.ok(await this.api.GET('/v1/jobs/{id}/match-estimate', { params: { path: { id } } })); this.sync();
      if (generation === this.generation) this.estimates.update(m => new Map(m).set(id, value));
    } catch { /* Run starts remain available when estimates are unavailable. */ }
  }
  async start(id: string): Promise<void> {
    this.sync(); const generation = this.generation;
    try { Api.ok(await this.api.POST('/v1/jobs/{id}/match-runs', { params: { path: { id }, header: { 'Idempotency-Key': crypto.randomUUID() } } })); }
    catch(e) { if (!(e instanceof ApiError && e.status === 409 && (e.problem as {existing_id?: string})?.existing_id)) throw e; }
    this.sync(); if (generation === this.generation) { await this.load(id); await this.estimate(id); }
  }
  async cancel(id: string, run: string): Promise<void> {
    this.sync(); const generation = this.generation;
    Api.ok(await this.api.POST('/v1/match-runs/{id}/cancel', { params: { path: { id: run } } }));
    this.sync(); if (generation === this.generation) await this.load(id);
  }
  async correct(id: string, hash: string, value: string): Promise<void> {
    this.sync(); const generation = this.generation;
    const params = { path: { id, text_hash: hash } };
    if (value === 'reset') Api.ok(await this.api.DELETE('/v1/jobs/{id}/match-overrides/{text_hash}', { params }));
    else Api.ok(await this.api.PUT('/v1/jobs/{id}/match-overrides/{text_hash}', { params, body: { class: value as 'must' | 'preferred' | 'duty' | 'ignore' } }));
    this.sync(); if (generation === this.generation) await this.load(id);
  }
}
