import { Component, ChangeDetectionStrategy, computed, effect, inject, input, signal, DestroyRef } from '@angular/core';
import { DecimalPipe, DatePipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ZardButtonComponent } from '@/shared/components/button';
import { MatchStore } from './match.store';
@Component({
 selector: 'app-match-tab',
 imports: [DecimalPipe, DatePipe, FormsModule, RouterLink, ZardButtonComponent],
 changeDetection: ChangeDetectionStrategy.OnPush,
 templateUrl: './match-tab.html',
})
export class MatchTab {
 readonly jobId = input.required<string>();
 protected readonly store = inject(MatchStore);
 protected readonly run = computed(() => this.store.runs().get(this.jobId()));
 protected readonly estimate = computed(() => this.store.estimates().get(this.jobId()));
 protected readonly busy = signal(false);
 protected readonly error = signal('');
 protected readonly active = computed(() => ['pending','running','blocked','needs_review'].includes(this.run()?.status ?? ''));
 constructor() {
   effect(onCleanup => { const id = this.jobId(); this.store.watch(id); onCleanup(() => this.store.unwatch(id)); });
   inject(DestroyRef).onDestroy(() => this.store.unwatch(this.jobId()));
 }
 protected async action(kind: string, hash = '', value = ''): Promise<void> {
   const id = this.jobId(); this.busy.set(true); this.error.set('');
   try {
     if (kind === 'start') await this.store.start(id);
     else if (kind === 'cancel') await this.store.cancel(id, this.run()!.id);
     else await this.store.correct(id, hash, value);
   } catch(e) { if (id === this.jobId()) this.error.set((e as Error).message); }
   finally { if (id === this.jobId()) this.busy.set(false); }
 }
}
