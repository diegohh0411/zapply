import { ChangeDetectionStrategy, Component, effect, inject, signal, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';

type Settings = Schemas['SearchSettings'];
type SystemKey = Schemas['SystemKey'];

const NO_KEY = 'none';
const single = (value: string | string[] | null): string => (Array.isArray(value) ? (value[0] ?? '') : (value ?? ''));
/** "0.006900000000" → "0.0069" */
const trimUSD = (v: string | null | undefined): string => (v ? v.replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '') : '');

// Search watchers: members' saved job searches run through TheirStack, which
// charges one API credit per posting returned. Members pay for their polls
// from their plan, at the credit price set here.
@Component({
  selector: 'app-search-settings',
  imports: [FormsModule, RouterLink, ZardButtonComponent, ZardInputComponent, ...ZardSelectImports],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section aria-labelledby="search-heading" class="space-y-5">
      <header class="space-y-1">
        <h2 id="search-heading" class="text-lg font-semibold">Job search for alerts</h2>
        <p class="max-w-2xl text-sm text-muted-foreground">
          Search watchers look for roles across every company, including those that post only on LinkedIn, through TheirStack. It charges one API credit
          per posting a search returns; each member pays for their searches from their plan, at the price below. Plans set how many search watchers a
          member can keep.
        </p>
      </header>
      @if (error()) {
        <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">{{ error() }}</p>
      }
      @if (settings(); as saved) {
        <form class="grid max-w-3xl gap-4 md:grid-cols-3" (ngSubmit)="save()">
          <div class="field">
            <span>TheirStack key</span>
            <z-select aria-label="TheirStack key" zPlaceholder="No key" [ngModel]="draft.key_id || noKey" (ngModelChange)="draft.key_id = pick(single($event))" [zDisabled]="busy()">
              <z-select-item [zValue]="noKey">No key</z-select-item>
              @for (k of keys(); track k.id) {
                <z-select-item [zValue]="k.id">{{ k.label }} · {{ k.fingerprint }}</z-select-item>
              }
            </z-select>
            <span class="text-xs font-normal text-muted-foreground">Add one in <a class="underline" routerLink="/admin/system-keys">system keys</a>.</span>
          </div>
          <label class="field"
            ><span>Price per credit · USD</span
            ><input z-input name="credit" [(ngModel)]="draft.credit_usd" inputmode="decimal" placeholder="0.0069" maxlength="20" autocomplete="off" />
            <span class="text-xs font-normal text-muted-foreground">What a TheirStack API credit costs you, such as $69 / 10,000.</span>
          </label>
          <label class="field"
            ><span>Postings per search</span
            ><input z-input name="max" type="number" min="1" max="100" step="1" [(ngModel)]="draft.max_results" />
            <span class="text-xs font-normal text-muted-foreground">The most one search returns, which caps what it can cost.</span>
          </label>
          <div class="flex flex-wrap items-center gap-3 md:col-span-3">
            <button z-button type="submit" [zDisabled]="busy()">{{ busy() ? 'Saving…' : 'Save job search' }}</button>
            <span class="text-sm text-muted-foreground" role="status">{{ notice() || status(saved) }}</span>
          </div>
        </form>
      } @else {
        <p class="text-sm" role="status">{{ loading() ? 'Loading job search settings…' : 'Job search settings are unavailable.' }}</p>
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
  draft = { key_id: '', credit_usd: '', max_results: 25 };
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

  protected status(s: Settings): string {
    return s.key_id && s.credit_usd ? 'Search watchers are available to plans that include them.' : 'Search watchers are off until a key and a price are set.';
  }

  async load(): Promise<void> {
    const identity = this.auth.identity(), seq = ++this.generation;
    const current = () => identity === this.auth.identity() && seq === this.generation;
    this.loading.set(true);
    this.error.set('');
    try {
      const [saved, keys] = await Promise.all([this.api.GET('/v1/admin/search-settings'), this.api.GET('/v1/admin/system-keys')]);
      if (!current()) return;
      this.keys.set(Api.ok(keys).filter((k) => k.kind === 'theirstack'));
      this.apply(Api.ok(saved));
    } catch (e) {
      if (current()) this.error.set(this.message(e));
    } finally {
      if (current()) this.loading.set(false);
    }
  }

  private apply(s: Settings): void {
    this.settings.set(s);
    this.draft = { key_id: s.key_id ?? '', credit_usd: trimUSD(s.credit_usd), max_results: s.max_results };
  }

  async save(): Promise<void> {
    const saved = this.settings();
    if (!saved || this.busy()) return;
    const identity = this.auth.identity(), seq = this.generation;
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
            credit_usd: this.draft.credit_usd.trim() || null,
            max_results: Math.max(1, Math.min(100, Math.trunc(Number(this.draft.max_results) || 25))),
          },
        }),
      );
      if (current()) {
        this.apply(value);
        this.notice.set('Job search saved.');
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
    if (error instanceof ApiError && error.fieldErrors.length) return error.fieldErrors.map((e) => e.message).join('; ');
    return error instanceof Error ? error.message : 'Could not save changes.';
  }
}
