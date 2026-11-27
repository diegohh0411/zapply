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
type Balance = Schemas['SearchBalance'];
type Plan = { credits: number; usd: number };

const NO_KEY = 'none';
const single = (value: string | string[] | null): string => (Array.isArray(value) ? (value[0] ?? '') : (value ?? ''));
/** "0.006900000000" → "0.0069" */
const trimUSD = (v: string | null | undefined): string => (v ? v.replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '') : '');

/**
 * TheirStack's published monthly API plans (theirstack.com/en/pricing, November
 * 2026). Its API has no pricing route, so the list lives here; "Custom" covers
 * yearly, negotiated or changed prices.
 */
export const THEIRSTACK_PLANS: readonly Plan[] = [
  { credits: 1_500, usd: 19 },
  { credits: 5_000, usd: 45 },
  { credits: 10_000, usd: 69 },
  { credits: 20_000, usd: 85 },
  { credits: 50_000, usd: 155 },
  { credits: 100_000, usd: 225 },
  { credits: 200_000, usd: 429 },
  { credits: 500_000, usd: 799 },
  { credits: 1_000_000, usd: 1_399 },
  { credits: 2_000_000, usd: 2_300 },
  { credits: 5_000_000, usd: 5_500 },
];
const CUSTOM = 'custom';
/** A plan's price per credit, as the server stores it (at most 12 decimals). */
export const perCredit = (p: Plan): string => trimUSD((p.usd / p.credits).toFixed(12));
const planLabel = (p: Plan): string => `${p.credits.toLocaleString('en-US')} credits · $${p.usd.toLocaleString('en-US')}/month`;

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
          TheirStack supplies the postings behind job alerts, across every company, including those that post only on LinkedIn. Every 30 minutes it
          is asked for new postings in the countries members accept and for remote ones; each run returns at most the postings below, at one API
          credit each, paid by the platform.
        </p>
      </header>
      @if (balance(); as b) {
        @if (b.configured) {
          <p class="text-sm" role="status" [class.text-destructive]="!!b.error || low(b)">
            @if (b.error) {
              Couldn't read the TheirStack balance: {{ b.error }}.
            } @else {
              TheirStack balance: <span class="font-medium tabular-nums">{{ count(b.credits) }}</span> API credits left,
              {{ count(b.used) }} used this billing cycle{{ b.expires_at ? '; the earliest expire ' + day(b.expires_at) : '' }}.
              @if (low(b)) {
                That's under a day of hourly searches; top up in TheirStack.
              }
            }
          </p>
        }
      }
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
          <div class="field">
            <span>TheirStack plan</span>
            <z-select aria-label="TheirStack plan" [ngModel]="plan()" (ngModelChange)="choosePlan(single($event))" [zDisabled]="busy()">
              @for (p of plans; track p.credits) {
                <z-select-item [zValue]="'' + p.credits">{{ label(p) }}</z-select-item>
              }
              <z-select-item [zValue]="custom">Custom price</z-select-item>
            </z-select>
            @if (plan() === custom) {
              <input z-input name="credit" aria-label="Price per credit in USD" [(ngModel)]="draft.credit_usd" inputmode="decimal" placeholder="0.0069" maxlength="20" autocomplete="off" />
              <span class="text-xs font-normal text-muted-foreground">What one API credit costs you in USD, for yearly or negotiated prices.</span>
            } @else {
              <span class="text-xs font-normal text-muted-foreground">\${{ draft.credit_usd }} per credit, from TheirStack's published prices.</span>
            }
          </div>
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
  readonly balance = signal<Balance | null>(null);
  /** The chosen plan's credits, or "custom". */
  readonly plan = signal(CUSTOM);
  protected readonly plans = THEIRSTACK_PLANS;
  protected readonly custom = CUSTOM;
  protected readonly label = planLabel;
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

  protected choosePlan(value: string): void {
    this.notice.set('');
    this.plan.set(value);
    const p = THEIRSTACK_PLANS.find((x) => '' + x.credits === value);
    if (p) this.draft.credit_usd = perCredit(p);
  }

  protected count(n: number | null | undefined): string {
    return (n ?? 0).toLocaleString('en-US');
  }

  protected day(iso: string): string {
    return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
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
    return s.key_id ? 'Job alerts are supplied through TheirStack.' : 'Job alerts get no new postings until a key is set.';
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
      void this.loadBalance(current);
    } catch (e) {
      if (current()) this.error.set(this.message(e));
    } finally {
      if (current()) this.loading.set(false);
    }
  }

  private apply(s: Settings): void {
    this.settings.set(s);
    this.draft = { key_id: s.key_id ?? '', credit_usd: trimUSD(s.credit_usd), max_results: s.max_results };
    const match = THEIRSTACK_PLANS.find((p) => perCredit(p) === this.draft.credit_usd);
    this.plan.set(match ? '' + match.credits : CUSTOM);
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
    if (error instanceof ApiError && error.fieldErrors.length) return error.fieldErrors.map((e) => e.message).join('; ');
    return error instanceof Error ? error.message : 'Could not save changes.';
  }
}
