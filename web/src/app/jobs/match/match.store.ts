import { DestroyRef, inject, Injectable } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { QueryClient } from '@tanstack/angular-query';

import { Api, ApiError, type Schemas } from '../../core/api';
import { AuthService } from '../../core/auth.service';
import { EventsService } from '../../core/events.service';

export type MatchRun = Schemas['MatchRun'];
export type MatchEstimate = Schemas['MatchEstimate'];

/** A job's latest match run (null when it has none). */
export const matchRunKey = (identity: string | null, jobId?: string) =>
  jobId === undefined
    ? (['match-run', identity] as const)
    : (['match-run', identity, jobId] as const);
export const matchEstimateKey = (identity: string | null, jobId?: string) =>
  jobId === undefined
    ? (['match-estimate', identity] as const)
    : (['match-estimate', identity, jobId] as const);

/**
 * Match runs live in the TanStack Query cache (see MatchTab). This holds the
 * fetchers and writes, and invalidates the cached runs on live events.
 */
@Injectable({ providedIn: 'root' })
export class MatchStore {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  private readonly client = inject(QueryClient);

  constructor() {
    inject(EventsService)
      .changes.pipe(takeUntilDestroyed(inject(DestroyRef)))
      .subscribe((e) => {
        const identity = this.auth.identity();
        if (e.topic === 'match_run.changed' && e.data.job_id)
          void this.client.invalidateQueries({ queryKey: matchRunKey(identity, e.data.job_id) });
        if (['evidence.', 'skill.', 'organization.', 'job.'].some((p) => e.topic.startsWith(p)))
          void this.client.invalidateQueries({ queryKey: matchRunKey(identity) });
      });
  }

  async fetchRun(id: string): Promise<MatchRun | null> {
    const page = Api.ok(
      await this.api.GET('/v1/jobs/{id}/match-runs', {
        params: { path: { id }, query: { limit: 1 } },
      }),
    );
    if (!page.items.length) return null;
    return Api.ok(
      await this.api.GET('/v1/match-runs/{id}', { params: { path: { id: page.items[0].id } } }),
    );
  }

  async fetchEstimate(id: string): Promise<MatchEstimate> {
    return Api.ok(await this.api.GET('/v1/jobs/{id}/match-estimate', { params: { path: { id } } }));
  }

  /** `identity` is the member who acted, so a late answer never touches the next member's cache. */
  private refresh(identity: string | null, id: string, estimate = false): Promise<unknown> {
    return Promise.all([
      this.client.invalidateQueries({ queryKey: matchRunKey(identity, id) }),
      estimate && this.client.invalidateQueries({ queryKey: matchEstimateKey(identity, id) }),
    ]);
  }

  async start(id: string): Promise<void> {
    const identity = this.auth.identity();
    try {
      Api.ok(
        await this.api.POST('/v1/jobs/{id}/match-runs', {
          params: { path: { id }, header: { 'Idempotency-Key': crypto.randomUUID() } },
        }),
      );
    } catch (e) {
      if (!(
        e instanceof ApiError &&
        e.status === 409 &&
        (e.problem as { existing_id?: string })?.existing_id
      ))
        throw e;
    }
    await this.refresh(identity, id, true);
  }

  async cancel(id: string, run: string): Promise<void> {
    const identity = this.auth.identity();
    Api.ok(await this.api.POST('/v1/match-runs/{id}/cancel', { params: { path: { id: run } } }));
    await this.refresh(identity, id);
  }

  async correct(id: string, hash: string, value: string): Promise<void> {
    const identity = this.auth.identity();
    const params = { path: { id, text_hash: hash } };
    if (value === 'reset')
      Api.ok(await this.api.DELETE('/v1/jobs/{id}/match-overrides/{text_hash}', { params }));
    else
      Api.ok(
        await this.api.PUT('/v1/jobs/{id}/match-overrides/{text_hash}', {
          params,
          body: { class: value as 'must' | 'preferred' | 'duty' | 'trait' | 'ignore' },
        }),
      );
    await this.refresh(identity, id);
  }
}
