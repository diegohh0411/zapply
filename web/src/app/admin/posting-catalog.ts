import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { DecimalPipe, NgTemplateOutlet } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { injectMutation, injectQuery, QueryClient } from '@tanstack/angular-query';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ZardBadgeComponent } from '@/shared/components/badge';
import { ZardButtonComponent } from '@/shared/components/button';
import { ZardCardImports } from '@/shared/components/card';
import { ZardComboboxImports } from '@/shared/components/combobox';
import { ZardProgressComponent } from '@/shared/components/progress';
import { ZardTableImports } from '@/shared/components/table';
import { usageUsd, usagePercent, usd } from '../usage/format';

type Stage = Schemas['PostingCatalogStage'];
type Label = Schemas['PostingLabel'];
type Settings = Schemas['ModelSettings'];

// A label field the model was less sure of than this shows in amber.
const LOW_CONFIDENCE = 0.6;
const RECENT = 20;
const single = (value: string | string[] | null): string => (Array.isArray(value) ? (value[0] ?? '') : (value ?? ''));
const byName = (a: { name: string }, b: { name: string }) => a.name.localeCompare(b.name);

const catalogKey = (identity: string | null) => ['admin-posting-catalog', identity] as const;
const settingsKey = (identity: string | null) => ['admin-model-settings', identity] as const;
const decisionModelsKey = (identity: string | null) => ['admin-decision-models', identity] as const;
const extractionModelsKey = (identity: string | null) => ['admin-extraction-models', identity] as const;

