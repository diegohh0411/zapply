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
import { lucideChevronRight, lucideExternalLink } from '@ng-icons/lucide';
import { RouterLink } from '@angular/router';

import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { categoryLabel } from '../mail/format';
import { MailStatusChange } from '../mail/status-change';

/** Mail about one application: the messages classified into this job. */
@Component({
  selector: 'app-mail-tab',
  imports: [DatePipe, MailStatusChange, NgIcon, RouterLink],
  providers: [provideIcons({ lucideChevronRight, lucideExternalLink })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="space-y-3 pt-3">
      @if (error()) {
        <p role="alert" class="text-sm text-destructive">{{ error() }}</p>
      }
      <ul class="divide-y rounded-lg border">
        @for (m of messages(); track m.id) {
          <li class="pr-2">
            <div class="flex items-start gap-1">
              <button
                type="button"
                class="flex min-w-0 flex-1 items-start gap-2 rounded-lg px-3 py-2 text-left text-sm hover:bg-muted/50"
                [attr.aria-expanded]="isOpen(m.id)"
                (click)="toggle(m.id)"
              >
                <ng-icon
                  name="lucideChevronRight"
                  size="16"
                  class="mt-0.5 shrink-0 text-muted-foreground transition-transform"
                  [class.rotate-90]="isOpen(m.id)"
                />
                <span class="min-w-0 flex-1 space-y-1">
                  <span class="flex items-center gap-2">
                    <span class="shrink-0 rounded-full bg-muted px-2 py-0.5 text-xs font-medium">{{
                      label(m.category)
                    }}</span>
                    <span class="truncate font-medium">{{ m.subject || '(no subject)' }}</span>
                    <time
                      class="ml-auto shrink-0 text-xs text-muted-foreground"
                      [attr.datetime]="m.received_at"
                      >{{ m.received_at | date: 'MMM d, y' }}</time
                    >
                  </span>
                  @if (isOpen(m.id)) {
                    <span class="block text-xs text-muted-foreground">
                      {{ m.from }} · {{ m.received_at | date: 'medium' }}
                    </span>
                    <span class="block text-sm text-muted-foreground">{{ m.snippet }}</span>
                  }
                </span>
              </button>
              <a
                [href]="m.gmail_url"
                target="_blank"
                rel="noopener noreferrer"
                class="mt-1.5 shrink-0 rounded-md p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
                aria-label="Open in Gmail"
                title="Open in Gmail"
              >
                <ng-icon name="lucideExternalLink" size="16" />
              </a>
            </div>
            <app-mail-status-change
              class="block pb-2 pl-9 empty:hidden"
              [message]="m"
              (changed)="replace($event)"
            />
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
  private readonly open = signal<ReadonlySet<string>>(new Set());
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
