import { ChangeDetectionStrategy, Component, effect, inject, signal, untracked } from '@angular/core';
import { DecimalPipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ZardButtonComponent } from '@/shared/components/button';
import { ZardComboboxImports } from '@/shared/components/combobox';
import { ZardSelectImports } from '@/shared/components/select';

type DecisionModel = Schemas['DecisionModel'];
type ExtractionModel = Schemas['ExtractionModel'];
type Effort = Schemas['ReasoningOptions']['efforts'][number];
type Settings = Schemas['ModelSettings'];

const EFFORT_LABELS: Record<Effort, string> = {
  max: 'Max',
  xhigh: 'Extra high',
  high: 'High',
  medium: 'Medium',
  low: 'Low',
  minimal: 'Minimal',
  off: 'Off',
};
// Share of the output cap OpenRouter reserves for reasoning at each effort.
const EFFORT_SHARE: Record<Effort, number> = { max: 0.95, xhigh: 0.95, high: 0.8, medium: 0.5, low: 0.2, minimal: 0.1, off: 0 };

// Searchable pickers receive a single id; multi-select values never occur here.
const single = (value: string | string[] | null): string => (Array.isArray(value) ? (value[0] ?? '') : (value ?? ''));
const byName = (a: {name: string}, b: {name: string}) => a.name.localeCompare(b.name);