// The posting catalog's two classifier models, where each stage stands, what
// the catalog's member has spent, and the newest labels to spot-check.
@Component({
  selector: 'app-posting-catalog',
  imports: [
    DecimalPipe, NgTemplateOutlet, FormsModule, RouterLink, ZardBadgeComponent, ZardButtonComponent, ZardProgressComponent,
    ...ZardCardImports, ...ZardComboboxImports, ...ZardTableImports,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
  <div class="space-y-8">
    <header class="flex flex-wrap items-start justify-between gap-3">
      <div class="space-y-1">
        <h2 class="text-lg font-semibold">Posting catalog</h2>
        <p class="text-sm text-muted-foreground">Every posting the platform sees, stored once and labeled by two models. Classification is charged to the internal catalog member, not to any member's allowance. Members don't see labels yet.</p>
      </div>
      <button z-button zType="outline" [zDisabled]="fetching() || saving()" (click)="refresh()">{{ fetching() ? 'Refreshing…' : 'Refresh' }}</button>
    </header>

    @if (error()) { <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">{{ error() }}</p> }

    @if (catalog(); as c) {
      <div class="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-4">
        <z-card zSize="sm"><z-card-content class="stat"><span class="stat-label">Postings</span><span class="stat-value">{{ c.postings | number }}</span><span class="stat-meta">+{{ c.postings_24h | number }} past 24 hours</span></z-card-content></z-card>
        <z-card zSize="sm"><z-card-content class="stat"><span class="stat-label">Fully labeled</span><span class="stat-value">{{ c.labeled | number }}</span><span class="stat-meta">{{ percent(c.labeled, c.postings) }}% · both stages answered</span></z-card-content></z-card>
        <z-card zSize="sm"><z-card-content class="stat"><span class="stat-label">Waiting</span><span class="stat-value">{{ c.waiting | number }}</span><span class="stat-meta">{{ running() | number }} {{ running() === 1 ? 'call' : 'calls' }} running now</span></z-card-content></z-card>
        <z-card zSize="sm"><z-card-content class="stat"><span class="stat-label">Failed</span><span class="stat-value" [class.text-destructive]="c.failed > 0">{{ c.failed | number }}</span><span class="stat-meta">a stage gave up after a retry</span></z-card-content></z-card>
      </div>
    }

    @if (settings(); as saved) {
      <section aria-label="Classifier models" class="space-y-4">
        <div class="grid gap-4 md:grid-cols-2">
          <z-card>
            <z-card-header>
              <z-card-title zTitle="Decision stage" />
              <z-card-description zDescription="Seniority, areas, employment type, remote, minimum years" />
              <z-card-action><z-badge [zType]="saved.posting_decision_model ? 'secondary' : 'outline'">{{ saved.posting_decision_model ? 'On' : 'Off' }}</z-badge></z-card-action>
            </z-card-header>
            <z-card-content class="grid gap-2.5">
              <z-combobox class="w-full" ariaLabel="Posting decision model" placeholder="Off" searchPlaceholder="Search models" [ngModel]="decision()" (ngModelChange)="pickDecision(single($event))" [zDisabled]="saving()">
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
            </z-card-content>
          </z-card>

          <z-card>
            <z-card-header>
              <z-card-title zTitle="Extraction stage" />
              <z-card-description zDescription="Specialty, countries, languages" />
              <z-card-action><z-badge [zType]="saved.posting_extraction_model ? 'secondary' : 'outline'">{{ saved.posting_extraction_model ? 'On' : 'Off' }}</z-badge></z-card-action>
            </z-card-header>
            <z-card-content class="grid gap-2.5">
              <z-combobox class="w-full" ariaLabel="Posting extraction model" placeholder="Off" searchPlaceholder="Search models" [ngModel]="extraction()" (ngModelChange)="pickExtraction(single($event))" [zDisabled]="saving()">
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
            </z-card-content>
          </z-card>
        </div>

        <footer class="flex flex-wrap items-center gap-3">
          <button z-button [zDisabled]="!canSave()" (click)="save()">{{ saving() ? 'Saving…' : 'Save models' }}</button>
          @if (dirty()) { <button z-button zType="ghost" [zDisabled]="saving()" (click)="discard()">Discard changes</button> }
          <span class="text-sm text-muted-foreground" role="status">{{ notice() || 'Both stages save together. Off leaves postings waiting; nothing is lost.' }}</span>
        </footer>
        @if (saveError()) { <p role="alert" class="text-sm text-destructive">{{ saveError() }}</p> }
      </section>
    } @else if (fetching()) {
      <p class="text-sm text-muted-foreground" role="status">Loading the posting catalog…</p>
    }

    @if (catalog(); as c) {
      <z-card>
        <z-card-header>
          <z-card-title [zTitle]="'Spend · ' + (c.usage.membership?.plan_name ?? 'No plan')" />
          <z-card-description [zDescription]="c.usage.blocked_reason === 'usage_limit' ? 'At the cap now: classification waits and resumes as older spend leaves the window.' : 'At the cap, classification waits and resumes as older spend leaves the window.'" />
          <z-card-action><a z-button zType="link" class="h-auto px-0 text-xs" routerLink="/admin/plans">Edit caps in Plans →</a></z-card-action>
        </z-card-header>
        <z-card-content class="grid gap-4 sm:grid-cols-2">
          <div class="grid gap-1.5">
            <div class="flex justify-between text-sm"><span class="text-muted-foreground">Past 7 days</span><span class="tabular-nums">{{ spent(c.usage.spent_7d_usd) }} / {{ money(c.usage.cap_7d_usd) }}</span></div>
            <z-progress aria-label="Spend, past 7 days" class="h-1.5" [value]="meter(c.usage.spent_7d_usd, c.usage.reserved_usd, c.usage.cap_7d_usd)" />
          </div>
          <div class="grid gap-1.5">
            <div class="flex justify-between text-sm"><span class="text-muted-foreground">Past 30 days</span><span class="tabular-nums">{{ spent(c.usage.spent_30d_usd) }} / {{ money(c.usage.cap_30d_usd) }}</span></div>
            <z-progress aria-label="Spend, past 30 days" class="h-1.5" [value]="meter(c.usage.spent_30d_usd, c.usage.reserved_usd, c.usage.cap_30d_usd)" />
          </div>
        </z-card-content>
      </z-card>

      <section aria-labelledby="catalog-labels" class="space-y-3">
        <div class="flex flex-wrap items-baseline justify-between gap-3">
          <h3 id="catalog-labels" class="text-sm font-semibold">Recent labels</h3>
          <span class="text-xs text-muted-foreground">Newest {{ c.recent.length }} · confidence under {{ lowConfidence }} in amber</span>
        </div>
        <div class="overflow-x-auto rounded-lg border">
          <table z-table class="min-w-[640px]">
            <thead z-table-header>
              <tr z-table-row><th z-table-head>Posting</th><th z-table-head>Seniority</th><th z-table-head>Areas</th><th z-table-head>Countries</th><th z-table-head>Remote</th></tr>
            </thead>
            <tbody z-table-body>
              @for (l of c.recent; track l.id) {
                <tr z-table-row class="align-top">
                  <td z-table-cell class="whitespace-normal"><div class="font-medium">{{ l.title }}</div><div class="text-xs text-muted-foreground">{{ l.company }}{{ l.location ? ' · ' + l.location : '' }}</div>@if (l.specialty) {<div class="text-xs text-muted-foreground">{{ l.specialty }}</div>}</td>
                  <td z-table-cell [class.text-amber-500]="low(l, 'seniority')">@if (l.seniority) { {{ l.seniority }} <span class="conf">{{ conf(l, 'seniority') }}</span> } @else { <span class="text-muted-foreground">—</span> }</td>
                  <td z-table-cell [class.text-amber-500]="low(l, 'areas')"><div class="flex flex-wrap items-center gap-1">@for (a of l.areas; track a) { <z-badge zType="secondary">{{ a.replaceAll('_', ' ') }}</z-badge> } @empty { <span class="text-muted-foreground">—</span> } <span class="conf">{{ conf(l, 'areas') }}</span></div></td>
                  <td z-table-cell [class.text-amber-500]="low(l, 'countries')">@if (l.extraction_model) { <div class="flex flex-wrap gap-1">@for (k of l.countries; track k) { <z-badge zType="outline">{{ k }}</z-badge> } @empty { <span class="text-muted-foreground">none</span> }</div> } @else { <span class="text-muted-foreground">not extracted</span> }</td>
                  <td z-table-cell [class.text-amber-500]="low(l, 'remote')">@if (l.remote === true || l.remote === false) { {{ l.remote ? 'yes' : 'no' }} <span class="conf">{{ conf(l, 'remote') }}</span> } @else { <span class="text-muted-foreground">—</span> }</td>
                </tr>
              } @empty {
                <tr z-table-row><td z-table-cell colspan="5" class="p-6 text-center text-muted-foreground">No postings labeled yet. Choose a model for a stage to start.</td></tr>
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
  .stat { display: flex; flex-direction: column; gap: .25rem; }
  .stat-label { font-size: .75rem; color: var(--muted-foreground); }
  .stat-value { font-size: 1.375rem; font-weight: 600; font-variant-numeric: tabular-nums; }
  .stat-meta { font-size: .75rem; color: var(--muted-foreground); }
  /* One bar split three ways; Zard's progress shows a single value. */
  .bar { display: flex; height: .5rem; overflow: hidden; border-radius: 999px; background: var(--muted); }
  .bar i { display: block; height: 100%; }
  .bar .done { background: var(--chart-2); }
  .bar .waiting { background: var(--muted-foreground); opacity: .6; }
  .bar .failed { background: var(--destructive); }
  .conf { font-size: .7rem; font-variant-numeric: tabular-nums; opacity: .75; }
  `],
})
export class PostingCatalog {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  private readonly queries = inject(QueryClient);
  protected readonly single = single;
  protected readonly money = usd;
  protected readonly spent = usageUsd;
  protected readonly lowConfidence = LOW_CONFIDENCE;

  // Cached per identity: coming back shows the last state at once and
  // refreshes behind it. The counts move while the classifier runs, so they
  // also refresh every 30 seconds while the tab is open.
  private readonly catalogQuery = injectQuery(() => ({
    queryKey: catalogKey(this.auth.identity()),
    enabled: !!this.auth.identity(),
    refetchInterval: 30_000,
    queryFn: async () => Api.ok(await this.api.GET('/v1/admin/posting-catalog', { params: { query: { limit: RECENT } } })),
  }));
  private readonly settingsQuery = injectQuery(() => ({
    queryKey: settingsKey(this.auth.identity()),
    enabled: !!this.auth.identity(),
    queryFn: async () => Api.ok(await this.api.GET('/v1/admin/model-settings')),
  }));
  private readonly decisionModelsQuery = injectQuery(() => ({
    queryKey: decisionModelsKey(this.auth.identity()),
    enabled: !!this.auth.identity(),
    staleTime: 5 * 60_000,
    queryFn: async () => [...Api.ok(await this.api.GET('/v1/admin/decision-models'))].sort(byName),
  }));
  private readonly extractionModelsQuery = injectQuery(() => ({
    queryKey: extractionModelsKey(this.auth.identity()),
    enabled: !!this.auth.identity(),
    staleTime: 5 * 60_000,
    queryFn: async () => [...Api.ok(await this.api.GET('/v1/admin/extraction-models'))].sort(byName),
  }));
  // The server rewrites the member-facing settings on every PUT, so they go
  // back as saved; an omitted extraction_model keeps its saved value.
  private readonly saveMutation = injectMutation(() => ({
    mutationFn: async ({ saved, decision, extraction }: { saved: Settings; decision: string; extraction: string }) =>
      Api.ok(
        await this.api.PUT('/v1/admin/model-settings', {
          params: { header: { 'If-Match': `"${saved.version}"` } },
          body: {
            decision_model: saved.decision_model ?? null,
            summary_model: saved.summary_model ?? null,
            summary_endpoint: saved.summary_endpoint ?? null,
            summary_rows: saved.summary_rows,
            summary_reasoning_effort: saved.summary_reasoning_effort ?? null,
            posting_decision_model: decision,
            posting_extraction_model: extraction,
          },
        }),
      ),
  }));

  readonly catalog = computed(() => this.catalogQuery.data() ?? null);
  readonly settings = computed(() => this.settingsQuery.data() ?? null);
  readonly decisionModels = computed(() => this.decisionModelsQuery.data() ?? []);
  readonly extractionModels = computed(() => this.extractionModelsQuery.data() ?? []);
  readonly fetching = computed(() => this.catalogQuery.isFetching() || this.settingsQuery.isFetching());
  readonly saving = computed(() => this.saveMutation.isPending());
  readonly error = computed(() => {
    const e = this.catalogQuery.error() ?? this.settingsQuery.error() ?? this.decisionModelsQuery.error() ?? this.extractionModelsQuery.error();
    return e ? e.message : '';
  });
  readonly saveError = signal('');
  readonly notice = signal('');
  // Unsaved choices; null follows the saved model.
  private readonly decisionDraft = signal<string | null>(null);
  private readonly extractionDraft = signal<string | null>(null);
  readonly decision = computed(() => this.decisionDraft() ?? this.settings()?.posting_decision_model ?? '');
  readonly extraction = computed(() => this.extractionDraft() ?? this.settings()?.posting_extraction_model ?? '');
  readonly decisionModel = computed(() => this.decisionModels().find((m) => m.id === this.decision()));
  readonly extractionModel = computed(() => this.extractionModels().find((m) => m.id === this.extraction()));
  readonly dirty = computed(() => {
    const saved = this.settings();
    return !!saved && (this.decision() !== (saved.posting_decision_model ?? '') || this.extraction() !== (saved.posting_extraction_model ?? ''));
  });
  readonly canSave = computed(() => this.dirty() && !this.saving() && !this.extractionModel()?.compatibility_reason);
  // Open model calls across both stages.
  readonly running = computed(() => {
    const s = this.catalog()?.stages;
    return s ? s.decision.running + s.extraction.running : 0;
  });

  pickDecision(id: string): void { this.decisionDraft.set(id); this.notice.set(''); }
  pickExtraction(id: string): void { this.extractionDraft.set(id); this.notice.set(''); }
  discard(): void {
    this.decisionDraft.set(null);
    this.extractionDraft.set(null);
    this.saveError.set('');
    this.notice.set('Changes discarded.');
  }
  refresh(): void {
    const identity = this.auth.identity();
    void this.queries.invalidateQueries({ queryKey: catalogKey(identity) });
    void this.queries.invalidateQueries({ queryKey: settingsKey(identity) });
  }

  async save(): Promise<void> {
    const saved = this.settings();
    if (!saved || !this.canSave()) return;
    const identity = this.auth.identity();
    this.saveError.set('');
    this.notice.set('');
    try {
      const value = await this.saveMutation.mutateAsync({ saved, decision: this.decision(), extraction: this.extraction() });
      if (identity !== this.auth.identity()) return;
      this.queries.setQueryData(settingsKey(identity), value);
      this.decisionDraft.set(null);
      this.extractionDraft.set(null);
      this.notice.set('Catalog models saved.');
      void this.queries.invalidateQueries({ queryKey: catalogKey(identity) });
    } catch (e) {
      if (identity === this.auth.identity()) this.saveError.set((e as Error).message);
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
