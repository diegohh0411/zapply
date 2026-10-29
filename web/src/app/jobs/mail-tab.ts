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
import { RouterLink } from '@angular/router';

import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { categoryLabel } from '../mail/format';

/** Mail about one application: the messages classified into this job. */
@Component({
  selector: 'app-mail-tab',
  imports: [DatePipe, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="space-y-3 pt-3">
      @if (error()) {
        <p role="alert" class="text-sm text-destructive">{{ error() }}</p>
      }
      <ul class="divide-y rounded-lg border">
        @for (m of messages(); track m.id) {
          <li class="space-y-1 p-3">
            <div class="flex flex-wrap items-center gap-2 text-sm">
              <span class="rounded-full bg-muted px-2 py-0.5 text-xs font-medium">{{
                label(m.category)
              }}</span>
              <span class="truncate font-medium">{{ m.from }}</span>
              <time class="ml-auto text-muted-foreground" [attr.datetime]="m.received_at">{{
                m.received_at | date: 'medium'
              }}</time>
            </div>
            <p class="text-sm font-medium">{{ m.subject || '(no subject)' }}</p>
            <p class="text-sm text-muted-foreground">{{ m.snippet }}</p>
          </li>
        } @empty {
          <li class="p-4 text-center text-sm text-muted-foreground">
            @if (loading()) {
              Loading…
            } @else {
              No mail linked to this job yet.
            }
          </li>
        }
      </ul>
      <p class="text-xs text-muted-foreground">
        Connect Gmail on the <a routerLink="/mail" class="underline">Mail</a> page to fill this in.
      </p>
    </div>
  `,
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
  private request = 0;

  constructor() {
    effect(() => {
      this.auth.identity();
      this.messages.set([]);
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
