import { ModelSettings } from './model-settings';
import { DatePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  effect,
  inject,
  signal,
} from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { AccountService } from '../core/account.service';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { blockMessage, usd } from '../usage/format';

type Plan = Schemas['Plan'];
type Member = Schemas['Member'];
type Attempt = Schemas['AttemptSummary'];

@Component({
  selector: 'app-admin-page',
  imports: [ModelSettings,DatePipe, FormsModule, RouterLink, ZardButtonComponent, ZardInputComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './admin.page.html',
})
export class AdminPage {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly account = inject(AccountService);
  private readonly events = inject(EventsService);
  private readonly params = toSignal(inject(ActivatedRoute).paramMap);
  protected readonly section = signal('plans');
  protected readonly plans = signal<Plan[]>([]);
  protected readonly members = signal<Member[]>([]);
  protected readonly keys = signal<Schemas['SystemKey'][]>([]);
  protected readonly attempts = signal<Attempt[]>([]);
  protected readonly operation = signal<Schemas['ModelOperation'] | null>(null);
  protected readonly loading = signal(false);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly moreAvailable = signal(false);
  protected readonly editingPlan = signal<Plan | null>(null);
  protected readonly resolving = signal<Attempt | null>(null);
  protected readonly money = usd;
  protected readonly message = blockMessage;
  protected planForm = { name: '', cap_7d_usd: '', cap_30d_usd: '' };
  protected keyForm = { label: '', key: '' };
  protected resolution = { amount_usd: '', generation_id: '', reason: '' };
  protected assignments: Record<string, string> = {};
  private generation = 0;
  private testKey: string | null = null;
  private poll: ReturnType<typeof setInterval> | undefined;

  constructor() {
    effect(() => {
      const section = this.params()?.get('section') ?? 'plans';
      this.auth.identity();
      this.section.set(section);
      this.clear();
      void this.load();
    });
    const change = this.events.changes.subscribe((e) => {
      if (e.topic === 'model_operation.changed') void this.refreshOperation();
    });
    const focus = () => void this.load();
    window.addEventListener('focus', focus);
    inject(DestroyRef).onDestroy(() => {
      this.generation++;
      clearInterval(this.poll);
      change.unsubscribe();
      window.removeEventListener('focus', focus);
      this.keyForm.key = '';
    });
  }

  private clear(): void {
    this.plans.set([]);
    this.members.set([]);
    this.keys.set([]);
    this.attempts.set([]);
    this.error.set('');
    this.editingPlan.set(null);
    this.resolving.set(null);
    this.operation.set(null);
    this.testKey = null;
    clearInterval(this.poll);
    this.assignments = {};
    this.planForm = { name: '', cap_7d_usd: '', cap_30d_usd: '' };
    this.keyForm = { label: '', key: '' };
  }

  async load(append = false): Promise<void> {
    const identity = this.auth.identity(),
      section = this.section(),
      generation = ++this.generation;
    if (!identity) return;
    this.loading.set(true);
    this.error.set('');
    try {
      if (section === 'plans') {
        const list = Api.ok(await this.api.client.GET('/v1/admin/plans'));
        if (this.current(identity, section, generation)) this.plans.set(list);
      } else if (section === 'members') {
        const after = append ? this.members().at(-1)?.id : undefined;
        const [plans, response] = await Promise.all([
          this.api.client.GET('/v1/admin/plans'),
          this.api.client.GET('/v1/admin/members', { params: { query: { after, limit: 50 } } }),
        ]);
        if (!this.current(identity, section, generation)) return;
        const list = Api.ok(response);
        this.plans.set(Api.ok(plans));
        this.moreAvailable.set(list.length === 50);
        this.members.update((rows) => (append ? [...rows, ...list] : list));
        for (const m of list) this.assignments[m.id] = m.membership?.plan_id ?? '';
      } else if (section === 'system-keys') {
        const list = Api.ok(await this.api.client.GET('/v1/admin/system-keys'));
        if (this.current(identity, section, generation)) this.keys.set(list);
      } else if (section === 'usage') {
        const before = append ? this.attempts().at(-1)?.id : undefined;
        const list = Api.ok(
          await this.api.client.GET('/v1/admin/model-attempts', {
            params: { query: { before, limit: 50 } },
          }),
        );
        if (!this.current(identity, section, generation)) return;
        this.attempts.update((rows) => (append ? [...rows, ...list] : list));
        this.moreAvailable.set(list.length === 50);
      }
    } catch (e) {
      if (this.current(identity, section, generation))
        this.error.set(e instanceof Error ? e.message : 'Could not load administration.');
    } finally {
      if (this.current(identity, section, generation)) this.loading.set(false);
    }
  }
  private current(identity: string | null, section: string, generation: number): boolean {
    return (
      identity === this.auth.identity() &&
      section === this.section() &&
      generation === this.generation
    );
  }
  private async mutate(action: () => Promise<void>): Promise<void> {
    if (this.busy()) return;
    const identity = this.auth.identity();
    this.busy.set(true);
    this.error.set('');
    try {
      await action();
      if (identity === this.auth.identity()) {
        await this.account.refresh();
        await this.load();
      }
    } catch (e) {
      if (identity === this.auth.identity()) this.error.set(this.saveError(e));
    } finally {
      if (identity === this.auth.identity()) this.busy.set(false);
    }
  }
  private saveError(error: unknown): string {
    if (error instanceof ApiError && error.fieldErrors.length) {
      const labels: Record<string, string> = {
        name: 'Name',
        cap_7d_usd: 'Past 7 days allowance',
        cap_30d_usd: 'Past 30 days allowance',
        label: 'Key label',
        key: 'OpenRouter key',
        amount_usd: 'Confirmed cost',
        reason: 'Reconciliation evidence',
        generation_id: 'Generation ID',
      };
      return error.fieldErrors
        .map((e) => (labels[e.field] ? labels[e.field] + ': ' : '') + e.message)
        .join('; ');
    }
    return error instanceof Error ? error.message : 'Could not save changes.';
  }
  editPlan(p: Plan): void {
    this.editingPlan.set(p);
    this.planForm = { name: p.name, cap_7d_usd: p.cap_7d_usd, cap_30d_usd: p.cap_30d_usd };
  }
  cancelPlan(): void {
    this.editingPlan.set(null);
    this.planForm = { name: '', cap_7d_usd: '', cap_30d_usd: '' };
  }
  async savePlan(): Promise<void> {
    const p = this.editingPlan(),
      body = { ...this.planForm };
    await this.mutate(async () => {
      if (p)
        Api.ok(
          await this.api.client.PUT('/v1/admin/plans/{id}', {
            params: { path: { id: p.id }, header: { 'If-Match': '"' + p.version + '"' } },
            body,
          }),
        );
      else Api.ok(await this.api.client.POST('/v1/admin/plans', { body }));
      this.cancelPlan();
    });
  }
  async saveMember(
    m: Member,
    status: 'active' | 'suspended' = m.membership?.status ?? 'active',
  ): Promise<void> {
    const plan_id = this.assignments[m.id];
    if (!plan_id) return;
    await this.mutate(async () => {
      Api.ok(
        await this.api.client.PUT('/v1/admin/members/{id}/membership', {
          params: {
            path: { id: m.id },
            header: { 'If-Match': '"' + (m.membership?.version ?? 0) + '"' },
          },
          body: { plan_id, status },
        }),
      );
    });
  }
  async addKey(): Promise<void> {
    const body = { ...this.keyForm };
    this.keyForm.key = '';
    await this.mutate(async () => {
      Api.ok(await this.api.client.POST('/v1/admin/system-keys', { body }));
      this.keyForm.label = '';
    });
  }
  async activate(k: Schemas['SystemKey']): Promise<void> {
    await this.mutate(async () => {
      Api.ok(
        await this.api.client.PUT('/v1/admin/system-keys/{id}/activation', {
          params: { path: { id: k.id }, header: { 'If-Match': '"' + k.version + '"' } },
          body: { active: !k.active },
        }),
      );
    });
  }
  startResolution(a: Attempt): void {
    this.resolving.set(a);
    this.resolution = { amount_usd: '', generation_id: a.generation_id ?? '', reason: '' };
  }
  async resolve(): Promise<void> {
    const a = this.resolving();
    if (!a) return;
    const body = { ...this.resolution, generation_id: this.resolution.generation_id || undefined };
    await this.mutate(async () => {
      Api.ok(
        await this.api.client.POST('/v1/admin/model-attempts/{id}/resolve', {
          params: { path: { id: a.id }, header: { 'If-Match': '"' + a.version + '"' } },
          body,
        }),
      );
      this.resolving.set(null);
    });
  }
  async testGateway(): Promise<void> {
    await this.mutate(async () => {
      this.testKey ??= crypto.randomUUID();
      const op = Api.ok(
        await this.api.client.POST('/v1/admin/gateway-tests', {
          params: { header: { 'Idempotency-Key': this.testKey } },
        }),
      );
      this.operation.set(op);
      this.testKey = null;
      clearInterval(this.poll);
      this.poll = setInterval(() => void this.refreshOperation(), 5000);
    });
  }
  private async refreshOperation(): Promise<void> {
    const op = this.operation(),
      identity = this.auth.identity();
    if (!op) return;
    try {
      const result = Api.ok(
        await this.api.client.GET('/v1/admin/gateway-tests/{id}', {
          params: { path: { id: op.id } },
        }),
      );
      if (identity !== this.auth.identity() || this.operation()?.id !== op.id) return;
      this.operation.set(result);
      if (['completed', 'failed', 'cancelled'].includes(result.status)) clearInterval(this.poll);
    } catch {
      /* Transient polling failures are retried; mutation errors are shown. */
    }
  }
}
