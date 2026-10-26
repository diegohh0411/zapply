import { inject, Injectable } from '@angular/core';
import createClient, { type Middleware } from 'openapi-fetch';

import type { components, paths } from '../api/schema';
import { AuthService } from './auth.service';

export type Schemas = components['schemas'];
export type FieldError = Schemas['FieldError'];

/** A problem+json response from the API. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly problem: Schemas['Problem'] | undefined,
  ) {
    super(problem?.detail || problem?.title || `Request failed (${status})`);
  }

  get fieldErrors(): FieldError[] {
    return this.problem?.errors ?? [];
  }

  /** The first error for a field, e.g. "title" or "highlights[0].text". */
  fieldError(field: string): string | undefined {
    return this.fieldErrors.find((e) => e.field === field)?.message;
  }
}

/** Typed client for /v1, generated from api/openapi.yaml (npm run gen:api). */
@Injectable({ providedIn: 'root' })
export class Api {
  private readonly auth = inject(AuthService);
  readonly client = createClient<paths>({ baseUrl: '' });

  constructor() {
    const identities = new WeakMap<Request, string | null>();
    const auth: Middleware = {
      onRequest: async ({ request }) => {
        const identity = this.auth.identity();
        const token = await this.auth.token();
        if (identity !== this.auth.identity())
          throw new Error('Sign-in changed; request discarded');
        identities.set(request, identity);
        if (token) request.headers.set('Authorization', 'Bearer ' + token);
        return request;
      },
      onResponse: async ({ request, response }) => {
        if (identities.get(request) !== this.auth.identity())
          throw new Error('Sign-in changed; response discarded');
        if (response.status === 401) void this.auth.signOut();
        return response;
      },
    };
    this.client.use(auth);
  }

  /** Unwraps an openapi-fetch result, throwing ApiError on failure. */
  static ok<T>(r: { data?: T; error?: unknown; response: Response }): T {
    if (r.error !== undefined || !r.response.ok) {
      throw new ApiError(r.response.status, r.error as Schemas['Problem'] | undefined);
    }
    return r.data as T;
  }
}

/** ETag of a response, for If-Match. */
export function etagOf(r: { response: Response }): string {
  return r.response.headers.get('ETag') ?? '';
}
