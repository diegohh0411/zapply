import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  input,
  output,
  signal,
} from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideLink, lucidePencil } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, type Schemas } from '../core/api';
import { JobsStore, STAGES } from '../jobs/jobs.store';

type Message = Schemas['MailMessage'];

/** The value of the "not about a job" choice; job ids are uuids, so it can't collide. */
const NONE = 'none';

/**
 * Lets the member correct which job a message is about. The classifier can
 * miss, or pick the wrong one; the member's choice is final and teaches it
 * which job later mail from the same sender belongs to.
 */
@Component({
  selector: 'app-mail-job-link',
  imports: [NgIcon, ZardButtonComponent],
  providers: [provideIcons({ lucideLink, lucidePencil })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (editing()) {
      <div class="flex flex-wrap items-center gap-2" (click)="$event.stopPropagation()">
        <label class="sr-only" [attr.for]="'mail-job-' + message().id"
          >Job this email is about</label
        >
        <select
          #pick
          [id]="'mail-job-' + message().id"
          class="h-8 max-w-full min-w-0 rounded-md border border-input bg-background px-2 text-xs sm:max-w-80"
          [disabled]="busy()"
          (change)="save(pick.value)"
          (keydown.escape)="editing.set(false)"
        >
          <option [value]="none" [selected]="!message().job_id">Not about any job</option>
          @for (g of groups(); track g.label) {
            <optgroup [label]="g.label">
              @for (j of g.jobs; track j.id) {
                <option [value]="j.id" [selected]="j.id === message().job_id">{{ j.label }}</option>
              }
            </optgroup>
          }
        </select>
        <button z-button zType="ghost" zSize="sm" [zDisabled]="busy()" (click)="editing.set(false)">
          Cancel
        </button>
      </div>
    } @else {
      <button
        z-button
        zType="ghost"
        zSize="sm"
        class="text-muted-foreground"
        [attr.aria-label]="
          message().job_id ? 'Change the job this email is about' : 'Link this email to a job'
        "
        (click)="start($event)"
      >
        <ng-icon [name]="message().job_id ? 'lucidePencil' : 'lucideLink'" />
        {{ message().job_id ? 'Change job' : 'Link to a job' }}
      </button>
    }
    @if (error()) {
      <p role="alert" class="mt-1 text-xs text-destructive">{{ error() }}</p>
    }
  `,
})
export class MailJobLink {
  readonly message = input.required<Message>();
  readonly changed = output<Message>();
  private readonly api = inject(Api);
  private readonly store = inject(JobsStore);
  protected readonly none = NONE;
  protected readonly editing = signal(false);
  protected readonly busy = signal(false);
  protected readonly error = signal('');

  /** Jobs grouped by stage, open ones first, so the likely one is near the top. */
  protected readonly groups = computed(() => {
    const byId = this.store.statusById();
    const current = this.message().job_id;
    return STAGES.map((s) => ({
      label: s.label,
      jobs: this.store
        .jobs()
        .filter(
          (j) => (!j.archived || j.id === current) && byId.get(j.status_id)?.stage === s.value,
        )
        .map((j) => ({ id: j.id, label: j.company ? `${j.title} at ${j.company}` : j.title }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    })).filter((g) => g.jobs.length > 0);
  });

  protected start(e: Event): void {
    e.stopPropagation();
    this.store.ensureLoaded();
    this.error.set('');
    this.editing.set(true);
  }

  protected async save(value: string): Promise<void> {
    const job = value === NONE ? null : value;
    if (job === (this.message().job_id ?? null)) {
      this.editing.set(false);
      return;
    }
    this.busy.set(true);
    this.error.set('');
    try {
      const m = Api.ok(
        await this.api.client.PUT('/v1/mail/messages/{id}/job', {
          params: { path: { id: this.message().id } },
          body: { job_id: job },
        }),
      );
      this.editing.set(false);
      this.changed.emit(m);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not change the job.');
    } finally {
      this.busy.set(false);
    }
  }
}
