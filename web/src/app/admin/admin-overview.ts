import { ChangeDetectionStrategy, Component, computed, effect, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { usd } from '../usage/format';
import { fetchAllMembers } from './members';

type Plan = Schemas['Plan'];
type Member = Schemas['Member'];
type Attempt = Schemas['AttemptSummary'];
type SystemKey = Schemas['SystemKey'];
type Settings = Schemas['ModelSettings'];

interface Attention {
  label: string;
  action: string;
  link: string;
}

// Attempts are listed in pages of this size; a full page means there may be more.
const ATTEMPT_PAGE = 50;

/** Status landing page: what is live, who needs attention, and where to act. */
@Component({
  selector: 'app-admin-overview',
  imports: [RouterLink, ZardButtonComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './admin-overview.html',
  styles: [
    `
      .card {
        display: flex;
        min-width: 0;
        flex-direction: column;
        gap: 0.25rem;
        border: 1px solid var(--border);
        border-radius: 0.75rem;
        background: var(--background);
        padding: 1.25rem;
        color: inherit;
        text-decoration: none;
      }
      .card:hover {
        border-color: var(--muted-foreground);
      }
      .card:focus-visible {
        outline: 2px solid var(--ring);
        outline-offset: 2px;
      }
      .card-label {
        font-size: 0.75rem;
        letter-spacing: 0.06em;
        text-transform: uppercase;
        color: var(--muted-foreground);
      }
      .card-value {
        font-size: 1.125rem;
        font-weight: 600;
        overflow-wrap: anywhere;
      }
      .card-meta {
        font-size: 0.8125rem;
        color: var(--muted-foreground);
      }
    `,
  ],
})
export class AdminOverview {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  protected readonly loading = signal(false);
  protected readonly error = signal('');
  protected readonly plans = signal<Plan[]>([]);
  protected readonly members = signal<Member[]>([]);
  protected readonly keys = signal<SystemKey[]>([]);
  protected readonly attempts = signal<Attempt[]>([]);
  protected readonly attemptsCapped = signal(false);
  protected readonly settings = signal<Settings | null>(null);
  private generation = 0;

  protected readonly unassigned = computed(() => this.members().filter((m) => !m.membership).length);
  protected readonly activeKey = computed(() => this.keys().find((k) => k.active) ?? null);
  protected readonly reserved = computed(() =>
    usd(this.attempts().reduce((sum, a) => sum + Number(a.reserved_usd), 0).toFixed(2)),
  );
  protected readonly attention = computed<Attention[]>(() => {
    const items: Attention[] = [];
    const attempts = this.attempts().length;
    if (attempts)
      items.push({
        label: `${attempts}${this.attemptsCapped() ? '+' : ''} model call${attempts === 1 ? '' : 's'} need a confirmed charge`,
        action: 'Resolve',
        link: '/admin/usage',
      });
    if (this.unassigned())
      items.push({
        label: `${this.unassigned()} member${this.unassigned() === 1 ? ' has' : 's have'} no plan`,
        action: 'Assign a plan',
        link: '/admin/members',
      });
    if (this.settings() && this.keys().length && !this.activeKey())
      items.push({
        label: 'No OpenRouter key is active, so model calls cannot run',
        action: 'Review keys',
        link: '/admin/keys',
      });
    return items;
  });

  constructor() {
    effect(() => {
      this.auth.identity();
      void this.load();
    });
  }

  async load(): Promise<void> {
    const identity = this.auth.identity(),
      generation = ++this.generation;
    if (!identity) return;
    const current = () => identity === this.auth.identity() && generation === this.generation;
    this.loading.set(true);
    this.error.set('');
    try {
      const [plans, settings, keys, attempts, members] = await Promise.all([
        this.api.GET('/v1/admin/plans'),
        this.api.GET('/v1/admin/model-settings'),
        this.api.GET('/v1/admin/system-keys'),
        this.api.GET('/v1/admin/model-attempts', { params: { query: { limit: ATTEMPT_PAGE } } }),
        fetchAllMembers(this.api),
      ]);
      if (!current()) return;
      const list = Api.ok(attempts);
      this.plans.set(Api.ok(plans));
      this.settings.set(Api.ok(settings));
      this.keys.set(Api.ok(keys));
      this.attempts.set(list);
      this.attemptsCapped.set(list.length === ATTEMPT_PAGE);
      this.members.set(members);
    } catch (e) {
      if (current()) this.error.set(e instanceof Error ? e.message : 'Could not load the overview.');
    } finally {
      if (current()) this.loading.set(false);
    }
  }

  summaryModel(): string {
    return this.settings()?.summary_model ?? 'Not configured';
  }
  decisionModel(): string {
    return this.settings()?.decision_model ?? 'Not configured';
  }
  extractionModel(): string {
    return this.settings()?.extraction_model ?? 'Off';
  }
}
