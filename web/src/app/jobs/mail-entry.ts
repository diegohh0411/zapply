import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, input, output, signal } from '@angular/core';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideChevronRight, lucideExternalLink } from '@ng-icons/lucide';

import { type Schemas } from '../core/api';
import { categoryLabel } from '../mail/format';
import { MailStatusChange } from '../mail/status-change';

/** One message in a job's activity timeline, with an expandable preview. */
@Component({
  selector: 'app-mail-entry',
  imports: [DatePipe, MailStatusChange, NgIcon],
  providers: [provideIcons({ lucideChevronRight, lucideExternalLink })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @let m = message();
    <button
      type="button"
      class="-mx-2 flex w-[calc(100%+1rem)] items-start gap-2 rounded-lg px-2 py-1 text-left hover:bg-muted/50 focus-visible:outline-2 focus-visible:outline-ring"
      [attr.aria-expanded]="open()"
      [attr.aria-controls]="'mail-preview-' + m.id"
      (click)="open.set(!open())"
    >
      <span class="min-w-0 flex-1">
        <span class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
          <span class="min-w-0 text-sm">
            <span class="font-medium">{{ m.subject || '(no subject)' }}</span>
          </span>
          <time class="shrink-0 text-xs text-muted-foreground" [attr.datetime]="m.received_at">{{
            m.received_at | date: 'medium'
          }}</time>
        </span>
        <span class="mt-0.5 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <span class="break-all">{{ m.from }}</span>
          <span class="rounded-md bg-muted px-1.5 py-0.5 text-foreground">{{
            label(m.category)
          }}</span>
        </span>
      </span>
      <ng-icon
        name="lucideChevronRight"
        class="mt-1 shrink-0 text-muted-foreground motion-safe:transition-transform"
        [class.rotate-90]="open()"
      />
    </button>
    <div [id]="'mail-preview-' + m.id" [hidden]="!open()" class="mt-1 space-y-2">
      <p
        class="max-w-prose whitespace-pre-wrap break-words text-sm leading-relaxed text-muted-foreground"
      >
        {{ m.snippet || 'No text preview is available for this message.' }}
      </p>
      <a
        [href]="m.gmail_url"
        target="_blank"
        rel="noopener noreferrer"
        class="inline-flex items-center gap-2 rounded-md py-1 text-sm font-medium text-brand underline-offset-4 hover:underline focus-visible:outline-2 focus-visible:outline-ring"
        >Read full message in Gmail <ng-icon name="lucideExternalLink"
      /></a>
    </div>
    <app-mail-status-change
      class="mt-1 block empty:hidden"
      [message]="m"
      (changed)="changed.emit($event)"
    />
  `,
})
export class MailEntry {
  readonly message = input.required<Schemas['MailMessage']>();
  readonly changed = output<Schemas['MailMessage']>();
  protected readonly open = signal(false);
  protected readonly label = categoryLabel;
}
