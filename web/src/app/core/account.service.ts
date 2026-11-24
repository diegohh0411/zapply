import { computed, DestroyRef, inject, Injectable } from '@angular/core';
import { injectQuery, QueryClient } from '@tanstack/angular-query';

import { Api, type Schemas } from './api';
import { AuthService } from './auth.service';
import { EventsService } from './events.service';

const meKey = (identity: string | null) => ['me', identity] as const;

/** The signed-in member's account, cached per member so every reader shares one request. */
@Injectable({ providedIn: 'root' })
export class AccountService {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  private readonly client = inject(QueryClient);

  private readonly query = injectQuery(() => ({
    queryKey: meKey(this.auth.identity()),
    queryFn: () => this.fetch(),
    enabled: !!this.auth.identity(),
  }));

  readonly me = computed<Schemas['Me'] | null>(() =>
    this.auth.identity() && !this.query.isError() ? (this.query.data() ?? null) : null,
  );

  constructor() {
    const sub = this.events.changes.subscribe((e) => {
      if (e.topic === 'membership.changed') void this.refresh();
    });
    const focus = () => void this.refresh();
    window.addEventListener('focus', focus);
    inject(DestroyRef).onDestroy(() => {
      sub.unsubscribe();
      window.removeEventListener('focus', focus);
    });
  }

  private async fetch(): Promise<Schemas['Me']> {
    return Api.ok(await this.api.client.GET('/v1/me'));
  }

  /** Refetches the account now and returns it, or null when signed out or the fetch fails. */
  async refresh(): Promise<Schemas['Me'] | null> {
    const identity = this.auth.identity();
    if (!identity) return null;
    try {
      const me = await this.client.fetchQuery({
        queryKey: meKey(identity),
        queryFn: () => this.fetch(),
        staleTime: 0,
      });
      return identity === this.auth.identity() ? me : null;
    } catch {
      return null;
    }
  }
}
