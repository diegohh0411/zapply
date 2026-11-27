import {
  ChangeDetectionStrategy,
  Component,
  effect,
  inject,
  signal,
  untracked,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';

type Settings = Schemas['SearchSettings'];
type SystemKey = Schemas['SystemKey'];
type Balance = Schemas['SearchBalance'];

const NO_KEY = 'none';
const single = (value: string | string[] | null): string =>
  Array.isArray(value) ? (value[0] ?? '') : (value ?? '');

// Job search: TheirStack supplies the postings job alerts are made of. It
// charges one API credit per posting returned; the platform pays.
@Component({
  selector: 'app-search-settings',
  imports: [FormsModule, RouterLink, ZardButtonComponent, ZardInputComponent, ...ZardSelectImports],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section aria-labelledby="search-heading" class="space-y-5">
      <header class="space-y-1">
        <h2 id="search-heading" class="text-lg font-semibold">Job search</h2>
        <p class="max-w-2xl text-sm text-muted-foreground">
          TheirStack supplies the postings behind job alerts, across every company, including those
          that post only on LinkedIn. Every 30 minutes it is asked for new postings in the countries
          members accept and for remote ones; each run returns at most the postings below, at one
          API credit each, paid by the platform.
        </p>
      </header>
      @if (balance(); as b) {
        @if (b.configured) {
          <p class="text-sm" role="status" [class.text-destructive]="!!b.error || low(b)">
            @if (b.error) {
              Couldn't read the TheirStack balance: {{ b.error }}.
            } @else {
              TheirStack balance:
              <span class="font-medium tabular-nums">{{ count(b.credits) }}</span> API credits left,
              {{ count(b.used) }} used this billing cycle{{
                b.expires_at ? '; the earliest expire ' + day(b.expires_at) : ''
              }}.
              @if (low(b)) {
                That's under a day of supplier runs; top up in TheirStack.
              }
            }
          </p>
        }
      }
      @if (error()) {
        <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">
          {{ error() }}
        </p>
      }
      @if (settings(); as saved) {
        <form class="grid max-w-3xl gap-4 md:grid-cols-2" (ngSubmit)="save()">
          <div class="field">
            <span>TheirStack key</span>
            <z-select
              aria-label="TheirStack key"
              zPlaceholder="No key"
              [ngModel]="draft.key_id || noKey"
              (ngModelChange)="draft.key_id = pick(single($event))"
              [zDisabled]="busy()"
            >
              <z-select-item [zValue]="noKey">No key</z-select-item>
              @for (k of keys(); track k.id) {
                <z-select-item [zValue]="k.id">{{ k.label }} · {{ k.fingerprint }}</z-select-item>
              }
            </z-select>
            <span class="text-xs font-normal text-muted-foreground"
              >Add one in
              <a class="underline" routerLink="/admin/system-keys">system keys</a>.</span
            >
          </div>
          <label class="field"
            ><span>Postings per search</span
            ><input
              z-input
              name="max"
              type="number"
              min="1"
              max="100"
              step="1"
              [(ngModel)]="draft.max_results"
            />
            <span class="text-xs font-normal text-muted-foreground"
              >The most one supplier run returns, which caps what it can cost.</span
            >
          </label>
          <div class="flex flex-wrap items-center gap-3 md:col-span-2">
            <button z-button type="submit" [zDisabled]="busy()">
              {{ busy() ? 'Saving…' : 'Save job search' }}
            </button>
            <span class="text-sm text-muted-foreground" role="status">{{
              notice() || status(saved)
            }}</span>
          </div>
        </form>
      } @else {
        <p class="text-sm" role="status">
          {{ loading() ? 'Loading job search settings…' : 'Job search settings are unavailable.' }}
        </p>
      }
    </section>
  `,
})
export class SearchSettings {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  protected readonly single = single;
  protected readonly noKey = NO_KEY;
  readonly settings = signal<Settings | null>(null);
  readonly keys = signal<SystemKey[]>([]);
  readonly error = signal('');
  readonly notice = signal('');
  readonly loading = signal(false);
  readonly busy = signal(false);
  readonly balance = signal<Balance | null>(null);
  draft = { key_id: '', max_results: 25 };
  private generation = 0;

  constructor() {
    effect(() => {
      this.auth.identity();
      untracked(() => {
        this.generation++;
        this.settings.set(null);
        void this.load();
      });
    });
  }

  protected pick(value: string): string {
    this.notice.set('');
    return value === NO_KEY ? '' : value;
  }

  protected count(n: number | null | undefined): string {
    return (n ?? 0).toLocaleString('en-US');
  }

  protected day(iso: string): string {
    return new Date(iso).toLocaleDateString(undefined, {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
    });
  }

  /** Fewer credits left than both suppliers could use in a day (two runs an hour each). */
  protected low(b: Balance): boolean {
    const max = this.settings()?.max_results ?? 25;
    return b.credits != null && b.credits < max * 2 * 48;
  }

  private async loadBalance(current: () => boolean): Promise<void> {
    try {
      const b = Api.ok(await this.api.GET('/v1/admin/search-balance'));
      if (current()) this.balance.set(b);
    } catch {
      if (current()) this.balance.set(null);
    }
  }

  protected status(s: Settings): string {
    return s.key_id
      ? 'Job alerts are supplied through TheirStack.'
      : 'Job alerts get no new postings until a key is set.';
  }

  async load(): Promise<void> {
    const identity = this.auth.identity(),
      seq = ++this.generation;
    const current = () => identity === this.auth.identity() && seq === this.generation;
    this.loading.set(true);
    this.error.set('');
    try {
      const [saved, keys] = await Promise.all([
        this.api.GET('/v1/admin/search-settings'),
        this.api.GET('/v1/admin/system-keys'),
      ]);
      if (!current()) return;
      this.keys.set(Api.ok(keys).filter((k) => k.kind === 'theirstack'));
      this.apply(Api.ok(saved));
      void this.loadBalance(current);
    } catch (e) {
      if (current()) this.error.set(this.message(e));
    } finally {
      if (current()) this.loading.set(false);
    }
  }

  private apply(s: Settings): void {
    this.settings.set(s);
    this.draft = { key_id: s.key_id ?? '', max_results: s.max_results };
  }

  async save(): Promise<void> {
    const saved = this.settings();
    if (!saved || this.busy()) return;
    const identity = this.auth.identity(),
      seq = this.generation;
    const current = () => identity === this.auth.identity() && seq === this.generation;
    this.busy.set(true);
    this.error.set('');
    this.notice.set('');
    try {
      const value = Api.ok(
        await this.api.PUT('/v1/admin/search-settings', {
          params: { header: { 'If-Match': `"${saved.version}"` } },
          body: {
            key_id: this.draft.key_id || null,
            max_results: Math.max(
              1,
              Math.min(100, Math.trunc(Number(this.draft.max_results) || 25)),
            ),
          },
        }),
      );
      if (current()) {
        this.apply(value);
        this.notice.set('Job search saved.');
        void this.loadBalance(current);
      }
    } catch (e) {
      if (current()) this.error.set(this.message(e));
    } finally {
      if (current()) this.busy.set(false);
    }
  }

  private message(error: unknown): string {
    if (error instanceof ApiError && error.status === 412)
      return 'Someone else changed these settings. Refresh to see their version, then redo your changes.';
    if (error instanceof ApiError && error.fieldErrors.length)
      return error.fieldErrors.map((e) => e.message).join('; ');
    return error instanceof Error ? error.message : 'Could not save changes.';
  }
}