// Each model section saves on its own. The request always carries the other
// section's saved values, because the server rewrites both on every PUT.
@Component({
 selector: 'app-model-settings', imports: [DecimalPipe, FormsModule, ZardButtonComponent, ...ZardComboboxImports, ...ZardSelectImports], changeDetection: ChangeDetectionStrategy.OnPush,
 template: `
 <div class="space-y-12">
   <div class="flex flex-wrap items-center justify-between gap-3">
     <p class="max-w-2xl text-sm text-muted-foreground">Each model saves separately. Unsaved changes stay here until you save or discard them.</p>
     <button z-button zType="outline" [zDisabled]="busy() || loading()" (click)="refreshCatalogue()">Refresh catalogue</button>
   </div>

   @if (error()) { <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">{{ error() }}</p> }

   @if (settings(); as saved) {
     <section aria-labelledby="summary-heading" class="space-y-6">
       <header class="flex flex-wrap items-start justify-between gap-3">
         <div class="space-y-1">
           <h2 id="summary-heading" class="text-lg font-semibold">Automatic summaries</h2>
           <p class="text-sm text-muted-foreground">Uses the active OpenRouter key and each member’s USD allowance.</p>
         </div>
         <span class="status-pill" [class.is-dirty]="summaryDirty()">{{ summaryDirty() ? 'Unsaved changes' : 'Saved' }}</span>
       </header>
       <p class="text-sm"><span class="text-muted-foreground">Live now: </span>{{ summaryLive() }}</p>

       <div class="field"><span class="field-label">Model</span>
         <z-combobox class="w-full max-w-xl" ariaLabel="Summary model" placeholder="Choose a model" searchPlaceholder="Search models" [ngModel]="summaryModel" (ngModelChange)="selectSummaryModel($event)" [zDisabled]="loading() || busy()">
           <z-combobox-input />
           <z-combobox-content>
             <z-combobox-empty>No models match your search.</z-combobox-empty>
             <z-combobox-list>
               @for (m of textModels(); track m.id) {
                 <z-combobox-item [zValue]="m.id" [zLabel]="m.name">
                   <span class="flex min-w-0 flex-col"><span class="truncate">{{ m.name }}</span><span class="truncate text-xs text-muted-foreground">{{ m.context_length | number }} context{{ m.reasoning ? ' · thinking' : '' }}</span></span>
                 </z-combobox-item>
               }
             </z-combobox-list>
           </z-combobox-content>
         </z-combobox>
         @if (selectedTextModel(); as m) { <p class="text-xs text-muted-foreground">Context {{ m.context_length | number }} · input {{ perMillion(m.pricing['prompt']) }} / 1M tokens · output {{ perMillion(m.pricing['completion']) }} / 1M tokens</p> }
       </div>

       <div class="field"><span class="field-label">Service tier</span>
         <z-select class="w-full max-w-xl" aria-label="Service tier" zPlaceholder="Choose a tier" [ngModel]="summaryTier" (ngModelChange)="selectTier(single($event))" [zDisabled]="!summaryModel || endpointLoading() || busy() || loading()">
           @for (tier of availableTiers(); track tier) { <z-select-item [zValue]="tier">{{ tierLabel(tier) }}</z-select-item> }
         </z-select>
         <span class="text-xs text-muted-foreground">Only tiers offered by this model are shown. Requests stay on the selected provider endpoint.</span>
       </div>

       <div class="field"><span class="field-label">Provider endpoint</span>
         <z-select class="w-full max-w-xl" aria-label="Provider endpoint" [zPlaceholder]="endpointLoading() ? 'Loading providers…' : 'Select a provider'" [ngModel]="summaryEndpoint" (ngModelChange)="selectEndpoint(single($event))" [zDisabled]="!summaryModel || endpointLoading() || busy() || loading()">
           @for (endpoint of tierEndpoints(); track endpoint.tag) { <z-select-item [zValue]="endpoint.tag">{{ endpoint.name }}</z-select-item> }
         </z-select>
         @if (summaryModel && !endpointLoading() && !endpointError() && !endpoints().length) { <span class="text-xs text-muted-foreground">No compatible providers are available for this model. Choose another model.</span> }
         @if (endpointError()) { <span role="alert" class="text-xs text-destructive">{{ endpointError() }}</span> <button z-button zType="link" class="w-fit px-0" (click)="loadEndpoints()">Retry loading providers</button> }
       </div>

       <div class="field"><span class="field-label">Thinking effort</span>
         <z-select class="w-full max-w-xl" aria-label="Thinking effort" zPlaceholder="Automatic" [ngModel]="summaryEffort" (ngModelChange)="selectEffort(single($event))" [zDisabled]="!effortChoices().length || busy() || loading()">
           @for (o of effortOptions(); track o.value) { <z-select-item [zValue]="o.value">{{ o.label }}</z-select-item> }
         </z-select>
         <span class="text-xs text-muted-foreground" [class.text-destructive]="effortUnavailable()">{{ effortHint() }}</span>
       </div>

       <div class="field"><span class="field-label">Summary rows</span>
         <z-select class="w-full max-w-xl" aria-label="Summary rows" [ngModel]="'' + summaryRows" (ngModelChange)="selectRows(single($event))" [zDisabled]="loading() || busy()">
           @for (rows of summaryRowChoices; track rows) { <z-select-item [zValue]="'' + rows">{{ rows }} {{ rows === 1 ? 'row' : 'rows' }}{{ rows === 2 ? ' (default)' : '' }}</z-select-item> }
         </z-select>
         <span class="text-xs text-muted-foreground">Lines the summary fills on the CV page. A summary that wraps to more or fewer goes back to the model with feedback, up to twice. Changing this regenerates automatic summaries.</span>
       </div>

       <footer class="flex flex-wrap items-center gap-3 border-t pt-5">
         <button z-button [zDisabled]="!canSaveSummary()" (click)="saveSummary()">{{ busy() ? 'Saving…' : 'Save summary settings' }}</button>
         @if (summaryDirty()) { <button z-button zType="ghost" [zDisabled]="busy()" (click)="discardSummary()">Discard changes</button> }
         <span class="text-sm" [class.text-muted-foreground]="!summaryBlocker()" [class.text-amber-700]="!!summaryBlocker()" role="status">{{ summaryBlocker() || summaryNotice() }}</span>
       </footer>
       @if (summaryError()) { <p role="alert" class="text-sm text-destructive">{{ summaryError() }}</p> }
     </section>

     <section aria-labelledby="decision-heading" class="space-y-6">
       <header class="flex flex-wrap items-start justify-between gap-3">
         <div class="space-y-1">
           <h2 id="decision-heading" class="text-lg font-semibold">Matching decisions</h2>
           <p class="text-sm text-muted-foreground">New runs use this model. Existing runs keep their selection. System One models do not use summary service tiers.</p>
         </div>
         <span class="status-pill" [class.is-dirty]="decisionDirty()">{{ decisionDirty() ? 'Unsaved changes' : 'Saved' }}</span>
       </header>
       <p class="text-sm"><span class="text-muted-foreground">Live now: </span>{{ decisionLive() }}</p>

       <div class="field"><span class="field-label">Decision model</span>
         <z-combobox class="w-full max-w-xl" ariaLabel="Decision model" placeholder="Not configured" searchPlaceholder="Search models" [ngModel]="selected" (ngModelChange)="selectDecisionModel(single($event))" [zDisabled]="loading() || busy()">
           <z-combobox-input />
           <z-combobox-content>
             <z-combobox-empty>No models match your search.</z-combobox-empty>
             <z-combobox-list>
               <z-combobox-item zValue="" zLabel="Not configured">Not configured</z-combobox-item>
               @for (m of models(); track m.id) { <z-combobox-item [zValue]="m.id" [zLabel]="m.name">{{ m.name }}</z-combobox-item> }
             </z-combobox-list>
           </z-combobox-content>
         </z-combobox>
         @if (selectedDecisionModel(); as m) {
           <span class="text-xs text-muted-foreground">Context {{ m.context_length | number }} · input {{ perMillion(m.pricing['prompt']) }} / 1M tokens · output {{ perMillion(m.pricing['completion']) }} / 1M tokens</span>
           @if (m.compatibility_reason) { <span class="text-xs text-amber-700">Calls wait: {{ m.compatibility_reason.replaceAll('_',' ') }}.</span> }
         }
       </div>

       <footer class="flex flex-wrap items-center gap-3 border-t pt-5">
         <button z-button [zDisabled]="!canSaveDecision()" (click)="saveDecision()">{{ busy() ? 'Saving…' : 'Save decision model' }}</button>
         @if (decisionDirty()) { <button z-button zType="ghost" [zDisabled]="busy()" (click)="discardDecision()">Discard changes</button> }
         <span class="text-sm text-muted-foreground" role="status">{{ decisionNotice() }}</span>
       </footer>
       @if (decisionError()) { <p role="alert" class="text-sm text-destructive">{{ decisionError() }}</p> }
     </section>

     <section aria-labelledby="extraction-heading" class="space-y-6">
       <header class="flex flex-wrap items-start justify-between gap-3">
         <div class="space-y-1">
           <h2 id="extraction-heading" class="text-lg font-semibold">Posting extraction</h2>
           <p class="text-sm text-muted-foreground">Reads the salary, work mode and other details a posting's page doesn't publish, from its scraped text. Off leaves the new job dialog with what the page and link say.</p>
         </div>
         <span class="status-pill" [class.is-dirty]="extractionDirty()">{{ extractionDirty() ? 'Unsaved changes' : 'Saved' }}</span>
       </header>
       <p class="text-sm"><span class="text-muted-foreground">Live now: </span>{{ extractionLive() }}</p>

       <div class="field"><span class="field-label">Extraction model</span>
         <z-combobox class="w-full max-w-xl" ariaLabel="Extraction model" placeholder="Off" searchPlaceholder="Search models" [ngModel]="extractionModel" (ngModelChange)="selectExtractionModel(single($event))" [zDisabled]="loading() || busy()">
           <z-combobox-input />
           <z-combobox-content>
             <z-combobox-empty>No models match your search.</z-combobox-empty>
             <z-combobox-list>
               <z-combobox-item zValue="" zLabel="Off">Off</z-combobox-item>
               @for (m of extractionModels(); track m.id) { <z-combobox-item [zValue]="m.id" [zLabel]="m.name" [zDisabled]="!!m.compatibility_reason">{{ m.name }}</z-combobox-item> }
             </z-combobox-list>
           </z-combobox-content>
         </z-combobox>
         @if (selectedExtractionModel(); as m) {
           <span class="text-xs text-muted-foreground">Input {{ perMillion(m.pricing['prompt']) }} / 1M tokens · {{ m.typed ? 'salaries read as numbers' : 'salaries read as text, then parsed' }}</span>
           @if (m.compatibility_reason) { <span class="text-xs text-amber-700">Unavailable: {{ m.compatibility_reason.replaceAll('_',' ') }}.</span> }
         }
       </div>

       <footer class="flex flex-wrap items-center gap-3 border-t pt-5">
         <button z-button [zDisabled]="!canSaveExtraction()" (click)="saveExtraction()">{{ busy() ? 'Saving…' : 'Save extraction model' }}</button>
         @if (extractionDirty()) { <button z-button zType="ghost" [zDisabled]="busy()" (click)="discardExtraction()">Discard changes</button> }
         <span class="text-sm text-muted-foreground" role="status">{{ extractionNotice() }}</span>
       </footer>
       @if (extractionError()) { <p role="alert" class="text-sm text-destructive">{{ extractionError() }}</p> }
     </section>
   } @else {
     <p class="text-sm" role="status">{{ loading() ? 'Loading model settings…' : 'Model settings are unavailable.' }}</p>
   }
 </div>`,
 styles: [`
 :host { display: block; }
 .field { display: grid; gap: .375rem; max-width: 44rem; }
 .field-label { font-size: .875rem; font-weight: 500; }
 .status-pill { display: inline-flex; align-items: center; gap: .375rem; border-radius: 999px; border: 1px solid var(--border); padding: .125rem .625rem; font-size: .75rem; font-weight: 500; color: var(--muted-foreground); white-space: nowrap; }
 .status-pill::before { content: ''; width: .375rem; height: .375rem; border-radius: 999px; background: currentColor; }
 .status-pill.is-dirty { color: #b45309; border-color: #fcd34d; }
 `],
})
export class ModelSettings {
 private readonly api = inject(Api).client;
 private readonly auth = inject(AuthService);
 protected readonly single = single;
 readonly endpoints = signal<{tag: string; name: string; service_tier: string}[]>([]);
 readonly endpointLoading = signal(false);
 readonly endpointError = signal('');
 private endpointGeneration = 0;
 readonly endpointMatches = (endpoint: {tag: string}) => endpoint.tag === this.summaryEndpoint;
 readonly settings = signal<Settings | null>(null);
 readonly textModels = signal<DecisionModel[]>([]);
 readonly models = signal<DecisionModel[]>([]);
 readonly error = signal(''); readonly loading = signal(false); readonly busy = signal(false);
 readonly summaryNotice = signal(''); readonly summaryError = signal('');
 readonly decisionNotice = signal(''); readonly decisionError = signal('');
 readonly extractionModels = signal<ExtractionModel[]>([]);
 readonly extractionNotice = signal(''); readonly extractionError = signal('');
 extractionModel = '';
 summaryTier = 'default';
 tierLabel(tier: string): string { return ({default: 'Standard · regular capacity', flex: 'Flex · lower price, lower priority', priority: 'Priority · faster processing, higher price', ultrafast: 'Ultrafast · highest speed and price'} as Record<string, string>)[tier] ?? tier; }
 availableTiers(): string[] { return [...new Set(this.endpoints().map(e => e.service_tier))]; }
 tierEndpoints() { return this.endpoints().filter(e => e.service_tier === this.summaryTier); }
 selectTier(tier: string): void { this.summaryTier = tier; const endpoints = this.tierEndpoints(); this.summaryEndpoint = endpoints.length === 1 ? endpoints[0]!.tag : ''; this.summaryNotice.set(''); }
 selectEndpoint(tag: string): void { this.summaryEndpoint = tag; this.summaryNotice.set(''); }
 readonly summaryRowChoices: Schemas['SummaryRows'][] = [1, 2, 3, 4];
 summaryRows: Schemas['SummaryRows'] = 2;
 summaryModel = ''; summaryEndpoint = '';
 // '' is automatic: optional thinking stays off and required thinking uses its lowest effort.
 summaryEffort: '' | Effort = '';
 selected = ''; private generation = 0;
 constructor() { effect(() => { this.auth.identity(); untracked(() => { this.generation++; this.settings.set(null); this.models.set([]); this.textModels.set([]); this.extractionModels.set([]); this.extractionModel = ''; this.selected = ''; this.summaryModel = ''; this.summaryEndpoint = ''; this.summaryEffort = ''; this.summaryRows = 2; ++this.endpointGeneration; this.endpoints.set([]); this.endpointError.set(''); this.endpointLoading.set(false); this.clearNotices(); void this.load(); }); }); }
 private clearNotices(): void { this.summaryNotice.set(''); this.summaryError.set(''); this.decisionNotice.set(''); this.decisionError.set(''); this.extractionNotice.set(''); this.extractionError.set(''); }
 private applySettings(saved: Settings): void {
   this.settings.set(saved); this.selected = saved.decision_model ?? ''; this.summaryModel = saved.summary_model ?? ''; this.summaryEndpoint = saved.summary_endpoint ?? ''; this.summaryEffort = saved.summary_reasoning_effort ?? ''; this.summaryRows = saved.summary_rows;
   this.extractionModel = saved.extraction_model ?? '';
 }
 private async fetchModels(current: () => boolean): Promise<void> {
   const textModels = Api.ok(await this.api.GET('/v1/admin/text-models')); if (!current()) return; this.textModels.set([...textModels].sort(byName));
   const models = Api.ok(await this.api.GET('/v1/admin/decision-models')); if (!current()) return; this.models.set([...models].sort(byName));
   const extraction = Api.ok(await this.api.GET('/v1/admin/extraction-models')); if (current()) this.extractionModels.set([...extraction].sort(byName));
 }
 async load(): Promise<void> {
   const identity = this.auth.identity(), seq = ++this.generation; this.loading.set(true); this.error.set(''); this.clearNotices();
   const current = () => identity === this.auth.identity() && seq === this.generation;
   try {
     const saved = Api.ok(await this.api.GET('/v1/admin/model-settings')); if (!current()) return;
     this.applySettings(saved);
     await this.loadEndpoints(); if (!current()) return;
     await this.fetchModels(current);
   } catch(e) { if (current()) this.error.set((e as Error).message); }
   finally { if (current()) this.loading.set(false); }
 }
 // Reloads the lists only, so unsaved drafts are kept.
 async refreshCatalogue(): Promise<void> {
   const identity = this.auth.identity(), seq = this.generation;
   const current = () => identity === this.auth.identity() && seq === this.generation;
   this.loading.set(true); this.error.set('');
   try {
     await this.fetchModels(current); if (!current()) return;
     await this.loadEndpoints();
   } catch(e) { if (current()) this.error.set((e as Error).message); }
   finally { if (current()) this.loading.set(false); }
 }
 selectedTextModel(): DecisionModel | undefined { return this.textModels().find(m => m.id === this.summaryModel); }
 selectedDecisionModel(): DecisionModel | undefined { return this.models().find(m => m.id === this.selected); }
 effortChoices(): Effort[] { return this.selectedTextModel()?.reasoning?.efforts ?? []; }
 effortOptions(): {value: '' | Effort; label: string}[] {
   return [{value: '', label: 'Automatic'}, ...this.effortChoices().map(value => ({value, label: EFFORT_LABELS[value]}))];
 }
 effortUnavailable(): boolean { return !!this.summaryEffort && !this.effortChoices().includes(this.summaryEffort); }
 effortHint(): string {
   const model = this.selectedTextModel();
   if (!model) return 'Choose a summary model to set its thinking effort.';
   if (!model.reasoning) return 'This model does not take a thinking setting.';
   if (!model.reasoning.efforts.length) return 'This model sets its own thinking effort.';
   if (this.effortUnavailable()) return 'The saved thinking effort is no longer offered by this model. Choose another effort before saving.';
   if (!this.summaryEffort) return 'Automatic keeps summaries short: optional thinking stays off, and required thinking uses its lowest effort.';
   if (this.summaryEffort === 'off') return 'Thinking stays off for summaries.';
   const share = Math.round(EFFORT_SHARE[this.summaryEffort] * 100);
   return `${EFFORT_LABELS[this.summaryEffort]} can spend up to about ${share}% of each answer's tokens thinking. Higher efforts cost more per summary.`;
 }
 perMillion(value: unknown): string {
   const usd = Number(value) * 1_000_000;
   if (!Number.isFinite(usd)) return '—';
   return `$${usd.toLocaleString('en-US', { maximumFractionDigits: usd < 1 ? 4 : 2 })}`;
 }
 selectRows(value: string): void { this.summaryRows = Number(value) as Schemas['SummaryRows']; this.summaryNotice.set(''); }
 selectEffort(value: string): void { this.summaryEffort = value as '' | Effort; this.summaryNotice.set(''); }
 selectDecisionModel(value: string): void { this.selected = value; this.decisionNotice.set(''); }
 selectSummaryModel(model: string | string[] | null): void {
   this.summaryModel = single(model); this.summaryEndpoint = ''; this.summaryEffort = ''; this.summaryNotice.set(''); void this.loadEndpoints();
 }
 async loadEndpoints(): Promise<void> {
   const model = this.summaryModel, identity = this.auth.identity(), seq = ++this.endpointGeneration;
   const current = () => seq === this.endpointGeneration && identity === this.auth.identity() && model === this.summaryModel;
   this.endpoints.set([]); this.endpointError.set(''); this.endpointLoading.set(!!model);
   if (!model) { this.summaryEndpoint = ''; return; }
   try {
     const endpoints = Api.ok(await this.api.GET('/v1/admin/text-model-endpoints', {params: {query: {model}}}));
     if (!current()) return;
     this.endpoints.set(endpoints);
     this.summaryTier = endpoints.find(e => e.tag === this.summaryEndpoint)?.service_tier ?? (endpoints.some(e => e.service_tier === 'default') ? 'default' : endpoints[0]?.service_tier ?? 'default');
     if (!endpoints.some(e => e.tag === this.summaryEndpoint)) this.summaryEndpoint = endpoints.length === 1 ? endpoints[0]!.tag : '';
   } catch (e) { if (current()) this.endpointError.set((e as Error).message); }
   finally { if (current()) this.endpointLoading.set(false); }
 }

