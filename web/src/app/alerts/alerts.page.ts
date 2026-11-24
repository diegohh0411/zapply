import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  effect,
  inject,
  signal,
} from '@angular/core';
import { FormsModule } from '@angular/forms';

import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideExternalLink,
  lucidePause,
  lucidePencil,
  lucidePlay,
  lucidePlus,
  lucideRadar,
  lucideTrash2,
  lucideTriangleAlert,
  lucideX,
} from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ConfirmService } from '../core/confirm.service';
import { relativeTime } from '../mail/format';

type Watch = Schemas['JobWatch'];

const atsLabel: Record<string, string> = {
  greenhouse: 'Greenhouse',
  lever: 'Lever',
  ashby: 'Ashby',
};

/** "react, go , ,backend" → ["react", "go", "backend"] */
function splitTerms(s: string): string[] {
  return s
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean);
}

interface Draft {
  url: string;
  company: string;
  keywords: string;
  locations: string;
  remoteOnly: boolean;
}

const emptyDraft = (): Draft => ({
  url: '',
  company: '',
  keywords: '',
  locations: '',
  remoteOnly: false,
});

/**
 * Job alerts: the employer boards the member watches. zapply reads each board
 * every 30 minutes and sends new postings that match to Notifications, so the
 * member can apply while the applicant pool is still small.
 */
