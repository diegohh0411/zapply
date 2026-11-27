import { AdminOverview } from './admin-overview';
import { ModelSettings, type ModelTab } from './model-settings';
import { PostingCatalog } from './posting-catalog';
import { EdgeWorkers } from './edge-workers';
import { JobSources } from './job-sources';
import { ScraperSettings } from './scraper-settings';
import { SearchSettings } from './search-settings';
import { DatePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  computed,
  effect,
  inject,
  signal,
} from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { fetchAllMembers } from './members';
import { AccountService } from '../core/account.service';
import { Api, ApiError, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { blockMessage, usd } from '../usage/format';

type Plan = Schemas['Plan'];
type Member = Schemas['Member'];
type Attempt = Schemas['AttemptSummary'];

const MODEL_TABS: { id: ModelTab; label: string }[] = [
  { id: 'summaries', label: 'Summaries' },
  { id: 'matching', label: 'Matching' },
  { id: 'extraction', label: 'Extraction' },
  { id: 'catalog', label: 'Posting catalog' },
];

const HEADINGS: Record<string, { title: string; description: string }> = {
  overview: {
    title: 'Platform administration',
    description: 'Current state of models, members and charges. Open an area to change it.',
  },
  plans: {
    title: 'Plans',
    description: 'A plan sets how much model use each assigned member gets, per 7 and 30 days.',
  },
  members: {
    title: 'Members',
    description:
      'Assign a plan to each member. Suspending model access keeps their library, jobs and results.',
  },
  models: {
    title: 'Models',
    description:
      'Each model saves on its own. Unsaved changes stay here until you save or discard them.',
  },
  keys: {
    title: 'Keys & checks',
    description:
      'Keys that power model calls, and a quick check that the platform can reach a model.',
  },
  scraping: {
    title: 'Scraping & search',
    description:
      'Which providers read job postings and in what order, the edge workers they can use, and the job search that supplies job alerts.',
  },
  'job-sources': {
    title: 'Job sources',
    description:
      'Where job alerts come from: TheirStack searches and the Greenhouse, Lever and Ashby boards the platform reads, how often each runs, and which boards to read.',
  },
  usage: {
    title: 'Unresolved usage',
    description:
      'These calls may have incurred a charge. Their reservations stay in place until the amount is confirmed. Other requests can use the remaining allowance. Confirm charges in OpenRouter before resolving them; requests with uncertain results cannot be automatically repeated.',
  },
};

@Component({
  selector: 'app-admin-page',
  imports: [
    AdminOverview,
    ModelSettings,
    PostingCatalog,
    ScraperSettings,
    SearchSettings,
    EdgeWorkers,
    JobSources,
    DatePipe,
    FormsModule,
    RouterLink,
    ZardButtonComponent,
    ZardInputComponent,
    ...ZardSelectImports,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './admin.page.html',
})
export class AdminPage {
  private readonly api = inject(Api);
  private readonly auth = inject(AuthService);
  private readonly account = inject(AccountService);
  private readonly events = inject(EventsService);
  private readonly route = inject(ActivatedRoute);
  private readonly params = toSignal(this.route.paramMap);
  private readonly query = toSignal(this.route.queryParamMap);
  protected readonly modelTabs = MODEL_TABS;
  protected readonly tab = computed<ModelTab>(() => {
    const value = this.query()?.get('tab');
    return value === 'matching' || value === 'extraction' || value === 'catalog'
      ? value
      : 'summaries';
  });
  protected readonly heading = computed(() => HEADINGS[this.section()] ?? HEADINGS['overview']!);
  protected readonly emailById = computed(
    () => new Map(this.members().map((m) => [m.id, m.email] as const)),
  );
  protected readonly section = signal('overview');
  protected readonly creating = signal(false);
  protected readonly memberQuery = signal('');
  protected readonly planFilter = signal('');
  protected readonly statusFilter = signal('');
  protected readonly visibleMembers = computed(() => {
    const query = this.memberQuery().trim().toLowerCase();
    const plan = this.planFilter();
    const status = this.statusFilter();
    return this.members().filter((m) => {
      if (query && !(m.email ?? '').toLowerCase().includes(query)) return false;
      if (plan && (m.membership?.plan_id ?? '') !== plan) return false;
      if (status === 'unassigned') return !m.membership;
      if (status) return m.membership?.status === status;
      return true;
    });
  });
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
  protected readonly keyKindLabel = (kind: Schemas['SystemKeyKind']): string =>
    ({
      openrouter: 'OpenRouter',
      firecrawl: 'Firecrawl',
      crawl4ai: 'Crawl4AI',
      theirstack: 'TheirStack',
    })[kind];
  protected readonly keyKind = (value: unknown): Schemas['SystemKeyKind'] =>
    value === 'firecrawl' || value === 'crawl4ai' || value === 'theirstack' ? value : 'openrouter';
  protected planForm = { name: '', cap_7d_usd: '', cap_30d_usd: '' };
  protected keyForm: { label: string; key: string; kind: Schemas['SystemKeyKind'] } = {
    label: '',
    key: '',
    kind: 'openrouter',
  };
  protected resolution = { amount_usd: '', generation_id: '', reason: '' };
  protected assignments: Record<string, string> = {};
  private generation = 0;
  private testKey: string | null = null;
  private poll: ReturnType<typeof setInterval> | undefined;

  constructor() {
    effect(() => {
      const section = this.params()?.get('section') ?? 'overview';
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
    this.creating.set(false);
    this.resolving.set(null);
    this.memberQuery.set('');
    this.planFilter.set('');
    this.statusFilter.set('');
    this.operation.set(null);
    this.testKey = null;
    clearInterval(this.poll);
    this.assignments = {};
    this.planForm = { name: '', cap_7d_usd: '', cap_30d_usd: '' };
    this.keyForm = { label: '', key: '', kind: 'openrouter' };
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
      } else if (section === 'keys') {
        const list = Api.ok(await this.api.client.GET('/v1/admin/system-keys'));
        if (this.current(identity, section, generation)) this.keys.set(list);
      } else if (section === 'usage') {
        const before = append ? this.attempts().at(-1)?.id : undefined;
        const [response, everyone] = await Promise.all([
          this.api.client.GET('/v1/admin/model-attempts', {
            params: { query: { before, limit: 50 } },
          }),
          // Charges name members by id; the members list maps each id to its email.
          fetchAllMembers(this.api.client),
        ]);
        if (!this.current(identity, section, generation)) return;
        const list = Api.ok(response);
        this.members.set(everyone);
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
        key: 'Provider key',
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
    this.creating.set(false);
    this.editingPlan.set(p);
    // The form shows the amount as dollars and cents, without the server's trailing zeros.
    this.planForm = {
      name: p.name,
      cap_7d_usd: usd(p.cap_7d_usd).slice(1),
      cap_30d_usd: usd(p.cap_30d_usd).slice(1),
    };
  }
  startCreate(): void {
    this.editingPlan.set(null);
    this.planForm = { name: '', cap_7d_usd: '', cap_30d_usd: '' };
    this.creating.set(true);
  }
  cancelPlan(): void {
    this.editingPlan.set(null);
    this.creating.set(false);
    this.planForm = { name: '', cap_7d_usd: '', cap_30d_usd: '' };
  }
  // A member row is dirty when its selected plan differs from the saved one.
  isDirty(m: Member): boolean {
    return (this.assignments[m.id] ?? '') !== (m.membership?.plan_id ?? '');
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
