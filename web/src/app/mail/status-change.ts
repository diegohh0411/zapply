import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  input,
  output,
  signal,
} from '@angular/core';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, type Schemas } from '../core/api';
import { JobsStore } from '../jobs/jobs.store';
import { StatusPill } from '../jobs/status-pill';

type Message = Schemas['MailMessage'];
type Action = Schemas['MailStatusChangeAction']['action'];

/**
 * The job status a message proposed: applied (with Undo), waiting for review
 * (Apply, Dismiss), or declined. Mail that changes nothing shows nothing.
 */
@Component({
  selector: 'app-mail-status-change',
  imports: [StatusPill, ZardButtonComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (change(); as c) {
      @if (c.outcome !== 'no_change' && proposed()) {
        <div class="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          @switch (c.outcome) {
            @case ('applied') {
              <span>{{ c.auto ? 'Moved automatically' : 'Moved' }}</span>
              @if (previous(); as p) {
                <app-status-pill [status]="p" />
                <span aria-hidden="true">→</span>
              }
              <app-status-pill [status]="proposed()!" />
              <button z-button zType="ghost" zSize="sm" [zDisabled]="busy()" (click)="act('undo')">
                Undo
              </button>
            }
            @case ('review') {
              <span>Suggests</span>
              <app-status-pill [status]="proposed()!" />
              @if (c.confidence !== null) {
                <span>{{ percent(c.confidence) }} sure</span>
              }
              <button z-button zSize="sm" [zDisabled]="busy()" (click)="act('apply')">Apply</button>
              <button
                z-button
                zType="ghost"
                zSize="sm"
                [zDisabled]="busy()"
                (click)="act('dismiss')"
              >
                Dismiss
              </button>
            }
            @case ('dismissed') {
              <span>Dismissed the move to</span>
              <app-status-pill [status]="proposed()!" />
            }
            @case ('undone') {
              <span>Undid the move to</span>
              <app-status-pill [status]="proposed()!" />
            }
          }
        </div>
        @if (error()) {
          <p role="alert" class="mt-1 text-xs text-destructive">{{ error() }}</p>
        }
      }
    }
  `,
})
export class MailStatusChange {
  readonly message = input.required<Message>();
  readonly changed = output<Message>();
  private readonly api = inject(Api);
  private readonly store = inject(JobsStore);
  protected readonly busy = signal(false);
  protected readonly error = signal('');

  protected readonly change = computed(() => this.message().status_change ?? null);
  protected readonly proposed = computed(() => this.status(this.change()?.proposed_status_id));
  protected readonly previous = computed(() => this.status(this.change()?.previous_status_id));

  constructor() {
    void this.store.ensureLoaded();
  }

  private status(id: string | null | undefined) {
    return id ? this.store.statusById().get(id) : undefined;
  }

  protected percent(p: number): string {
    return `${Math.round(p * 100)}%`;
  }

  protected async act(action: Action): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const m = Api.ok(
        await this.api.client.POST('/v1/mail/messages/{id}/status-change', {
          params: { path: { id: this.message().id } },
          body: { action },
        }),
      );
      this.changed.emit(m);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not update the job.');
    } finally {
      this.busy.set(false);
    }
  }
}
