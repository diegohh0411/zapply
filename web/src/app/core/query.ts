import { effect, inject, provideEnvironmentInitializer, type EnvironmentProviders, makeEnvironmentProviders } from '@angular/core';
import { provideTanStackQuery, QueryClient } from '@tanstack/angular-query';

import { ApiError } from './api';
import { AuthService } from './auth.service';
import { EventsService } from './events.service';

/**
 * Server state lives in TanStack Query's cache, keyed by request, so a page
 * shows what it last loaded the moment you come back to it and refreshes in
 * the background. Pages read it with injectQuery; writes go through
 * injectMutation and update or invalidate the keys they touch.
 */
export function provideQueries(): EnvironmentProviders {
  return makeEnvironmentProviders([
    provideTanStackQuery(
      () =>
        new QueryClient({
          defaultOptions: {
            queries: {
              // Fresh enough to skip a refetch when hopping between pages.
              staleTime: 30_000,
              // A 4xx won't fix itself; retry only network and server errors.
              retry: (n, e) => n < 2 && !(e instanceof ApiError && e.status < 500),
            },
          },
        }),
    ),
    provideEnvironmentInitializer(() => {
      const client = inject(QueryClient);
      const auth = inject(AuthService);
      const events = inject(EventsService);
      // Cached data belongs to one member: drop all of it when the sign-in changes.
      let identity = auth.identity();
      effect(() => {
        const next = auth.identity();
        if (next !== identity) client.clear();
        identity = next;
      });
      // The event stream lost its place; anything cached may be out of date.
      events.resync.subscribe(() => void client.invalidateQueries());
    }),
  ]);
}
