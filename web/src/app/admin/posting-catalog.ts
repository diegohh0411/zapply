import { ChangeDetectionStrategy, Component, computed, effect, inject, signal, untracked } from '@angular/core';
import { DecimalPipe, NgTemplateOutlet } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ZardButtonComponent } from '@/shared/components/button';
import { ZardComboboxImports } from '@/shared/components/combobox';
import { usageUsd, usagePercent, usd } from '../usage/format';

type Catalog = Schemas['PostingCatalog'];
type Stage = Schemas['PostingCatalogStage'];
type Label = Schemas['PostingLabel'];
type Settings = Schemas['ModelSettings'];

// A label field the model was less sure of than this shows in amber.
const LOW_CONFIDENCE = 0.6;
const single = (value: string | string[] | null): string => (Array.isArray(value) ? (value[0] ?? '') : (value ?? ''));
const byName = (a: { name: string }, b: { name: string }) => a.name.localeCompare(b.name);

// The posting catalog's two classifier models, where each stage stands, what
// the catalog's member has spent, and the newest labels to spot-check.
@Component({
  selector: 'app-posting-catalog',
  imports: [DecimalPipe, NgTemplateOutlet, FormsModule, RouterLink, ZardButtonComponent, ...ZardComboboxImports],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
  <div class="space-y-8">
    <header class="flex flex-wrap items-start justify-between gap-3">
      <div class="space-y-1">
        <h2 class="text-lg font-semibold">Posting catalog</h2>
        <p class="text-sm text-muted-foreground">Every posting the platform sees, stored once and labeled by two models. Classification is charged to the internal catalog member, not to any member's allowance. Members don't see labels yet.</p>
      </div>
      <button z-button zType="outline" [zDisabled]="loading() || busy()" (click)="load()">Refresh</button>
    </header>

    @if (error()) { <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">{{ error() }}</p> }

    @if (catalog(); as c) {
      <div class="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-4">
        <div class="stat"><span class="stat-label">Postings</span><span class="stat-value">{{ c.postings | number }}</span><span class="stat-meta">+{{ c.postings_24h | number }} past 24 hours</span></div>
        <div class="stat"><span class="stat-label">Fully labeled</span><span class="stat-value">{{ c.labeled | number }}</span><span class="stat-meta">{{ percent(c.labeled, c.postings) }}% · both stages answered</span></div>
        <div class="stat"><span class="stat-label">Waiting</span><span class="stat-value">{{ c.waiting | number }}</span><span class="stat-meta">{{ running() | number }} {{ running() === 1 ? 'call' : 'calls' }} running now</span></div>
        <div class="stat"><span class="stat-label">Failed</span><span class="stat-value" [class.text-destructive]="c.failed > 0">{{ c.failed | number }}</span><span class="stat-meta">a stage gave up after a retry</span></div>
      </div>
    }

    @if (settings()) {
      <section aria-label="Classifier models" class="space-y-4">
        <div class="grid gap-4 md:grid-cols-2">
          <div class="stage">
            <div class="flex items-center justify-between gap-3"><h3 class="text-sm font-semibold">Decision stage</h3><span class="status-pill" [class.is-on]="!!settings()?.posting_decision_model">{{ settings()?.posting_decision_model ? 'On' : 'Off' }}</span></div>
            <z-combobox class="w-full" ariaLabel="Posting decision model" placeholder="Off" searchPlaceholder="Search models" [ngModel]="decision" (ngModelChange)="decision = single($event); notice.set('')" [zDisabled]="loading() || busy()">
              <z-combobox-input />
              <z-combobox-content>
                <z-combobox-empty>No models match your search.</z-combobox-empty>
                <z-combobox-list>
                  <z-combobox-item zValue="" zLabel="Off">Off</z-combobox-item>
                  @for (m of decisionModels(); track m.id) { <z-combobox-item [zValue]="m.id" [zLabel]="m.name">{{ m.name }}</z-combobox-item> }
                </z-combobox-list>
              </z-combobox-content>
            </z-combobox>
            @if (decisionModel(); as m) {
              <span class="text-xs text-muted-foreground">Input {{ perMillion(m.pricing['prompt']) }} / 1M tokens · output {{ perMillion(m.pricing['completion']) }} / 1M tokens</span>
              @if (m.compatibility_reason) { <span class="text-xs text-amber-500">Calls wait: {{ m.compatibility_reason.replaceAll('_', ' ') }}.</span> }
            }
            @if (catalog(); as c) { <ng-container *ngTemplateOutlet="progress; context: { $implicit: c.stages.decision }" /> }
            <span class="text-xs text-muted-foreground">Seniority, areas, employment type, remote, minimum years</span>
          </div>

          <div class="stage">
            <div class="flex items-center justify-between gap-3"><h3 class="text-sm font-semibold">Extraction stage</h3><span class="status-pill" [class.is-on]="!!settings()?.posting_extraction_model">{{ settings()?.posting_extraction_model ? 'On' : 'Off' }}</span></div>
            <z-combobox class="w-full" ariaLabel="Posting extraction model" placeholder="Off" searchPlaceholder="Search models" [ngModel]="extraction" (ngModelChange)="extraction = single($event); notice.set('')" [zDisabled]="loading() || busy()">
              <z-combobox-input />
              <z-combobox-content>
                <z-combobox-empty>No models match your search.</z-combobox-empty>
                <z-combobox-list>
                  <z-combobox-item zValue="" zLabel="Off">Off</z-combobox-item>
                  @for (m of extractionModels(); track m.id) { <z-combobox-item [zValue]="m.id" [zLabel]="m.name" [zDisabled]="!!m.compatibility_reason">{{ m.name }}</z-combobox-item> }
                </z-combobox-list>
              </z-combobox-content>
            </z-combobox>
            @if (extractionModel(); as m) {
              <span class="text-xs text-muted-foreground">Input {{ perMillion(m.pricing['prompt']) }} / 1M tokens</span>
              @if (m.compatibility_reason) { <span class="text-xs text-amber-500">Unavailable: {{ m.compatibility_reason.replaceAll('_', ' ') }}.</span> }
            }
            @if (catalog(); as c) { <ng-container *ngTemplateOutlet="progress; context: { $implicit: c.stages.extraction }" /> }
            <span class="text-xs text-muted-foreground">Specialty, countries, languages</span>
          </div>
        </div>

        <footer class="flex flex-wrap items-center gap-3">
          <button z-button [zDisabled]="!canSave()" (click)="save()">{{ busy() ? 'Saving…' : 'Save models' }}</button>
          @if (dirty()) { <button z-button zType="ghost" [zDisabled]="busy()" (click)="discard()">Discard changes</button> }
          <span class="text-sm text-muted-foreground" role="status">{{ notice() || 'Both stages save together. Off leaves postings waiting; nothing is lost.' }}</span>
        </footer>
        @if (saveError()) { <p role="alert" class="text-sm text-destructive">{{ saveError() }}</p> }
      </section>
    } @else if (loading()) {
      <p class="text-sm text-muted-foreground" role="status">Loading the posting catalog…</p>
    }

    @if (catalog(); as c) {
      <section aria-labelledby="catalog-spend" class="stage">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <h3 id="catalog-spend" class="text-sm font-semibold">Spend · {{ c.usage.membership?.plan_name ?? 'No plan' }}</h3>
          <a class="text-xs underline-offset-4 hover:underline" routerLink="/admin/plans">Edit caps in Plans →</a>
        </div>
        <div class="grid gap-4 sm:grid-cols-2">
          <div class="grid gap-1.5">
            <div class="flex justify-between text-sm"><span class="text-muted-foreground">Past 7 days</span><span class="tabular-nums">{{ spent(c.usage.spent_7d_usd) }} / {{ money(c.usage.cap_7d_usd) }}</span></div>
            <div class="meter" role="progressbar" aria-label="Past 7 days" [attr.aria-valuenow]="meter(c.usage.spent_7d_usd, c.usage.reserved_usd, c.usage.cap_7d_usd)"><i [style.width.%]="meter(c.usage.spent_7d_usd, c.usage.reserved_usd, c.usage.cap_7d_usd)"></i></div>
          </div>
          <div class="grid gap-1.5">
            <div class="flex justify-between text-sm"><span class="text-muted-foreground">Past 30 days</span><span class="tabular-nums">{{ spent(c.usage.spent_30d_usd) }} / {{ money(c.usage.cap_30d_usd) }}</span></div>
            <div class="meter" role="progressbar" aria-label="Past 30 days" [attr.aria-valuenow]="meter(c.usage.spent_30d_usd, c.usage.reserved_usd, c.usage.cap_30d_usd)"><i [style.width.%]="meter(c.usage.spent_30d_usd, c.usage.reserved_usd, c.usage.cap_30d_usd)"></i></div>
          </div>
        </div>
        <span class="text-xs" [class.text-amber-500]="c.usage.blocked_reason === 'usage_limit'" [class.text-muted-foreground]="c.usage.blocked_reason !== 'usage_limit'">
          {{ c.usage.blocked_reason === 'usage_limit' ? 'At the cap now: classification waits and resumes as older spend leaves the window.' : 'At the cap, classification waits and resumes as older spend leaves the window.' }}
        </span>
      </section>

      <section aria-labelledby="catalog-labels" class="space-y-3">
        <div class="flex flex-wrap items-baseline justify-between gap-3">
          <h3 id="catalog-labels" class="text-sm font-semibold">Recent labels</h3>
          <span class="text-xs text-muted-foreground">Newest {{ c.recent.length }} · confidence under {{ lowConfidence }} in amber</span>
        </div>
        <div class="overflow-x-auto rounded-lg border">
          <table class="w-full min-w-[640px] text-left text-sm">
            <thead class="bg-muted">
              <tr><th class="p-3">Posting</th><th class="p-3">Seniority</th><th class="p-3">Areas</th><th class="p-3">Countries</th><th class="p-3">Remote</th></tr>
            </thead>
            <tbody>
              @for (l of c.recent; track l.id) {
                <tr class="border-t align-top">
                  <td class="p-3"><div class="font-medium">{{ l.title }}</div><div class="text-xs text-muted-foreground">{{ l.company }}{{ l.location ? ' · ' + l.location : '' }}</div>@if (l.specialty) {<div class="text-xs text-muted-foreground">{{ l.specialty }}</div>}</td>
                  <td class="p-3" [class.text-amber-500]="low(l, 'seniority')">@if (l.seniority) { {{ l.seniority }} <span class="conf">{{ conf(l, 'seniority') }}</span> } @else { <span class="text-muted-foreground">—</span> }</td>
                  <td class="p-3" [class.text-amber-500]="low(l, 'areas')">@for (a of l.areas; track a) { <span class="tag">{{ a.replaceAll('_', ' ') }}</span> } @empty { <span class="text-muted-foreground">—</span> } <span class="conf">{{ conf(l, 'areas') }}</span></td>
                  <td class="p-3" [class.text-amber-500]="low(l, 'countries')">@if (l.extraction_model) { @for (k of l.countries; track k) { <span class="tag">{{ k }}</span> } @empty { <span class="text-muted-foreground">none</span> } } @else { <span class="text-muted-foreground">not extracted</span> }</td>
                  <td class="p-3" [class.text-amber-500]="low(l, 'remote')">@if (l.remote === true || l.remote === false) { {{ l.remote ? 'yes' : 'no' }} <span class="conf">{{ conf(l, 'remote') }}</span> } @else { <span class="text-muted-foreground">—</span> }</td>
                </tr>
              } @empty {
                <tr><td colspan="5" class="p-6 text-center text-muted-foreground">No postings labeled yet. Choose a model for a stage to start.</td></tr>
              }
            </tbody>
          </table>
        </div>
      </section>
    }
  </div>

  <ng-template #progress let-s>
    <div class="bar" aria-hidden="true">
      <i class="done" [style.width.%]="share(s, s.done)"></i><i class="waiting" [style.width.%]="share(s, s.waiting + s.running)"></i><i class="failed" [style.width.%]="share(s, s.failed)"></i>
    </div>
    <span class="text-xs text-muted-foreground tabular-nums">{{ s.done | number }} done · {{ s.waiting | number }} waiting{{ s.running ? ' · ' + s.running + ' running' : '' }} · {{ s.failed | number }} failed</span>
  </ng-template>`,
  styles: [`
  :host { display: block; }
  .stat, .stage { display: flex; min-width: 0; flex-direction: column; gap: .625rem; border: 1px solid var(--border); border-radius: .75rem; background: var(--card); padding: 1rem; }
  .stat { gap: .25rem; }
  .stat-label { font-size: .75rem; color: var(--muted-foreground); }
  .stat-value { font-size: 1.375rem; font-weight: 600; font-variant-numeric: tabular-nums; }
  .stat-meta { font-size: .75rem; color: var(--muted-foreground); }
  .status-pill { display: inline-flex; align-items: center; gap: .375rem; border-radius: 999px; border: 1px solid var(--border); padding: .125rem .625rem; font-size: .75rem; font-weight: 500; color: var(--muted-foreground); }
  .status-pill::before { content: ''; width: .375rem; height: .375rem; border-radius: 999px; background: currentColor; }
  .status-pill.is-on { color: var(--chart-2); }
  .bar { display: flex; height: .5rem; overflow: hidden; border-radius: 999px; background: var(--muted); }
  .bar i { display: block; height: 100%; }
  .bar .done { background: var(--chart-2); }
  .bar .waiting { background: var(--muted-foreground); opacity: .6; }
  .bar .failed { background: var(--destructive); }
  .meter { height: .375rem; overflow: hidden; border-radius: 999px; background: var(--muted); }
  .meter i { display: block; height: 100%; background: var(--chart-2); }
  .tag { display: inline-block; margin: 0 .25rem .125rem 0; border-radius: .3rem; background: var(--muted); padding: 0 .375rem; font-size: .75rem; color: var(--foreground); }
  .conf { font-size: .7rem; font-variant-numeric: tabular-nums; opacity: .75; }
  `],
})
export class PostingCatalog {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  protected readonly single = single;
  protected readonly money = usd;
  protected readonly spent = usageUsd;
  protected readonly lowConfidence = LOW_CONFIDENCE;
  readonly catalog = signal<Catalog | null>(null);
  readonly settings = signal<Settings | null>(null);
  readonly decisionModels = signal<Schemas['DecisionModel'][]>([]);
  readonly extractionModels = signal<Schemas['ExtractionModel'][]>([]);
  readonly loading = signal(false);
  readonly busy = signal(false);
  readonly error = signal('');
  readonly saveError = signal('');
  readonly notice = signal('');
  // Open model calls across both stages.
  readonly running = computed(() => {
    const s = this.catalog()?.stages;
    return s ? s.decision.running + s.extraction.running : 0;
  });
  decision = '';
  extraction = '';
  private generation = 0;

  constructor() {
    effect(() => {
      this.auth.identity();
      untracked(() => {
        this.generation++;
        this.catalog.set(null);
        this.settings.set(null);
        this.decision = '';
        this.extraction = '';
        void this.load();
      });
    });
  }

  async load(): Promise<void> {
    const identity = this.auth.identity(),
      seq = ++this.generation;
    const current = () => identity === this.auth.identity() && seq === this.generation;
    this.loading.set(true);
    this.error.set('');
    try {
      const [catalog, settings] = await Promise.all([
        this.api.GET('/v1/admin/posting-catalog', { params: { query: { limit: 20 } } }),
        this.api.GET('/v1/admin/model-settings'),
      ]);
      if (!current()) return;
      this.catalog.set(Api.ok(catalog));
      const saved = Api.ok(settings);
      // A refresh keeps unsaved choices.
      if (!this.dirty()) {
        this.decision = saved.posting_decision_model ?? '';
        this.extraction = saved.posting_extraction_model ?? '';
      }
      this.settings.set(saved);
      const [decision, extraction] = await Promise.all([
        this.api.GET('/v1/admin/decision-models'),
        this.api.GET('/v1/admin/extraction-models'),
      ]);
      if (!current()) return;
      this.decisionModels.set([...Api.ok(decision)].sort(byName));
      this.extractionModels.set([...Api.ok(extraction)].sort(byName));
    } catch (e) {
      if (current()) this.error.set((e as Error).message);
    } finally {
      if (current()) this.loading.set(false);
    }
  }

  decisionModel() { return this.decisionModels().find((m) => m.id === this.decision); }
  extractionModel() { return this.extractionModels().find((m) => m.id === this.extraction); }
  dirty(): boolean {
    const saved = this.settings();
    return !!saved && (this.decision !== (saved.posting_decision_model ?? '') || this.extraction !== (saved.posting_extraction_model ?? ''));
  }
  canSave(): boolean {
    return this.dirty() && !this.busy() && !this.loading() && !this.extractionModel()?.compatibility_reason;
  }
  discard(): void {
    const saved = this.settings();
    this.decision = saved?.posting_decision_model ?? '';
    this.extraction = saved?.posting_extraction_model ?? '';
    this.saveError.set('');
    this.notice.set('Changes discarded.');
  }

  async save(): Promise<void> {
    const saved = this.settings();
    if (!saved || !this.canSave()) return;
    const identity = this.auth.identity(),
      generation = this.generation;
    const current = () => identity === this.auth.identity() && generation === this.generation;
    this.busy.set(true);
    this.saveError.set('');
    this.notice.set('');
    try {
      // The server rewrites the member-facing settings on every PUT, so they
      // go back as saved; an omitted extraction_model keeps its saved value.
      const value = Api.ok(
        await this.api.PUT('/v1/admin/model-settings', {
          params: { header: { 'If-Match': `"${saved.version}"` } },
          body: {
            decision_model: saved.decision_model ?? null,
            summary_model: saved.summary_model ?? null,
            summary_endpoint: saved.summary_endpoint ?? null,
            summary_rows: saved.summary_rows,
            summary_reasoning_effort: saved.summary_reasoning_effort ?? null,
            posting_decision_model: this.decision,
            posting_extraction_model: this.extraction,
          },
        }),
      );
      if (!current()) return;
      this.settings.set(value);
      this.notice.set('Catalog models saved.');
      void this.load();
    } catch (e) {
      if (current()) this.saveError.set((e as Error).message);
    } finally {
      if (current()) this.busy.set(false);
    }
  }

  percent(part: number, whole: number): number { return whole ? Math.round((part / whole) * 100) : 0; }
  share(s: Stage, n: number): number {
    const total = s.done + s.waiting + s.running + s.failed;
    return total ? (n / total) * 100 : 0;
  }
  meter(spent: string, reserved: string, cap: string): number { return usagePercent(spent, reserved, cap); }
  conf(l: Label, field: string): string {
    const c = l.confidence[field];
    return c === undefined ? '' : c.toFixed(2).replace(/^0/, '');
  }
  low(l: Label, field: string): boolean {
    const c = l.confidence[field];
    return c !== undefined && c < LOW_CONFIDENCE;
  }
  perMillion(value: unknown): string {
    const n = Number(value) * 1_000_000;
    if (!Number.isFinite(n)) return '—';
    return `$${n.toLocaleString('en-US', { maximumFractionDigits: n < 1 ? 4 : 2 })}`;
  }
}
