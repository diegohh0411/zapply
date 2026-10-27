import { DestroyRef, effect, inject, Injectable, signal } from '@angular/core';

import { Api, type Schemas } from './api';
import { AuthService } from './auth.service';
import { EventsService } from './events.service';

@Injectable({ providedIn: 'root' })
export class AccountService {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  readonly me = signal<Schemas['Me'] | null>(null);
  private generation = 0;

  constructor() {
    effect(() => {
      this.auth.identity();
      this.me.set(null);
      void this.refresh();
    });
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

  async refresh(): Promise<Schemas['Me'] | null> {
    const identity = this.auth.identity();
    const generation = ++this.generation;
    if (!identity) {
      this.me.set(null);
      return null;
    }
    try {
      const me = Api.ok(await this.api.client.GET('/v1/me'));
      if (identity === this.auth.identity() && generation === this.generation) this.me.set(me);
      return identity === this.auth.identity() ? me : null;
    } catch {
      if (identity === this.auth.identity() && generation === this.generation) this.me.set(null);
      return null;
    }
  }
}
