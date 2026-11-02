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
   <label class="field"><span>Summary model (OpenRouter ID)</span><select class="rounded border bg-background p-2" [(ngModel)]="summaryModel"><option value="">Not configured</option>@for (m of textModels(); track m.id) {<option [value]="m.id">{{ m.name }}</option>}</select></label>
   <label class="field"><span>Summary provider endpoint</span><input class="rounded border bg-background p-2" [(ngModel)]="summaryEndpoint" placeholder="OpenRouter endpoint tag" /></label>
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
     <div class="flex gap-2"><button z-button [zDisabled]="loading() || busy()" (click)="save()">Save model</button><button z-button zType="outline" [zDisabled]="busy() || loading()" (click)="load()">Refresh catalogue</button></div>
   } @else { <p class="text-sm">{{ loading() ? 'Loading model settings…' : 'Model settings are unavailable.' }}</p> }
 </section>`,
})
export class ModelSettings {
 private readonly api = inject(Api).client;
 private readonly auth = inject(AuthService);
 readonly settings = signal<Schemas['ModelSettings'] | null>(null);
 readonly textModels = signal<Schemas['DecisionModel'][]>([]);
 readonly models = signal<Schemas['DecisionModel'][]>([]);
 readonly error = signal(''); readonly loading = signal(false); readonly busy = signal(false);
 summaryModel = ''; summaryEndpoint = '';
 selected = ''; private generation = 0;
 constructor() { effect(() => { this.auth.identity(); untracked(() => { this.generation++; this.settings.set(null); this.models.set([]); this.textModels.set([]); this.selected = ''; this.summaryModel = ''; this.summaryEndpoint = ''; void this.load(); }); }); }
 async load(): Promise<void> {
   const identity = this.auth.identity(), seq = ++this.generation; this.loading.set(true); this.error.set('');
   const current = () => identity === this.auth.identity() && seq === this.generation;
   try {
     const saved = Api.ok(await this.api.GET('/v1/admin/model-settings')); if (!current()) return;
     this.settings.set(saved); this.selected = saved.decision_model ?? ''; this.summaryModel = saved.summary_model ?? ''; this.summaryEndpoint = saved.summary_endpoint ?? '';
     const textModels = Api.ok(await this.api.GET('/v1/admin/text-models')); if (current()) this.textModels.set(textModels);
     const models = Api.ok(await this.api.GET('/v1/admin/decision-models')); if (current()) this.models.set(models);
   } catch(e) { if (current()) this.error.set((e as Error).message); }
   finally { if (current()) this.loading.set(false); }
 }
 async save(): Promise<void> {
   const saved = this.settings(); if (!saved) return;
   const identity = this.auth.identity(), generation = this.generation; this.busy.set(true); this.error.set('');
   const current = () => identity === this.auth.identity() && generation === this.generation;
   try {
     const value = Api.ok(await this.api.PUT('/v1/admin/model-settings', { params: { header: { 'If-Match': `"${saved.version}"` } }, body: { decision_model: this.selected || null, summary_model: this.summaryModel || null, summary_endpoint: this.summaryEndpoint || null } }));
     if (current()) this.settings.set(value);
   } catch(e) { if (current()) this.error.set((e as Error).message); }
   finally { if (current()) this.busy.set(false); }
 }
}
