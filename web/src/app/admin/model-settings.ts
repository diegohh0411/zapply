import { ChangeDetectionStrategy, Component, effect, inject, signal, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ZardButtonComponent } from '@/shared/components/button';
@Component({
 selector: 'app-model-settings', imports: [FormsModule, ZardButtonComponent], changeDetection: ChangeDetectionStrategy.OnPush,
 template: `
 <section class="space-y-6 rounded-xl border p-5 sm:p-6">
   <header class="flex flex-wrap items-start justify-between gap-3"><div><h2 class="text-lg font-semibold">AI models</h2><p class="text-sm text-muted-foreground">Configure automatic summaries and matching decisions.</p></div><button z-button zType="outline" [zDisabled]="busy() || loading()" (click)="load()">Refresh catalogue</button></header>
   <fieldset class="model-group"><legend>Automatic summaries</legend>
   <p class="text-sm text-muted-foreground">Uses the active OpenRouter key and each member’s USD allowance.</p>
   <label class="field"><span>Model</span><select class="rounded border bg-background p-2" [ngModel]="summaryModel" (ngModelChange)="selectSummaryModel($event)" [disabled]="loading() || busy()"><option value="">Not configured</option>@for (m of textModels(); track m.id) {<option [value]="m.id">{{ m.name }}</option>}</select></label>
   <label class="field"><span>Service tier</span><select class="rounded border bg-background p-2" [ngModel]="summaryTier" (ngModelChange)="selectTier($event)" [disabled]="!summaryModel || endpointLoading() || busy() || loading()">
     @for (tier of availableTiers(); track tier) {<option [value]="tier">{{ tierLabel(tier) }}</option>}
   </select><span class="text-xs text-muted-foreground">Only tiers offered by this model are shown. Requests stay on the selected provider endpoint.</span></label>
   <label class="field"><span>Provider endpoint</span><select class="rounded border bg-background p-2" [(ngModel)]="summaryEndpoint" [disabled]="!summaryModel || endpointLoading() || busy() || loading()">
     <option value="">{{ endpointLoading() ? 'Loading providers…' : 'Select a provider' }}</option>
     @for (endpoint of tierEndpoints(); track endpoint.tag) {<option [value]="endpoint.tag">{{ endpoint.name }}</option>}
   </select></label>
   @if (summaryModel && !endpointLoading() && !endpointError() && !endpoints().length) {<p class="text-sm text-muted-foreground">No compatible providers are available for this model. Choose another model.</p>}
   @if (endpointError()) {<p role="alert" class="text-sm text-destructive">{{ endpointError() }}</p><button z-button zType="outline" (click)="loadEndpoints()">Retry loading providers</button>}
   <details class="output-limits"><summary>Output limits</summary><div class="space-y-3 pt-4"><label class="field"><span>Summary token cap</span><select class="rounded border bg-background p-2" [(ngModel)]="summaryMaxOutputTokens" [disabled]="loading() || busy()">
     @for (cap of summaryTokenCaps; track cap.value) {<option [ngValue]="cap.value">{{ cap.label }}</option>}
   </select></label>
   <p class="text-xs text-muted-foreground">Includes reasoning and summary text. Higher caps give reasoning models more room and reserve more of the user's allowance.</p>
   <label class="field"><span>Final summary character cap</span><select class="rounded border bg-background p-2" [(ngModel)]="summaryMaxCharacters" [disabled]="loading() || busy()">
     @for (cap of summaryCharacterCaps; track cap) {<option [ngValue]="cap">{{ cap }} characters{{ cap === 500 ? ' (default)' : '' }}</option>}
   </select></label>
   <p class="text-xs text-muted-foreground">Limits generated summary text, including spaces and punctuation. Changing this setting regenerates automatic summaries.</p>
   </div></details></fieldset>

   @if (error()) { <p role="alert" class="text-sm text-destructive">{{ error() }}</p> }
   @if (settings(); as saved) {
     <fieldset class="model-group"><legend>Matching decisions</legend><p class="text-sm text-muted-foreground">New runs use this model. Existing runs keep their selection. System One models do not use summary service tiers.</p><p class="text-xs text-muted-foreground">Saved model: {{ saved.decision_model || 'Not configured' }}</p>
     <label class="field"><span>Decision model</span><select class="rounded border bg-background p-2" [(ngModel)]="selected" [disabled]="loading() || busy()"><option value="">Not configured</option>@for (m of models(); track m.id) {<option [value]="m.id">{{ m.name }}</option>}</select></label>
     @for (m of models(); track m.id) { @if (m.id === selected) {
       <p class="text-xs">Context: {{ m.context_length }} · prompt price: {{ m.pricing['prompt'] }} USD per token · completion: {{ m.pricing['completion'] }}</p>
       @if (m.compatibility_reason) { <p class="text-xs text-amber-700">Calls wait: {{ m.compatibility_reason.replaceAll('_',' ') }}.</p> }
     } }
     </fieldset><div class="flex flex-wrap items-center gap-3 border-t pt-4"><button z-button [zDisabled]="loading() || busy() || endpointLoading() || (!!summaryModel && !endpoints().some(endpointMatches))" (click)="save()">{{ busy() ? 'Saving…' : 'Save model settings' }}</button><span role="status" class="text-sm text-muted-foreground">{{ success() }}</span></div>
   } @else { <p class="text-sm">{{ loading() ? 'Loading model settings…' : 'Model settings are unavailable.' }}</p> }
 </section>`,
 styles: [`
 :host { display: block; }
 .model-group { min-width: 0; margin: 0; border: 0; border-top: 1px solid var(--border); padding: 1.25rem 0 0; display: grid; gap: 1rem; }
 .model-group legend { padding-right: .75rem; font-weight: 600; }
 .field { display: grid; gap: .375rem; max-width: 44rem; }
 select { width: 100%; min-height: 2.75rem; }
 select:focus-visible, summary:focus-visible { outline: 2px solid var(--ring); outline-offset: 3px; }
 .output-limits { border-radius: .5rem; padding: 1rem; background: var(--muted); }
 summary { cursor: pointer; font-weight: 500; }
 `],
})
export class ModelSettings {
 private readonly api = inject(Api).client;
 private readonly auth = inject(AuthService);
 readonly endpoints = signal<{tag: string; name: string; service_tier: string}[]>([]);
 readonly success = signal('');
 summaryTier = 'default';
 tierLabel(tier: string): string { return ({default: 'Standard · regular capacity', flex: 'Flex · lower price, lower priority', priority: 'Priority · faster processing, higher price', ultrafast: 'Ultrafast · highest speed and price'} as Record<string, string>)[tier] ?? tier; }
 availableTiers(): string[] { return [...new Set(this.endpoints().map(e => e.service_tier))]; }
 tierEndpoints() { return this.endpoints().filter(e => e.service_tier === this.summaryTier); }
 selectTier(tier: string): void { this.summaryTier = tier; const endpoints = this.tierEndpoints(); this.summaryEndpoint = endpoints.length === 1 ? endpoints[0]!.tag : ''; this.success.set(''); }
 readonly endpointLoading = signal(false);
 readonly endpointError = signal('');
 private endpointGeneration = 0;
 readonly endpointMatches = (endpoint: {tag: string}) => endpoint.tag === this.summaryEndpoint;
 readonly settings = signal<Schemas['ModelSettings'] | null>(null);
 readonly textModels = signal<Schemas['DecisionModel'][]>([]);
 readonly models = signal<Schemas['DecisionModel'][]>([]);
 readonly error = signal(''); readonly loading = signal(false); readonly busy = signal(false);
 readonly summaryTokenCaps: {value: Schemas['SummaryMaxOutputTokens']; label: string}[] = [
   {value: 1024, label: '1,024 tokens'}, {value: 2048, label: '2,048 tokens (default)'},
   {value: 4096, label: '4,096 tokens'}, {value: 8192, label: '8,192 tokens'},
 ];
 readonly summaryCharacterCaps: Schemas['SummaryMaxCharacters'][] = [100, 200, 300, 400, 500];
 summaryMaxCharacters: Schemas['SummaryMaxCharacters'] = 500;
 summaryMaxOutputTokens: Schemas['SummaryMaxOutputTokens'] = 2048;
 summaryModel = ''; summaryEndpoint = '';
 selected = ''; private generation = 0;
 constructor() { effect(() => { this.auth.identity(); untracked(() => { this.generation++; this.settings.set(null); this.models.set([]); this.textModels.set([]); this.selected = ''; this.summaryModel = ''; this.summaryEndpoint = ''; this.summaryMaxOutputTokens = 2048; this.summaryMaxCharacters = 500; ++this.endpointGeneration; this.endpoints.set([]); this.endpointError.set(''); this.endpointLoading.set(false); void this.load(); }); }); }
 async load(): Promise<void> {
   const identity = this.auth.identity(), seq = ++this.generation; this.loading.set(true); this.error.set('');
   const current = () => identity === this.auth.identity() && seq === this.generation;
   try {
     const saved = Api.ok(await this.api.GET('/v1/admin/model-settings')); if (!current()) return;
     this.settings.set(saved); this.selected = saved.decision_model ?? ''; this.summaryModel = saved.summary_model ?? ''; this.summaryEndpoint = saved.summary_endpoint ?? ''; this.summaryMaxOutputTokens = saved.summary_max_output_tokens; this.summaryMaxCharacters = saved.summary_max_characters;
     await this.loadEndpoints(); if (!current()) return;
     const textModels = Api.ok(await this.api.GET('/v1/admin/text-models')); if (!current()) return; this.textModels.set(textModels);
     const models = Api.ok(await this.api.GET('/v1/admin/decision-models')); if (current()) this.models.set(models);
   } catch(e) { if (current()) this.error.set((e as Error).message); }
   finally { if (current()) this.loading.set(false); }
 }
 selectSummaryModel(model: string): void {
   this.summaryModel = model; this.summaryEndpoint = ''; void this.loadEndpoints();
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
   const saved = this.settings(); if (!saved || this.endpointLoading() || (this.summaryModel && !this.endpoints().some(this.endpointMatches))) return;
   const identity = this.auth.identity(), generation = this.generation; this.busy.set(true); this.error.set(''); this.success.set('');
   const current = () => identity === this.auth.identity() && generation === this.generation;
   try {
     const value = Api.ok(await this.api.PUT('/v1/admin/model-settings', { params: { header: { 'If-Match': `"${saved.version}"` } }, body: { decision_model: this.selected || null, summary_model: this.summaryModel || null, summary_endpoint: this.summaryEndpoint || null, summary_max_output_tokens: this.summaryMaxOutputTokens, summary_max_characters: this.summaryMaxCharacters } }));
     if (current()) { this.settings.set(value); this.success.set('Model settings saved.'); }
   } catch(e) { if (current()) this.error.set((e as Error).message); }
   finally { if (current()) this.busy.set(false); }
 }
}