@Component({
  selector: 'app-alerts-page',
  imports: [FormsModule, NgIcon, ZardButtonComponent, ZardInputComponent],
  providers: [
    provideIcons({
      lucideExternalLink,
      lucidePause,
      lucidePencil,
      lucidePlay,
      lucidePlus,
      lucideRadar,
      lucideTrash2,
      lucideTriangleAlert,
      lucideX,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-[56rem] px-6 pb-16 pt-7">
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div class="max-w-[38rem]">
          <h1 class="text-xl font-semibold tracking-tight">Job alerts</h1>
          <p class="mt-1 text-sm text-muted-foreground">
            Watch a company's careers board and hear about new openings within 30 minutes of them
            going live, before they spread to LinkedIn and the applicant pool fills up.
          </p>
        </div>
        @if (!formOpen()) {
          <button z-button (click)="openForm()">
            <ng-icon name="lucidePlus" />Watch a company
          </button>
        }
      </header>

      @if (error()) {
        <div
          role="alert"
          class="mt-5 flex items-start justify-between gap-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive"
        >
          <p>{{ error() }}</p>
          <button
            z-button
            zType="ghost"
            zSize="icon-xs"
            aria-label="Dismiss"
            (click)="error.set('')"
          >
            <ng-icon name="lucideX" />
          </button>
        </div>
      }

      @if (formOpen()) {
        <form
          class="mt-6 grid gap-4 rounded-lg border bg-card p-5"
          (ngSubmit)="save()"
          aria-labelledby="watch-form-title"
        >
          <h2 id="watch-form-title" class="text-sm font-semibold">
            {{ editing() ? 'Edit ' + editing()!.company : 'Watch a company' }}
          </h2>
          @if (!editing()) {
            <label class="grid gap-1.5 text-sm">
              <span class="font-medium">Careers link</span>
              <input
                z-input
                name="url"
                [(ngModel)]="draft.url"
                placeholder="https://jobs.lever.co/acme"
                autocomplete="off"
                required
              />
              <span class="text-xs text-muted-foreground">
                A Greenhouse, Lever or Ashby board, or any posting on one. Most startups' "Careers"
                or "Apply" links lead to one of these.
              </span>
              @if (fieldError()['url']; as m) {
                <span class="text-xs text-destructive">{{ m }}</span>
              }
            </label>
          }
          <label class="grid gap-1.5 text-sm">
            <span class="font-medium"
              >Company name <span class="font-normal text-muted-foreground">(optional)</span></span
            >
            <input
              z-input
              name="company"
              [(ngModel)]="draft.company"
              placeholder="Taken from the board"
            />
            @if (fieldError()['company']; as m) {
              <span class="text-xs text-destructive">{{ m }}</span>
            }
          </label>
          <div class="grid gap-4 sm:grid-cols-2">
            <label class="grid gap-1.5 text-sm">
              <span class="font-medium">Title keywords</span>
              <input
                z-input
                name="keywords"
                [(ngModel)]="draft.keywords"
                placeholder="engineer, backend"
              />
              <span class="text-xs text-muted-foreground"
                >Comma-separated. Empty alerts on every opening.</span
              >
              @if (fieldError()['keywords']; as m) {
                <span class="text-xs text-destructive">{{ m }}</span>
              }
            </label>
            <label class="grid gap-1.5 text-sm">
              <span class="font-medium">Locations</span>
              <input
                z-input
                name="locations"
                [(ngModel)]="draft.locations"
                placeholder="New York, Mexico"
              />
              <span class="text-xs text-muted-foreground"
                >Comma-separated. Empty matches anywhere.</span
              >
              @if (fieldError()['locations']; as m) {
                <span class="text-xs text-destructive">{{ m }}</span>
              }
            </label>
          </div>
          <label class="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              name="remote"
              class="size-4 accent-foreground"
              [(ngModel)]="draft.remoteOnly"
            />
            Only remote roles
          </label>
          <div class="flex justify-end gap-2">
            <button z-button zType="ghost" type="button" (click)="closeForm()">Cancel</button>
            <button z-button type="submit" [zLoading]="saving()">
              {{ editing() ? 'Save' : 'Start watching' }}
            </button>
          </div>
        </form>
      }

      @if (watches().length) {
        <ul class="mt-6 divide-y rounded-lg border">
          @for (w of watches(); track w.id) {
            <li class="flex flex-wrap items-start gap-3 px-4 py-3.5" [class.opacity-60]="w.paused">
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium">{{ w.company }}</span>
                  <span class="rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground">{{
                    ats(w.ats)
                  }}</span>
                  @if (w.paused) {
                    <span class="rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground"
                      >Paused</span
                    >
                  }
                </div>
                <p class="mt-1 text-sm text-muted-foreground">{{ filters(w) }}</p>
                <p class="mt-1 flex items-center gap-1 text-xs text-muted-foreground">
                  @if (w.last_error) {
                    <ng-icon name="lucideTriangleAlert" class="text-amber-600" aria-hidden="true" />
                    <span class="text-amber-700 dark:text-amber-400">{{ w.last_error }}</span>
                  } @else if (w.last_checked_at) {
                    Checked {{ ago(w.last_checked_at) }}
                  } @else {
                    First check within a minute
                  }
                </p>
              </div>
              <div class="flex items-center gap-0.5">
                <a
                  z-button
                  zType="ghost"
                  zSize="icon-sm"
                  [href]="w.board_url"
                  target="_blank"
                  rel="noopener noreferrer"
                  aria-label="Open careers page"
                  title="Open careers page"
                >
                  <ng-icon name="lucideExternalLink" />
                </a>
                <button
                  z-button
                  zType="ghost"
                  zSize="icon-sm"
                  aria-label="Edit"
                  title="Edit"
                  (click)="edit(w)"
                >
                  <ng-icon name="lucidePencil" />
                </button>
                <button
                  z-button
                  zType="ghost"
                  zSize="icon-sm"
                  [attr.aria-label]="w.paused ? 'Resume' : 'Pause'"
                  [title]="w.paused ? 'Resume' : 'Pause'"
                  (click)="togglePause(w)"
                >
                  <ng-icon [name]="w.paused ? 'lucidePlay' : 'lucidePause'" />
                </button>
                <button
                  z-button
                  zType="ghost"
                  zSize="icon-sm"
                  aria-label="Stop watching"
                  title="Stop watching"
                  (click)="remove(w)"
                >
                  <ng-icon name="lucideTrash2" />
                </button>
              </div>
            </li>
          }
        </ul>
      } @else if (loaded() && !formOpen()) {
        <div
          class="mt-10 flex flex-col items-center gap-2 text-center text-sm text-muted-foreground"
        >
          <ng-icon name="lucideRadar" class="text-3xl" aria-hidden="true" />
          <p class="font-medium text-foreground">No companies watched yet</p>
          <p class="max-w-[28rem]">
            Paste a company's careers link and zapply will tell you when it posts a role that fits,
            usually hours before it shows up elsewhere.
          </p>
        </div>
      }
    </div>
  `,
})
export class AlertsPage {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly confirm = inject(ConfirmService);

  protected readonly watches = signal<Watch[]>([]);
  protected readonly loaded = signal(false);
  protected readonly error = signal('');
  protected readonly formOpen = signal(false);
  protected readonly editing = signal<Watch | null>(null);
  protected readonly saving = signal(false);
  protected readonly fieldError = signal<Record<string, string>>({});
  protected readonly now = signal(Date.now());
  protected draft: Draft = emptyDraft();

  protected readonly ats = (s: string): string => atsLabel[s] ?? s;
  protected readonly ago = (iso: string): string => relativeTime(iso, this.now());

  constructor() {
    effect(() => {
      this.auth.identity();
      this.watches.set([]);
      void this.load();
    });
    const tick = setInterval(() => this.now.set(Date.now()), 60_000);
    inject(DestroyRef).onDestroy(() => clearInterval(tick));
  }

  protected filters(w: Watch): string {
    const parts = [w.keywords.length ? `Titles with ${w.keywords.join(', ')}` : 'Every opening'];
    if (w.locations.length) parts.push(`in ${w.locations.join(', ')}`);
    if (w.remote_only) parts.push('remote only');
    return parts.join(' · ');
  }

  async load(): Promise<void> {
    const identity = this.auth.identity();
    if (!identity) return;
    try {
      const list = Api.ok(await this.api.client.GET('/v1/job-watches'));
      if (identity !== this.auth.identity()) return;
      this.watches.set(list.items);
      this.loaded.set(true);
    } catch (e) {
      this.fail(e, 'Could not load your job alerts.');
    }
  }

  private fail(e: unknown, fallback: string): void {
    this.error.set(e instanceof Error ? e.message : fallback);
  }

  protected openForm(): void {
    this.editing.set(null);
    this.draft = emptyDraft();
    this.fieldError.set({});
    this.formOpen.set(true);
  }

  protected edit(w: Watch): void {
    this.editing.set(w);
    this.draft = {
      url: w.board_url,
      company: w.company,
      keywords: w.keywords.join(', '),
      locations: w.locations.join(', '),
      remoteOnly: w.remote_only,
    };
    this.fieldError.set({});
    this.formOpen.set(true);
  }

  protected closeForm(): void {
    this.formOpen.set(false);
    this.editing.set(null);
  }

  private replace(w: Watch): void {
    this.watches.update((list) =>
      list.some((x) => x.id === w.id) ? list.map((x) => (x.id === w.id ? w : x)) : [w, ...list],
    );
  }

  async save(): Promise<void> {
    if (this.saving()) return;
    const d = this.draft,
      editing = this.editing();
    const body = {
      company: d.company.trim() || undefined,
      keywords: splitTerms(d.keywords),
      locations: splitTerms(d.locations),
      remote_only: d.remoteOnly,
    };
    this.saving.set(true);
    this.fieldError.set({});
    this.error.set('');
    try {
      const w = editing
        ? Api.ok(
            await this.api.client.PATCH('/v1/job-watches/{id}', {
              params: { path: { id: editing.id } },
              body: { ...body, company: body.company ?? editing.company },
            }),
          )
        : Api.ok(
            await this.api.client.POST('/v1/job-watches', { body: { ...body, url: d.url.trim() } }),
          );
      this.replace(w);
      this.closeForm();
    } catch (e) {
      if (e instanceof ApiError && e.fieldErrors.length)
        this.fieldError.set(Object.fromEntries(e.fieldErrors.map((f) => [f.field, f.message])));
      else if (e instanceof ApiError && e.status === 409)
        this.fieldError.set({ url: 'You already watch this company.' });
      else this.fail(e, 'Could not save the alert.');
    } finally {
      this.saving.set(false);
    }
  }

  async togglePause(w: Watch): Promise<void> {
    try {
      this.replace(
        Api.ok(
          await this.api.client.PATCH('/v1/job-watches/{id}', {
            params: { path: { id: w.id } },
            body: { paused: !w.paused },
          }),
        ),
      );
    } catch (e) {
      this.fail(e, 'Could not change the alert.');
    }
  }

  async remove(w: Watch): Promise<void> {
    const ok = await this.confirm.ask({
      title: `Stop watching ${w.company}?`,
      description: 'Its alerts leave your notifications too.',
      okText: 'Stop watching',
    });
    if (!ok) return;
    try {
      Api.ok(
        await this.api.client.DELETE('/v1/job-watches/{id}', { params: { path: { id: w.id } } }),
      );
      this.watches.update((list) => list.filter((x) => x.id !== w.id));
    } catch (e) {
      this.fail(e, 'Could not remove the alert.');
    }
  }
}
