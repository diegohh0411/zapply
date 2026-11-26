import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  input,
  output,
  signal,
  viewChild,
  afterNextRender,
  Injector,
} from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideLink, lucidePencil } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardComboboxComponent, type ZardComboboxGroup } from '@/shared/components/combobox';
import { injectMutation, QueryClient } from '@tanstack/angular-query';

import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { jobActivitiesKey, JobsStore, STAGES } from '../jobs/jobs.store';

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
  imports: [NgIcon, ZardButtonComponent, ZardComboboxComponent],
  providers: [provideIcons({ lucideLink, lucidePencil })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (editing()) {
      <div class="flex flex-wrap items-center gap-2" (click)="$event.stopPropagation()">
        <z-combobox
          class="w-full sm:w-96"
          ariaLabel="Job this email is about"
          placeholder="Choose a job"
          searchPlaceholder="Search by title or company"
          emptyText="No job matches."
          zAutoHighlight
          [groups]="groups()"
          [zValue]="message().job_id ?? none"
          [zDisabled]="busy()"
          [(zOpen)]="pickerOpen"
          (zValueChange)="save($event)"
        />
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
  protected readonly error = signal('');
  private readonly auth = inject(AuthService);
  private readonly queries = inject(QueryClient);
  /**
   * The message moves between two jobs' timelines: both cached timelines and
   * every cached notification list are refetched.
   */
  private readonly relink = injectMutation(() => ({
    mutationFn: async (job: string | null) =>
      Api.ok(
        await this.api.client.PUT('/v1/mail/messages/{id}/job', {
          params: { path: { id: this.message().id } },
          body: { job_id: job },
        }),
      ),
    onSuccess: (m: Message, _job: string | null, ctx: { previous: string | null } | undefined) => {
      const who = this.auth.identity();
      for (const id of new Set([ctx?.previous, m.job_id].filter((x): x is string => !!x)))
        void this.queries.invalidateQueries({ queryKey: jobActivitiesKey(who, id) });
      void this.queries.invalidateQueries({ queryKey: ['notifications', who] });
    },
    onMutate: () => ({ previous: this.message().job_id ?? null }),
  }));
  protected readonly busy = computed(() => this.relink.isPending());

  /** Jobs grouped by stage, open ones first, so the likely one is near the top. */
  /** Open jobs first by stage, then "not about a job", for the combobox. */
  protected readonly groups = computed<ZardComboboxGroup[]>(() => {
    const byId = this.store.statusById();
    const current = this.message().job_id;
    // In-process jobs first: mail is most often about an application under way.
    const order = ['active', 'offer', 'saved', 'closed'];
    const sorted = [...STAGES].sort((a, b) => order.indexOf(a.value) - order.indexOf(b.value));
    const stages = sorted
      .map((s) => ({
        label: s.label,
        options: this.store
          .jobs()
          .filter(
            (j) => (!j.archived || j.id === current) && byId.get(j.status_id)?.stage === s.value,
          )
          .map((j) => ({ value: j.id, label: j.company ? `${j.title} at ${j.company}` : j.title }))
          .sort((a, b) => a.label.localeCompare(b.label)),
      }))
      .filter((g) => g.options.length > 0);
    return [{ options: [{ value: NONE, label: 'Not about any job' }] }, ...stages];
  });
  /** The panel opens with the editor; closing it without a pick cancels. */
  protected readonly pickerOpen = signal(false);
  private readonly picker = viewChild(ZardComboboxComponent);
  private readonly injector = inject(Injector);

  protected start(e: Event): void {
    e.stopPropagation();
    this.store.ensureLoaded();
    this.error.set('');
    this.editing.set(true);
    this.pickerOpen.set(true);
    // Focus the search so typing filters at once.
    afterNextRender(() => this.picker()?.focusInput(), { injector: this.injector });
  }

  protected async save(value: string | string[] | null): Promise<void> {
    // Picking the current choice again clears the combobox: treat it as no change.
    if (typeof value !== 'string') {
      this.editing.set(false);
      return;
    }
    const job = value === NONE ? null : value;
    if (job === (this.message().job_id ?? null)) {
      this.editing.set(false);
      return;
    }
    this.error.set('');
    try {
      const m = await this.relink.mutateAsync(job);
      this.editing.set(false);
      this.changed.emit(m);
    } catch (e) {
      this.error.set(e instanceof Error ? e.message : 'Could not change the job.');
    }
  }
}
