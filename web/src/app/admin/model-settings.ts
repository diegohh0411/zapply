import { ChangeDetectionStrategy, Component, effect, inject, signal, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ZardButtonComponent } from '@/shared/components/button';
@Component({
 selector: 'app-model-settings', imports: [FormsModule, ZardButtonComponent], changeDetection: ChangeDetectionStrategy.OnPush,
 template: `
 <section class="space-y-3 rounded-xl border p-5">
   <h2 class="font-medium">AI models</h2>
   <label class="field"><span>Summary model (OpenRouter ID)</span><select class="rounded border bg-background p-2" [ngModel]="summaryModel" (ngModelChange)="selectSummaryModel($event)" [disabled]="loading() || busy()"><option value="">Not configured</option>@for (m of textModels(); track m.id) {<option [value]="m.id">{{ m.name }}</option>}</select></label>
   <label class="field"><span>Summary provider endpoint</span><select class="rounded border bg-background p-2" [(ngModel)]="summaryEndpoint" [disabled]="!summaryModel || endpointLoading() || busy() || loading()">
     <option value="">{{ endpointLoading() ? 'Loading providers…' : 'Select a provider' }}</option>
     @for (endpoint of endpoints(); track endpoint.tag) {<option [value]="endpoint.tag">{{ endpoint.name }}</option>}
   </select></label>
   @if (summaryModel && !endpointLoading() && !endpointError() && !endpoints().length) {<p class="text-sm text-muted-foreground">No compatible providers are available for this model. Choose another model.</p>}
   @if (endpointError()) {<p role="alert" class="text-sm text-destructive">{{ endpointError() }}</p><button z-button zType="outline" (click)="loadEndpoints()">Retry loading providers</button>}
   <label class="field"><span>Summary token cap</span><select class="rounded border bg-background p-2" [(ngModel)]="summaryMaxOutputTokens" [disabled]="loading() || busy()">
     @for (cap of summaryTokenCaps; track cap.value) {<option [ngValue]="cap.value">{{ cap.label }}</option>}
   </select></label>
   <p class="text-xs text-muted-foreground">Includes reasoning and summary text. Higher caps give reasoning models more room and reserve more of the user's allowance. The final summary remains limited to 500 characters.</p>
   <p class="text-xs text-muted-foreground">Automatic summaries use this model and each user's existing USD allowance.</p>
   <p class="text-sm text-muted-foreground">Uses the active system OpenRouter key. New runs use this selection; existing runs retain their model.</p>
   @if (error()) { <p role="alert" class="text-sm text-destructive">{{ error() }}</p> }
   @if (settings(); as saved) {
     <p class="text-xs text-muted-foreground">Saved model: {{ saved.decision_model || 'Not configured' }}</p>
     <label class="field"><span>Decision model</span><select class="rounded border bg-background p-2" [(ngModel)]="selected" [disabled]="loading() || busy()"><option value="">Not configured</option>@for (m of models(); track m.id) {<option [value]="m.id">{{ m.name }}</option>}</select></label>
     @for (m of models(); track m.id) { @if (m.id === selected) {
       <p class="text-xs">Context: {{ m.context_length }} · prompt price: {{ m.pricing['prompt'] }} USD per token · completion: {{ m.pricing['completion'] }}</p>
       @if (m.compatibility_reason) { <p class="text-xs text-amber-700">Calls wait: {{ m.compatibility_reason.replaceAll('_',' ') }}.</p> }
     } }
     <div class="flex gap-2"><button z-button [zDisabled]="loading() || busy() || endpointLoading() || (!!summaryModel && !endpoints().some(endpointMatches))" (click)="save()">Save model</button><button z-button zType="outline" [zDisabled]="busy() || loading()" (click)="load()">Refresh catalogue</button></div>
   } @else { <p class="text-sm">{{ loading() ? 'Loading model settings…' : 'Model settings are unavailable.' }}</p> }
 </section>`,
})
export class ModelSettings {
 private readonly api = inject(Api).client;
 private readonly auth = inject(AuthService);
 readonly endpoints = signal<{tag: string; name: string}[]>([]);
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
 summaryMaxOutputTokens: Schemas['SummaryMaxOutputTokens'] = 2048;
 summaryModel = ''; summaryEndpoint = '';
 selected = ''; private generation = 0;
 constructor() { effect(() => { this.auth.identity(); untracked(() => { this.generation++; this.settings.set(null); this.models.set([]); this.textModels.set([]); this.selected = ''; this.summaryModel = ''; this.summaryEndpoint = ''; this.summaryMaxOutputTokens = 2048; ++this.endpointGeneration; this.endpoints.set([]); this.endpointError.set(''); this.endpointLoading.set(false); void this.load(); }); }); }
 async load(): Promise<void> {
   const identity = this.auth.identity(), seq = ++this.generation; this.loading.set(true); this.error.set('');
   const current = () => identity === this.auth.identity() && seq === this.generation;
   try {
     const saved = Api.ok(await this.api.GET('/v1/admin/model-settings')); if (!current()) return;
     this.settings.set(saved); this.selected = saved.decision_model ?? ''; this.summaryModel = saved.summary_model ?? ''; this.summaryEndpoint = saved.summary_endpoint ?? ''; this.summaryMaxOutputTokens = saved.summary_max_output_tokens;
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
     if (!endpoints.some(e => e.tag === this.summaryEndpoint)) this.summaryEndpoint = endpoints.length === 1 ? endpoints[0]!.tag : '';
   } catch (e) { if (current()) this.endpointError.set((e as Error).message); }
   finally { if (current()) this.endpointLoading.set(false); }
 }
 async save(): Promise<void> {
   const saved = this.settings(); if (!saved || this.endpointLoading() || (this.summaryModel && !this.endpoints().some(this.endpointMatches))) return;
   const identity = this.auth.identity(), generation = this.generation; this.busy.set(true); this.error.set('');
   const current = () => identity === this.auth.identity() && generation === this.generation;
   try {
     const value = Api.ok(await this.api.PUT('/v1/admin/model-settings', { params: { header: { 'If-Match': `"${saved.version}"` } }, body: { decision_model: this.selected || null, summary_model: this.summaryModel || null, summary_endpoint: this.summaryEndpoint || null, summary_max_output_tokens: this.summaryMaxOutputTokens } }));
     if (current()) this.settings.set(value);
   } catch(e) { if (current()) this.error.set((e as Error).message); }
   finally { if (current()) this.busy.set(false); }
 }
}
