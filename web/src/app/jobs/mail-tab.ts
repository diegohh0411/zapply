import { DatePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  effect,
  inject,
  input,
  signal,
} from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideChevronRight,
  lucideExternalLink,
  lucideRefreshCw,
  lucideMail,
} from '@ng-icons/lucide';
import { RouterLink } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { categoryLabel } from '../mail/format';
import { MailStatusChange } from '../mail/status-change';

/** Mail about one application: the messages classified into this job. */
@Component({
  selector: 'app-mail-tab',
  imports: [DatePipe, MailStatusChange, NgIcon, RouterLink, ZardButtonComponent],
  providers: [
    provideIcons({ lucideChevronRight, lucideExternalLink, lucideRefreshCw, lucideMail }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './mail-tab.html',
})
export class MailTab {
  readonly jobId = input.required<string>();
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly events = inject(EventsService);
  protected readonly label = categoryLabel;
  protected readonly messages = signal<Schemas['MailMessage'][]>([]);
  protected readonly loading = signal(false);
  protected readonly error = signal('');
  private readonly open = signal<ReadonlySet<string>>(new Set());
  private request = 0;

  constructor() {
    effect(() => {
      this.auth.identity();
      this.messages.set([]);
      this.open.set(new Set());
      void this.load(this.jobId());
    });
    const change = this.events.changes.subscribe((e) => {
      if (e.topic === 'mail.message.classified') void this.load(this.jobId());
    });
    const resync = this.events.resync.subscribe(() => void this.load(this.jobId()));
    inject(DestroyRef).onDestroy(() => {
      this.request++;
      change.unsubscribe();
      resync.unsubscribe();
    });
  }

  protected refresh(): void {
    void this.load(this.jobId());
  }

  protected replace(m: Schemas['MailMessage']): void {
    this.messages.update((list) => list.map((x) => (x.id === m.id ? m : x)));
  }

  protected isOpen(id: string): boolean {
    return this.open().has(id);
  }

  protected toggle(id: string): void {
    this.open.update((s) => {
      const next = new Set(s);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  }

  private async load(jobId: string): Promise<void> {
    const identity = this.auth.identity(),
      request = ++this.request;
    if (!identity) return;
    this.loading.set(true);
    this.error.set('');
    try {
      const page = Api.ok(
        await this.api.client.GET('/v1/mail/messages', {
          params: { query: { job_id: jobId, limit: 50 } },
        }),
      );
      if (identity === this.auth.identity() && request === this.request)
        this.messages.set(page.items);
    } catch (e) {
      if (identity === this.auth.identity() && request === this.request)
        this.error.set(e instanceof Error ? e.message : 'Could not load mail.');
    } finally {
      if (identity === this.auth.identity() && request === this.request) this.loading.set(false);
    }
  }
}
