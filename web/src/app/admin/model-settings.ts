import { ChangeDetectionStrategy, Component, effect, inject, signal, untracked } from '@angular/core';
import { DecimalPipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ZardButtonComponent } from '@/shared/components/button';
import { ZardComboboxImports } from '@/shared/components/combobox';
import { ZardSelectImports } from '@/shared/components/select';

type DecisionModel = Schemas['DecisionModel'];
type Effort = Schemas['ReasoningOptions']['efforts'][number];

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

@Component({
 selector: 'app-model-settings', imports: [DecimalPipe, FormsModule, ZardButtonComponent, ...ZardComboboxImports, ...ZardSelectImports], changeDetection: ChangeDetectionStrategy.OnPush,
 template: `
 <section class="space-y-6 rounded-xl border p-5 sm:p-6">
   <header class="flex flex-wrap items-start justify-between gap-3"><div><h2 class="text-lg font-semibold">AI models</h2><p class="text-sm text-muted-foreground">Configure automatic summaries and matching decisions.</p></div><button z-button zType="outline" [zDisabled]="busy() || loading()" (click)="load()">Refresh catalogue</button></header>
   <fieldset class="model-group"><legend>Automatic summaries</legend>
   <p class="text-sm text-muted-foreground">Uses the active OpenRouter key and each member’s USD allowance.</p>

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
   </div>
   @if (summaryModel && !endpointLoading() && !endpointError() && !endpoints().length) {<p class="text-sm text-muted-foreground">No compatible providers are available for this model. Choose another model.</p>}
   @if (endpointError()) {<p role="alert" class="text-sm text-destructive">{{ endpointError() }}</p><button z-button zType="outline" (click)="loadEndpoints()">Retry loading providers</button>}

   <div class="field"><span class="field-label">Thinking effort</span>
     <z-select class="w-full max-w-xl" aria-label="Thinking effort" zPlaceholder="Automatic" [ngModel]="summaryEffort" (ngModelChange)="selectEffort(single($event))" [zDisabled]="!effortChoices().length || busy() || loading()">
       @for (o of effortOptions(); track o.value) { <z-select-item [zValue]="o.value">{{ o.label }}</z-select-item> }
     </z-select>
     <span class="text-xs text-muted-foreground" [class.text-destructive]="effortUnavailable()">{{ effortHint() }}</span>
   </div>

   <details class="output-limits"><summary>Output limits</summary><div class="space-y-3 pt-4">
     <div class="field"><span class="field-label">Summary token cap</span>
       <z-select class="w-full max-w-xl" aria-label="Summary token cap" [ngModel]="'' + summaryMaxOutputTokens" (ngModelChange)="selectTokenCap(single($event))" [zDisabled]="loading() || busy()">
         @for (cap of summaryTokenCaps; track cap.value) { <z-select-item [zValue]="'' + cap.value">{{ cap.label }}</z-select-item> }
       </z-select>
       <p class="text-xs text-muted-foreground">Includes reasoning and summary text. Higher caps give reasoning models more room and reserve more of the user's allowance.</p>
     </div>
     <div class="field"><span class="field-label">Final summary character cap</span>
       <z-select class="w-full max-w-xl" aria-label="Final summary character cap" [ngModel]="'' + summaryMaxCharacters" (ngModelChange)="selectCharacterCap(single($event))" [zDisabled]="loading() || busy()">
         @for (cap of summaryCharacterCaps; track cap) { <z-select-item [zValue]="'' + cap">{{ cap }} characters{{ cap === 500 ? ' (default)' : '' }}</z-select-item> }
       </z-select>
       <p class="text-xs text-muted-foreground">Limits generated summary text, including spaces and punctuation. Changing this setting regenerates automatic summaries.</p>
     </div>
   </div></details></fieldset>

   @if (error()) { <p role="alert" class="text-sm text-destructive">{{ error() }}</p> }
   @if (settings(); as saved) {
     <fieldset class="model-group"><legend>Matching decisions</legend><p class="text-sm text-muted-foreground">New runs use this model. Existing runs keep their selection. System One models do not use summary service tiers.</p><p class="text-xs text-muted-foreground">Saved model: {{ saved.decision_model || 'Not configured' }}</p>
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
     </div>
     @if (selectedDecisionModel(); as m) {
       <p class="text-xs">Context {{ m.context_length | number }} · input {{ perMillion(m.pricing['prompt']) }} / 1M tokens · output {{ perMillion(m.pricing['completion']) }} / 1M tokens</p>
       @if (m.compatibility_reason) { <p class="text-xs text-amber-700">Calls wait: {{ m.compatibility_reason.replaceAll('_',' ') }}.</p> }
     }
     </fieldset><div class="flex flex-wrap items-center gap-3 border-t pt-4"><button z-button [zDisabled]="loading() || busy() || endpointLoading() || effortUnavailable() || (!!summaryModel && !endpoints().some(endpointMatches))" (click)="save()">{{ busy() ? 'Saving…' : 'Save model settings' }}</button><span role="status" class="text-sm text-muted-foreground">{{ success() }}</span></div>
   } @else { <p class="text-sm">{{ loading() ? 'Loading model settings…' : 'Model settings are unavailable.' }}</p> }
 </section>`,
 styles: [`
 :host { display: block; }
 .model-group { min-width: 0; margin: 0; border: 0; border-top: 1px solid var(--border); padding: 1.25rem 0 0; display: grid; gap: 1rem; }
 .model-group legend { padding-right: .75rem; font-weight: 600; }
 .field { display: grid; gap: .375rem; max-width: 44rem; }
 .field-label { font-size: .875rem; font-weight: 500; }
 .output-limits { border-radius: .5rem; padding: 1rem; background: var(--muted); }
 summary { cursor: pointer; font-weight: 500; }
 summary:focus-visible { outline: 2px solid var(--ring); outline-offset: 3px; }
 `],
})
export class ModelSettings {
 private readonly api = inject(Api).client;
 private readonly auth = inject(AuthService);
 protected readonly single = single;
 readonly endpoints = signal<{tag: string; name: string; service_tier: string}[]>([]);
 readonly success = signal('');
 summaryTier = 'default';
 tierLabel(tier: string): string { return ({default: 'Standard · regular capacity', flex: 'Flex · lower price, lower priority', priority: 'Priority · faster processing, higher price', ultrafast: 'Ultrafast · highest speed and price'} as Record<string, string>)[tier] ?? tier; }
 availableTiers(): string[] { return [...new Set(this.endpoints().map(e => e.service_tier))]; }
 tierEndpoints() { return this.endpoints().filter(e => e.service_tier === this.summaryTier); }
 selectTier(tier: string): void { this.summaryTier = tier; const endpoints = this.tierEndpoints(); this.summaryEndpoint = endpoints.length === 1 ? endpoints[0]!.tag : ''; this.success.set(''); }
 selectEndpoint(tag: string): void { this.summaryEndpoint = tag; this.success.set(''); }
 readonly endpointLoading = signal(false);
 readonly endpointError = signal('');
 private endpointGeneration = 0;
 readonly endpointMatches = (endpoint: {tag: string}) => endpoint.tag === this.summaryEndpoint;
 readonly settings = signal<Schemas['ModelSettings'] | null>(null);
 readonly textModels = signal<DecisionModel[]>([]);
 readonly models = signal<DecisionModel[]>([]);
 readonly error = signal(''); readonly loading = signal(false); readonly busy = signal(false);
 readonly summaryTokenCaps: {value: Schemas['SummaryMaxOutputTokens']; label: string}[] = [
   {value: 1024, label: '1,024 tokens'}, {value: 2048, label: '2,048 tokens (default)'},
   {value: 4096, label: '4,096 tokens'}, {value: 8192, label: '8,192 tokens'},
 ];
 readonly summaryCharacterCaps: Schemas['SummaryMaxCharacters'][] = [100, 200, 300, 400, 500];
 summaryMaxCharacters: Schemas['SummaryMaxCharacters'] = 500;
 summaryMaxOutputTokens: Schemas['SummaryMaxOutputTokens'] = 2048;
 summaryModel = ''; summaryEndpoint = '';
 // '' is automatic: optional thinking stays off and required thinking uses its lowest effort.
 summaryEffort: '' | Effort = '';
 selected = ''; private generation = 0;
 constructor() { effect(() => { this.auth.identity(); untracked(() => { this.generation++; this.settings.set(null); this.models.set([]); this.textModels.set([]); this.selected = ''; this.summaryModel = ''; this.summaryEndpoint = ''; this.summaryEffort = ''; this.summaryMaxOutputTokens = 2048; this.summaryMaxCharacters = 500; ++this.endpointGeneration; this.endpoints.set([]); this.endpointError.set(''); this.endpointLoading.set(false); void this.load(); }); }); }
 async load(): Promise<void> {
   const identity = this.auth.identity(), seq = ++this.generation; this.loading.set(true); this.error.set('');
   const current = () => identity === this.auth.identity() && seq === this.generation;
   try {
     const saved = Api.ok(await this.api.GET('/v1/admin/model-settings')); if (!current()) return;
     this.settings.set(saved); this.selected = saved.decision_model ?? ''; this.summaryModel = saved.summary_model ?? ''; this.summaryEndpoint = saved.summary_endpoint ?? ''; this.summaryEffort = saved.summary_reasoning_effort ?? ''; this.summaryMaxOutputTokens = saved.summary_max_output_tokens; this.summaryMaxCharacters = saved.summary_max_characters;
     await this.loadEndpoints(); if (!current()) return;
     const textModels = Api.ok(await this.api.GET('/v1/admin/text-models')); if (!current()) return; this.textModels.set([...textModels].sort(byName));
     const models = Api.ok(await this.api.GET('/v1/admin/decision-models')); if (current()) this.models.set([...models].sort(byName));
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
   return `Thinking counts toward the ${this.summaryMaxOutputTokens.toLocaleString('en-US')}-token output cap. ${EFFORT_LABELS[this.summaryEffort]} can use up to about ${share}% of it, leaving the rest for the summary.`;
 }
 perMillion(value: unknown): string {
   const usd = Number(value) * 1_000_000;
   if (!Number.isFinite(usd)) return '—';
   return `$${usd.toLocaleString('en-US', { maximumFractionDigits: usd < 1 ? 4 : 2 })}`;
 }
 selectTokenCap(value: string): void { this.summaryMaxOutputTokens = Number(value) as Schemas['SummaryMaxOutputTokens']; this.success.set(''); }
 selectCharacterCap(value: string): void { this.summaryMaxCharacters = Number(value) as Schemas['SummaryMaxCharacters']; this.success.set(''); }
 selectEffort(value: string): void { this.summaryEffort = value as '' | Effort; this.success.set(''); }
 selectDecisionModel(value: string): void { this.selected = value; this.success.set(''); }
 selectSummaryModel(model: string | string[] | null): void {
   this.summaryModel = single(model); this.summaryEndpoint = ''; this.summaryEffort = ''; void this.loadEndpoints();
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
 async save(): Promise<void> {
   const saved = this.settings(); if (!saved || this.endpointLoading() || this.effortUnavailable() || (this.summaryModel && !this.endpoints().some(this.endpointMatches))) return;
   const identity = this.auth.identity(), generation = this.generation; this.busy.set(true); this.error.set(''); this.success.set('');
   const current = () => identity === this.auth.identity() && generation === this.generation;
   try {
     const value = Api.ok(await this.api.PUT('/v1/admin/model-settings', { params: { header: { 'If-Match': `"${saved.version}"` } }, body: { decision_model: this.selected || null, summary_model: this.summaryModel || null, summary_endpoint: this.summaryEndpoint || null, summary_max_output_tokens: this.summaryMaxOutputTokens, summary_max_characters: this.summaryMaxCharacters, summary_reasoning_effort: this.summaryModel ? (this.summaryEffort || null) : null } }));
     if (current()) { this.settings.set(value); this.summaryEffort = value.summary_reasoning_effort ?? ''; this.success.set('Model settings saved.'); }
   } catch(e) { if (current()) this.error.set((e as Error).message); }
   finally { if (current()) this.busy.set(false); }
 }
}
