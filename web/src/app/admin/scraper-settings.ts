import { ChangeDetectionStrategy, Component, effect, inject, signal, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';

type Settings = Schemas['ScraperSettings'];
type SystemKey = Schemas['SystemKey'];

// What the form edits. `key_id` is '' for no key, and rule providers are
// positions in the provider list, as the API stores them.
type Kind = Schemas['ScraperProvider']['kind'];
interface Provider { kind: Kind; base_url: string; key_id: string; credit_usd: string }
interface Rule { host: string; providers: number[] }

const NO_KEY = 'none';
const KIND_LABEL: Record<Kind, string> = { firecrawl: 'Firecrawl', crawl4ai: 'Crawl4AI', edge: 'Edge workers' };
const asKind = (value: string): Kind => (value === 'crawl4ai' || value === 'edge' ? value : 'firecrawl');
const single = (value: string | string[] | null): string => (Array.isArray(value) ? (value[0] ?? '') : (value ?? ''));

// Scraping: posting pages are read by the providers below in order, and the
// next one is tried when a provider fails. A site rule picks other providers
// (or another order) for a host and its subdomains; the first matching rule wins.
@Component({
  selector: 'app-scraper-settings',
  imports: [FormsModule, RouterLink, ZardButtonComponent, ZardInputComponent, ...ZardSelectImports],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="space-y-12">
      @if (error()) {
        <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">{{ error() }}</p>
      }
      @if (settings(); as saved) {
        <section aria-labelledby="providers-heading" class="space-y-5">
          <header class="flex flex-wrap items-start justify-between gap-3">
            <div class="space-y-1">
              <h2 id="providers-heading" class="text-lg font-semibold">Scraping providers</h2>
              <p class="max-w-2xl text-sm text-muted-foreground">
                Posting links are read by the first provider that can. When it fails, the next one is tried. Changes apply within seconds.
              </p>
            </div>
            <span class="status-pill" [class.is-dirty]="dirty()">{{ dirty() ? 'Unsaved changes' : 'Saved' }}</span>
          </header>
          @if (!saved.saved && !providers.length) {
            <p class="text-sm text-muted-foreground">No provider is configured, so posting links are not read.</p>
          }

          <ol class="space-y-3">
            @for (p of providers; track p; let i = $index; let first = $first; let last = $last) {
              <li class="space-y-3 rounded-lg border p-4">
                <div class="flex flex-wrap items-center justify-between gap-2">
                  <h3 class="text-sm font-medium">{{ i + 1 }}. {{ kindLabel(p.kind) }}</h3>
                  <div class="flex gap-1">
                    <button z-button zType="outline" zSize="sm" [attr.aria-label]="'Move provider ' + (i + 1) + ' up'" [zDisabled]="first || busy()" (click)="moveProvider(i, -1)">Up</button>
                    <button z-button zType="outline" zSize="sm" [attr.aria-label]="'Move provider ' + (i + 1) + ' down'" [zDisabled]="last || busy()" (click)="moveProvider(i, 1)">Down</button>
                    <button z-button zType="outline" zSize="sm" [attr.aria-label]="'Remove provider ' + (i + 1)" [zDisabled]="busy()" (click)="removeProvider(i)">Remove</button>
                  </div>
                </div>
                <div class="grid gap-4 md:grid-cols-3">
                  <div class="field"
                    ><span>Scraper</span>
                    <z-select [attr.aria-label]="'Scraper for provider ' + (i + 1)" [ngModel]="p.kind" (ngModelChange)="selectKind(p, single($event))" [zDisabled]="busy()">
                      <z-select-item zValue="firecrawl">Firecrawl</z-select-item>
                      <z-select-item zValue="crawl4ai">Crawl4AI</z-select-item>
                      <z-select-item zValue="edge">Edge workers</z-select-item>
                    </z-select>
                  </div>
                  @if (p.kind === 'edge') {
                    <p class="text-sm text-muted-foreground md:col-span-2">
                      Pages are read by your edge workers, on home machines whose addresses sites do not block. They need no address or key. Manage them below; a
                      worker opens only the sites listed there. If no worker is online for a site, the next provider is tried at once.
                    </p>
                  } @else {
                  <label class="field"
                    ><span>API address</span
                    ><input z-input [name]="'base-url-' + i" [(ngModel)]="p.base_url" (ngModelChange)="touch()" [placeholder]="p.kind === 'crawl4ai' ? 'http://crawl4ai:11235' : 'https://api.firecrawl.dev'" maxlength="2048" autocomplete="off" [required]="p.kind === 'crawl4ai'" />
                    @if (p.kind === 'crawl4ai') {
                      <span class="text-xs font-normal text-muted-foreground">Required: the address of your Crawl4AI server (port 11235). It has no hosted API.</span>
                    } @else {
                      <span class="text-xs font-normal text-muted-foreground">Leave empty for Firecrawl's hosted API. Self-hosted servers and Firecrawl Local use their own http(s) address.</span>
                    }
                  </label>
                  <div class="field"
                    ><span>{{ p.kind === 'crawl4ai' ? 'API token' : 'API key' }}</span>
                    <z-select [attr.aria-label]="(p.kind === 'crawl4ai' ? 'API token for provider ' : 'API key for provider ') + (i + 1)" zPlaceholder="No key" [ngModel]="p.key_id || noKey" (ngModelChange)="selectKey(p, single($event))" [zDisabled]="busy()">
                      <z-select-item [zValue]="noKey">No key</z-select-item>
                      @for (k of keysFor(p.kind); track k.id) {
                        <z-select-item [zValue]="k.id">{{ k.label }} · {{ k.fingerprint }}</z-select-item>
                      }
                    </z-select>
                    <span class="text-xs font-normal text-muted-foreground">
                      Keys are stored encrypted with the other <a class="underline" routerLink="/admin/system-keys">system keys</a>; add a {{ kindLabel(p.kind) }} key there.
                    </span>
                  </div>
                  <label class="field"
                    ><span>Price per credit · USD</span
                    ><input z-input [name]="'credit-' + i" [(ngModel)]="p.credit_usd" (ngModelChange)="touch()" inputmode="decimal" placeholder="Free" maxlength="20" autocomplete="off" />
                    <span class="text-xs font-normal text-muted-foreground">
                      {{ p.kind === 'firecrawl' ? 'Firecrawl charges 1 credit a page, 5 through its stealth proxy.' : 'Each page counts as 1 credit.' }}
                      Members pay for the credits their scrapes use, from their plan. Leave empty for a free provider.
                    </span>
                  </label>
                  }
                </div>
              </li>
            } @empty {
              <li class="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">No providers yet.</li>
            }
          </ol>
          <div class="flex flex-wrap gap-2">
            <button z-button zType="outline" [zDisabled]="busy() || providers.length >= maxProviders" (click)="addProvider('firecrawl')">Add Firecrawl provider</button>
            <button z-button zType="outline" [zDisabled]="busy() || providers.length >= maxProviders" (click)="addProvider('crawl4ai')">Add Crawl4AI provider</button>
            <button z-button zType="outline" [zDisabled]="busy() || providers.length >= maxProviders" (click)="addProvider('edge')">Add edge workers provider</button>
          </div>
        </section>

        <section aria-labelledby="rules-heading" class="space-y-5">
          <header class="space-y-1">
            <h2 id="rules-heading" class="text-lg font-semibold">Site rules</h2>
            <p class="max-w-2xl text-sm text-muted-foreground">
              A rule picks the providers, and their order, for one site and its subdomains: <span class="font-mono">linkedin.com</span> also covers
              <span class="font-mono">www.linkedin.com</span>. The first matching rule wins. Pages no rule matches use every provider above, in order.
            </p>
          </header>
          <ol class="space-y-3">
            @for (r of rules; track r; let i = $index; let first = $first; let last = $last) {
              <li class="space-y-3 rounded-lg border p-4">
                <div class="flex flex-wrap items-end justify-between gap-3">
                  <label class="field w-full max-w-sm"
                    ><span>Site {{ i + 1 }}</span
                    ><input z-input [name]="'rule-host-' + i" [(ngModel)]="r.host" (ngModelChange)="touch()" placeholder="linkedin.com" maxlength="253" autocomplete="off" autocapitalize="off" spellcheck="false" />
                  </label>
                  <div class="flex gap-1">
                    <button z-button zType="outline" zSize="sm" [attr.aria-label]="'Move rule ' + (i + 1) + ' up'" [zDisabled]="first || busy()" (click)="moveRule(i, -1)">Up</button>
                    <button z-button zType="outline" zSize="sm" [attr.aria-label]="'Move rule ' + (i + 1) + ' down'" [zDisabled]="last || busy()" (click)="moveRule(i, 1)">Down</button>
                    <button z-button zType="outline" zSize="sm" [attr.aria-label]="'Remove rule ' + (i + 1)" [zDisabled]="busy()" (click)="removeRule(i)">Remove</button>
                  </div>
                </div>
                <ol class="space-y-2">
                  @for (n of r.providers; track n; let j = $index; let jFirst = $first; let jLast = $last) {
                    <li class="flex flex-wrap items-center justify-between gap-2 text-sm">
                      <span>{{ j + 1 }}. {{ providerLabel(n) }}</span>
                      <span class="flex gap-1">
                        <button z-button zType="ghost" zSize="sm" [attr.aria-label]="'Try ' + providerLabel(n) + ' earlier for ' + (r.host || 'this site')" [zDisabled]="jFirst || busy()" (click)="moveRuleProvider(r, j, -1)">Up</button>
                        <button z-button zType="ghost" zSize="sm" [attr.aria-label]="'Try ' + providerLabel(n) + ' later for ' + (r.host || 'this site')" [zDisabled]="jLast || busy()" (click)="moveRuleProvider(r, j, 1)">Down</button>
                        <button z-button zType="ghost" zSize="sm" [attr.aria-label]="'Remove ' + providerLabel(n) + ' from ' + (r.host || 'this site')" [zDisabled]="busy()" (click)="removeRuleProvider(r, j)">Remove</button>
                      </span>
                    </li>
                  }
                </ol>
                @if (unused(r).length) {
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="text-xs text-muted-foreground">Add:</span>
                    @for (n of unused(r); track n) {
                      <button z-button zType="outline" zSize="sm" [zDisabled]="busy()" (click)="addRuleProvider(r, n)">{{ providerLabel(n) }}</button>
                    }
                  </div>
                }
              </li>
            } @empty {
              <li class="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">No site rules. Every page uses the providers above.</li>
            }
          </ol>
          <button z-button zType="outline" [zDisabled]="busy() || !providers.length || rules.length >= maxRules" (click)="addRule()">Add site rule</button>
        </section>

        <footer class="flex flex-wrap items-center gap-3 border-t pt-5">
          <button z-button [zDisabled]="!dirty() || busy() || loading()" (click)="save()">{{ busy() ? 'Saving…' : 'Save scraping settings' }}</button>
          @if (dirty()) {
            <button z-button zType="ghost" [zDisabled]="busy()" (click)="discard()">Discard changes</button>
          }
          <span class="text-sm text-muted-foreground" role="status">{{ notice() }}</span>
        </footer>
      } @else {
        <p class="text-sm" role="status">{{ loading() ? 'Loading scraping settings…' : 'Scraping settings are unavailable.' }}</p>
      }
    </div>
  `,
  styles: [
    `
      :host { display: block; }
      .status-pill { display: inline-flex; align-items: center; gap: .375rem; border-radius: 999px; border: 1px solid var(--border); padding: .125rem .625rem; font-size: .75rem; font-weight: 500; color: var(--muted-foreground); white-space: nowrap; }
      .status-pill::before { content: ''; width: .375rem; height: .375rem; border-radius: 999px; background: currentColor; }
      .status-pill.is-dirty { color: #b45309; border-color: #fcd34d; }
    `,
  ],
})
export class ScraperSettings {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  protected readonly single = single;
  protected readonly noKey = NO_KEY;
  protected readonly maxProviders = 10;
  protected readonly maxRules = 100;
  readonly settings = signal<Settings | null>(null);
  readonly scraperKeys = signal<SystemKey[]>([]);
  readonly error = signal('');
  readonly notice = signal('');
  readonly loading = signal(false);
  readonly busy = signal(false);
  // The drafts are edited in place; `edits` makes the dirty check re-run.
  providers: Provider[] = [];
  rules: Rule[] = [];
  private readonly edits = signal(0);
  private generation = 0;

  constructor() {
    effect(() => {
      this.auth.identity();
      untracked(() => {
        this.generation++;
        this.settings.set(null);
        this.scraperKeys.set([]);
        this.providers = [];
        this.rules = [];
        this.notice.set('');
        void this.load();
      });
    });
  }

  async load(): Promise<void> {
    const identity = this.auth.identity(), seq = ++this.generation;
    const current = () => identity === this.auth.identity() && seq === this.generation;
    this.loading.set(true);
    this.error.set('');
    try {
      const [saved, keys] = await Promise.all([
        this.api.GET('/v1/admin/scraper-settings'),
        this.api.GET('/v1/admin/system-keys'),
      ]);
      if (!current()) return;
      const value = Api.ok(saved);
      this.scraperKeys.set(Api.ok(keys).filter((k) => k.kind !== 'openrouter'));
      this.apply(value);
    } catch (e) {
      if (current()) this.error.set(this.message(e));
    } finally {
      if (current()) this.loading.set(false);
    }
  }

  private apply(saved: Settings): void {
    this.settings.set(saved);
    this.providers = saved.providers.map((p) => ({ kind: p.kind, base_url: p.base_url ?? '', key_id: p.key_id ?? '', credit_usd: p.credit_usd ?? '' }));
    this.rules = saved.rules.map((r) => ({ host: r.host, providers: [...r.providers] }));
    this.edits.update((n) => n + 1);
  }

  /** What the form would save. */
  private body(): Schemas['ScraperSettingsInput'] {
    return {
      providers: this.providers.map((p) => ({
        kind: p.kind,
        base_url: p.base_url.trim() || null,
        key_id: p.key_id || null,
        credit_usd: p.kind === 'edge' ? null : p.credit_usd.trim() || null,
      })),
      rules: this.rules.map((r) => ({ host: r.host.trim().toLowerCase(), providers: [...r.providers] })),
    };
  }

  dirty(): boolean {
    this.edits();
    const saved = this.settings();
    if (!saved) return false;
    const normalized = {
      providers: saved.providers.map((p) => ({ kind: p.kind, base_url: p.base_url || null, key_id: p.key_id || null, credit_usd: p.credit_usd || null })),
      rules: saved.rules,
    };
    return JSON.stringify(this.body()) !== JSON.stringify(normalized);
  }

  touch(): void {
    this.notice.set('');
    this.edits.update((n) => n + 1);
  }

  providerLabel(i: number): string {
    const p = this.providers[i];
    if (!p) return 'Removed provider';
    return `${i + 1}. ${KIND_LABEL[p.kind]}${p.base_url ? ' (' + this.hostOf(p.base_url) + ')' : ''}`;
  }
  kindLabel(kind: Kind): string {
    return KIND_LABEL[kind];
  }
  /** Only a key of the provider's own kind can be sent to it. */
  keysFor(kind: Kind): SystemKey[] {
    return this.scraperKeys().filter((k) => k.kind === kind);
  }
  selectKind(p: Provider, value: string): void {
    const kind = asKind(value);
    if (kind === p.kind) return;
    p.kind = kind;
    // A key of the other kind would be refused on save, and edge workers take neither.
    if (kind === 'edge') { p.base_url = ''; p.credit_usd = ''; }
    if (p.key_id && !this.keysFor(kind).some((k) => k.id === p.key_id)) p.key_id = '';
    this.touch();
  }
  private hostOf(url: string): string {
    try { return new URL(url).host; } catch { return url; }
  }

  selectKey(p: Provider, value: string): void {
    p.key_id = value === NO_KEY ? '' : value;
    this.touch();
  }

  addProvider(kind: Kind): void {
    const keys = this.keysFor(kind);
    this.providers.push({ kind, base_url: '', key_id: keys.length === 1 ? keys[0]!.id : '', credit_usd: '' });
    this.touch();
  }
  removeProvider(i: number): void {
    this.providers.splice(i, 1);
    // Rules name providers by position: drop this one, shift the later ones.
    for (const r of this.rules) r.providers = r.providers.filter((n) => n !== i).map((n) => (n > i ? n - 1 : n));
    this.touch();
  }
  moveProvider(i: number, by: -1 | 1): void {
    const j = i + by;
    if (j < 0 || j >= this.providers.length) return;
    [this.providers[i], this.providers[j]] = [this.providers[j]!, this.providers[i]!];
    for (const r of this.rules) r.providers = r.providers.map((n) => (n === i ? j : n === j ? i : n));
    this.touch();
  }

  addRule(): void {
    this.rules.push({ host: '', providers: this.providers.map((_, i) => i) });
    this.touch();
  }
  removeRule(i: number): void {
    this.rules.splice(i, 1);
    this.touch();
  }
  moveRule(i: number, by: -1 | 1): void {
    const j = i + by;
    if (j < 0 || j >= this.rules.length) return;
    [this.rules[i], this.rules[j]] = [this.rules[j]!, this.rules[i]!];
    this.touch();
  }
  unused(r: Rule): number[] {
    return this.providers.map((_, i) => i).filter((i) => !r.providers.includes(i));
  }
  addRuleProvider(r: Rule, n: number): void {
    r.providers.push(n);
    this.touch();
  }
  removeRuleProvider(r: Rule, j: number): void {
    r.providers.splice(j, 1);
    this.touch();
  }
  moveRuleProvider(r: Rule, j: number, by: -1 | 1): void {
    const k = j + by;
    if (k < 0 || k >= r.providers.length) return;
    [r.providers[j], r.providers[k]] = [r.providers[k]!, r.providers[j]!];
    this.touch();
  }

  discard(): void {
    const saved = this.settings();
    if (!saved) return;
    this.apply(saved);
    this.error.set('');
    this.notice.set('Changes discarded.');
  }

  async save(): Promise<void> {
    const saved = this.settings();
    if (!saved || !this.dirty() || this.busy()) return;
    const identity = this.auth.identity(), seq = this.generation;
    const current = () => identity === this.auth.identity() && seq === this.generation;
    this.busy.set(true);
    this.error.set('');
    this.notice.set('');
    try {
      const value = Api.ok(
        await this.api.PUT('/v1/admin/scraper-settings', {
          params: { header: { 'If-Match': `"${saved.version}"` } },
          body: this.body(),
        }),
      );
      if (current()) {
        this.apply(value);
        this.notice.set('Scraping settings saved. They apply to new requests within seconds.');
      }
    } catch (e) {
      if (current()) this.error.set(this.message(e));
    } finally {
      if (current()) this.busy.set(false);
    }
  }

  /** Field errors name "providers[0].base_url"; show them as "Provider 1 address". */
  private message(error: unknown): string {
    if (error instanceof ApiError && error.status === 412)
      return 'Someone else changed these settings. Refresh to see their version, then redo your changes.';
    if (error instanceof ApiError && error.fieldErrors.length) {
      return error.fieldErrors
        .map((e) => {
          const m = /^(providers|rules)\[(\d+)\](?:\.(\w+))?$/.exec(e.field);
          if (!m) return e.message;
          const what = m[1] === 'providers' ? `Provider ${+m[2]! + 1}` : `Site rule ${+m[2]! + 1}`;
          const part = ({ base_url: 'API address', kind: 'scraper', host: 'site', providers: 'providers', credit_usd: 'price per credit' } as Record<string, string>)[m[3] ?? ''];
          return `${what}${part ? ' ' + part : ''}: ${e.message}`;
        })
        .join('; ');
    }
    return error instanceof Error ? error.message : 'Could not save changes.';
  }
}