 // Dirty state compares drafts with the last saved settings.
 summaryDirty(): boolean {
   const saved = this.settings(); if (!saved) return false;
   return this.summaryModel !== (saved.summary_model ?? '') || this.summaryEndpoint !== (saved.summary_endpoint ?? '') || this.summaryEffort !== (saved.summary_reasoning_effort ?? '') || this.summaryRows !== saved.summary_rows;
 }
 decisionDirty(): boolean { return this.selected !== (this.settings()?.decision_model ?? ''); }
 // Explains why Save is off. Empty when saving is allowed.
 summaryBlocker(): string {
   if (this.endpointLoading()) return 'Loading providers…';
   if (this.endpointError()) return 'Providers did not load. Retry before saving.';
   if (this.summaryModel && !this.endpoints().some(this.endpointMatches)) return 'Choose a provider endpoint before saving.';
   if (this.effortUnavailable()) return 'Choose a thinking effort this model accepts.';
   return '';
 }
 canSaveSummary(): boolean { return !!this.settings() && this.summaryDirty() && !this.summaryBlocker() && !this.busy() && !this.loading(); }
 canSaveDecision(): boolean { return !!this.settings() && this.decisionDirty() && !this.busy() && !this.loading(); }
 summaryLive(): string {
   const saved = this.settings(); if (!saved) return '';
   if (!saved.summary_model) return 'Not configured';
   const model = this.textModels().find(m => m.id === saved.summary_model)?.name ?? saved.summary_model;
   const endpoint = this.endpoints().find(e => e.tag === saved.summary_endpoint)?.name ?? saved.summary_endpoint ?? '';
   const effort = saved.summary_reasoning_effort ? EFFORT_LABELS[saved.summary_reasoning_effort] : 'Automatic';
   const route = endpoint ? `${model} via ${endpoint}` : model;
   return [route, `Thinking ${effort}`, `${saved.summary_rows} ${saved.summary_rows === 1 ? 'row' : 'rows'}`].join(' · ');
 }
 decisionLive(): string {
   const saved = this.settings(); if (!saved) return '';
   if (!saved.decision_model) return 'Not configured';
   return this.models().find(m => m.id === saved.decision_model)?.name ?? saved.decision_model;
 }
 discardSummary(): void {
   const saved = this.settings(); if (!saved) return;
   this.summaryModel = saved.summary_model ?? ''; this.summaryEndpoint = saved.summary_endpoint ?? ''; this.summaryEffort = saved.summary_reasoning_effort ?? ''; this.summaryRows = saved.summary_rows;
   this.summaryError.set(''); this.summaryNotice.set('Changes discarded.'); void this.loadEndpoints();
 }
 selectedExtractionModel(): ExtractionModel | undefined { return this.extractionModels().find(m => m.id === this.extractionModel); }
 selectExtractionModel(value: string): void { this.extractionModel = value; this.extractionNotice.set(''); }
 extractionDirty(): boolean { return this.extractionModel !== (this.settings()?.extraction_model ?? ''); }
 canSaveExtraction(): boolean { return !!this.settings() && this.extractionDirty() && !this.busy() && !this.loading() && !this.selectedExtractionModel()?.compatibility_reason; }
 extractionLive(): string {
   const saved = this.settings(); if (!saved) return '';
   if (!saved.extraction_model) return 'Off';
   return this.extractionModels().find(m => m.id === saved.extraction_model)?.name ?? saved.extraction_model;
 }
 discardExtraction(): void {
   this.extractionModel = this.settings()?.extraction_model ?? ''; this.extractionError.set(''); this.extractionNotice.set('Changes discarded.');
 }
 async saveExtraction(): Promise<void> {
   const saved = this.settings(); if (!saved || !this.canSaveExtraction()) return;
   const identity = this.auth.identity(), generation = this.generation; this.busy.set(true); this.clearNotices();
   const current = () => identity === this.auth.identity() && generation === this.generation;
   try {
     const value = Api.ok(await this.api.PUT('/v1/admin/model-settings', { params: { header: { 'If-Match': `"${saved.version}"` } }, body: { decision_model: saved.decision_model ?? null, summary_model: saved.summary_model ?? null, summary_endpoint: saved.summary_endpoint ?? null, summary_rows: saved.summary_rows, summary_reasoning_effort: saved.summary_reasoning_effort ?? null, extraction_model: this.extractionModel } }));
     if (current()) { this.settings.set(value); this.extractionNotice.set(this.extractionModel ? 'Extraction model saved.' : 'Extraction turned off.'); }
   } catch(e) { if (current()) this.extractionError.set((e as Error).message); }
   finally { if (current()) this.busy.set(false); }
 }
 discardDecision(): void {
   this.selected = this.settings()?.decision_model ?? ''; this.decisionError.set(''); this.decisionNotice.set('Changes discarded.');
 }

