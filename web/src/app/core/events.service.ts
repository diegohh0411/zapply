import { effect, inject, Injectable, signal } from '@angular/core';
import { Subject } from 'rxjs';

import { AuthService } from './auth.service';

export interface ChangeEvent {
  topic: string;
  data: { id: string; version?: number; job_id?: string };
}

/**
 * Live changes from GET /v1/events (docs/parts/03-events.md). EventSource
 * can't send a bearer token, so this reads the stream with fetch.
 *
 * Contract: after `ready`, a client that had no cursor (or got `reset`)
 * refetches; `resync` tells stores to do that.
 */
@Injectable({ providedIn: 'root' })
export class EventsService {
  private readonly auth = inject(AuthService);

  readonly changes = new Subject<ChangeEvent>();
  readonly resync = new Subject<void>();
  readonly status = signal<'offline' | 'connecting' | 'live'>('offline');

  private lastId: string | null = null;
  private needsResync = true;
  private abort: AbortController | null = null;
  private backoff = 1000;

  constructor() {
    effect(() => {
      if (this.auth.signedIn()) this.start();
      else this.stop();
    });
  }

  private start(): void {
    if (this.abort) return;
    this.abort = new AbortController();
    void this.loop(this.abort.signal);
  }

  private stop(): void {
    this.abort?.abort();
    this.abort = null;
    this.lastId = null;
    this.needsResync = true;
    this.status.set('offline');
  }

  private async loop(signal: AbortSignal): Promise<void> {
    while (!signal.aborted) {
      let retry = this.backoff;
      try {
        retry = await this.connect(signal);
      } catch {
        // network error: back off below
      }
      if (signal.aborted) return;
      this.status.set('connecting');
      await sleep(retry, signal);
      this.backoff = Math.min(this.backoff * 2, 30_000);
    }
  }

  /** Reads one stream until it ends; returns how long to wait before reconnecting. */
  private async connect(signal: AbortSignal): Promise<number> {
    this.status.set('connecting');
    const token = await this.auth.token();
    if (!token) return 5000;
    const headers: Record<string, string> = { Authorization: 'Bearer ' + token, Accept: 'text/event-stream' };
    if (this.lastId) headers['Last-Event-ID'] = this.lastId;
    const res = await fetch('/v1/events', { headers, signal, cache: 'no-store' });
    if (!res.ok || !res.body) return res.status === 429 ? 10_000 : this.backoff;

    let retry = this.backoff;
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    let buf = '';
    let ev: { id?: string; event?: string; data: string[] } = { data: [] };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return retry;
      buf += value;
      let nl: number;
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl).replace(/\r$/, '');
        buf = buf.slice(nl + 1);
        if (line === '') {
          this.dispatch(ev);
          ev = { data: [] };
          continue;
        }
        if (line.startsWith(':')) continue;
        const i = line.indexOf(':');
        const field = i < 0 ? line : line.slice(0, i);
        const val = i < 0 ? '' : line.slice(i + 1).replace(/^ /, '');
        if (field === 'id') ev.id = val;
        else if (field === 'event') ev.event = val;
        else if (field === 'data') ev.data.push(val);
        else if (field === 'retry' && /^\d+$/.test(val)) retry = +val;
      }
    }
  }

  private dispatch(ev: { id?: string; event?: string; data: string[] }): void {
    if (!ev.event) return;
    if (ev.id) this.lastId = ev.id;
    switch (ev.event) {
      case 'ready':
        this.status.set('live');
        this.backoff = 1000;
        if (this.needsResync) {
          this.needsResync = false;
          this.resync.next();
        }
        return;
      case 'reset':
        this.lastId = null;
        this.needsResync = true;
        return;
    }
    try {
      this.changes.next({ topic: ev.event, data: JSON.parse(ev.data.join('\n')) });
    } catch {
      // ignore malformed data
    }
  }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener('abort', () => {
      clearTimeout(t);
      resolve();
    });
  });
}