 async saveSummary(): Promise<void> {
   const saved = this.settings(); if (!saved || !this.canSaveSummary()) return;
   const identity = this.auth.identity(), generation = this.generation; this.busy.set(true); this.summaryError.set(''); this.summaryNotice.set(''); this.decisionNotice.set('');
   const current = () => identity === this.auth.identity() && generation === this.generation;
   try {
     const value = Api.ok(await this.api.PUT('/v1/admin/model-settings', { params: { header: { 'If-Match': `"${saved.version}"` } }, body: { decision_model: saved.decision_model ?? null, summary_model: this.summaryModel || null, summary_endpoint: this.summaryEndpoint || null, summary_rows: this.summaryRows, summary_reasoning_effort: this.summaryModel ? (this.summaryEffort || null) : null } }));
     if (current()) { this.settings.set(value); this.summaryNotice.set('Summary settings saved.'); }
   } catch(e) { if (current()) this.summaryError.set((e as Error).message); }
   finally { if (current()) this.busy.set(false); }
 }
 async saveDecision(): Promise<void> {
   const saved = this.settings(); if (!saved || !this.canSaveDecision()) return;
   const identity = this.auth.identity(), generation = this.generation; this.busy.set(true); this.decisionError.set(''); this.decisionNotice.set(''); this.summaryNotice.set('');
   const current = () => identity === this.auth.identity() && generation === this.generation;
   try {
     const value = Api.ok(await this.api.PUT('/v1/admin/model-settings', { params: { header: { 'If-Match': `"${saved.version}"` } }, body: { decision_model: this.selected || null, summary_model: saved.summary_model ?? null, summary_endpoint: saved.summary_endpoint ?? null, summary_rows: saved.summary_rows, summary_reasoning_effort: saved.summary_reasoning_effort ?? null } }));
     if (current()) { this.settings.set(value); this.decisionNotice.set('Decision model saved.'); }
   } catch(e) { if (current()) this.decisionError.set((e as Error).message); }
   finally { if (current()) this.busy.set(false); }
 }
}
