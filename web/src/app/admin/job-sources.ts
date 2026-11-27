import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { DatePipe, DecimalPipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { injectMutation, injectQuery, QueryClient } from '@tanstack/angular-query';
import { Api, type Schemas } from '../core/api';
import { AuthService } from '../core/auth.service';
import { ZardBadgeComponent } from '@/shared/components/badge';
import { ZardButtonComponent } from '@/shared/components/button';
import { ZardCardImports } from '@/shared/components/card';
import { ZardSelectImports } from '@/shared/components/select';
import { ZardTableImports } from '@/shared/components/table';
import { ZardTextareaComponent } from '@/shared/components/textarea';

type Board = Schemas['AtsBoard'];
type Status = Board['status'];
type Interval = Schemas['SupplierInterval'];

const PAGE = 50;
const INTERVALS: { value: Interval; label: string }[] = [
  { value: 5, label: '5 min' },
  { value: 10, label: '10 min' },
  { value: 15, label: '15 min' },
  { value: 30, label: '30 min' },
  { value: 60, label: '1 hour' },
  { value: 360, label: '6 hours' },
  { value: 1440, label: '24 hours' },
];
const ATS_NAMES: Record<Board['ats'], string> = { greenhouse: 'Greenhouse', lever: 'Lever', ashby: 'Ashby' };
const single = (value: string | string[] | null): string => (Array.isArray(value) ? (value[0] ?? '') : (value ?? ''));

const sourcesKey = (identity: string | null) => ['admin-job-sources', identity] as const;
const boardsKey = (identity: string | null, status?: Status, page?: number) =>
  status === undefined ? (['admin-ats-boards', identity] as const) : (['admin-ats-boards', identity, status, page] as const);

// The suppliers that find postings for job alerts, how often each runs, and
// the ATS boards the board supplier reads: pending ones to approve, active
// ones and disabled ones.
@Component({
  selector: 'app-job-sources',
  imports: [
    DatePipe, DecimalPipe, FormsModule, ZardBadgeComponent, ZardButtonComponent, ZardTextareaComponent,
    ...ZardCardImports, ...ZardSelectImports, ...ZardTableImports,
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
  <div class="space-y-8">
    @if (error()) { <p role="alert" class="rounded-lg border border-destructive p-3 text-sm text-destructive">{{ error() }}</p> }

    <section aria-labelledby="suppliers-heading" class="space-y-3">
      <div class="flex flex-wrap items-baseline justify-between gap-3">
        <h2 id="suppliers-heading" class="text-lg font-semibold">Suppliers</h2>
        <button z-button zType="outline" [zDisabled]="fetching()" (click)="refresh()">{{ fetching() ? 'Refreshing…' : 'Refresh' }}</button>
      </div>
      <div class="overflow-x-auto rounded-lg border">
        <table z-table class="min-w-[640px]">
          <thead z-table-header>
            <tr z-table-row><th z-table-head>Supplier</th><th z-table-head>Runs every</th><th z-table-head>Last good run</th><th z-table-head>Credits</th><th z-table-head>Status</th></tr>
          </thead>
          <tbody z-table-body>
            @for (s of sources()?.suppliers ?? []; track s.name) {
              <tr z-table-row>
                <td z-table-cell><div class="font-medium">{{ s.name }}</div><div class="text-xs text-muted-foreground">{{ describe(s.name) }}</div></td>
                <td z-table-cell>
                  <z-select class="w-32" [attr.aria-label]="'Interval for ' + s.name" [ngModel]="'' + s.interval_minutes"
                    (ngModelChange)="setInterval(s.name, single($event))" [zDisabled]="saving()">
                    @for (i of intervals; track i.value) { <z-select-item [zValue]="'' + i.value">{{ i.label }}</z-select-item> }
                  </z-select>
                </td>
                <td z-table-cell>{{ s.last_ok_at ? (s.last_ok_at | date: 'MMM d, HH:mm') : 'never' }}</td>
                <td z-table-cell class="tabular-nums">{{ s.name === boardSupplier ? 'free' : (s.credits_total | number) }}</td>
                <td z-table-cell>
                  @if (s.name === boardSupplier) {
                    <z-badge zType="secondary">{{ sources()?.boards?.active ?? 0 }} boards</z-badge>
                    @if (s.failures) { <z-badge zType="outline" class="ml-1 text-amber-500">{{ s.failures }} failing</z-badge> }
                  } @else if (s.failures) {
                    <z-badge zType="destructive" [title]="s.last_error">Backing off · {{ s.last_error }}</z-badge>
                  } @else { <z-badge zType="secondary">OK</z-badge> }
                </td>
              </tr>
            } @empty {
              <tr z-table-row><td z-table-cell colspan="5" class="p-6 text-center text-muted-foreground">{{ fetching() ? 'Loading…' : 'No suppliers yet.' }}</td></tr>
            }
          </tbody>
        </table>
      </div>
      <p class="text-xs text-muted-foreground">A shorter interval applies now. Boards are read for free; failing boards back off up to a day.</p>
    </section>

    <section aria-labelledby="boards-heading" class="space-y-3">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h2 id="boards-heading" class="text-lg font-semibold">ATS boards</h2>
        <div class="flex flex-wrap gap-2" role="group" aria-label="Board status">
          @for (t of tabs(); track t.status) {
            <button z-button [zType]="status() === t.status ? 'secondary' : 'ghost'" [attr.aria-pressed]="status() === t.status" (click)="show(t.status)">
              {{ t.label }} {{ t.count | number }}
            </button>
          }
          <button z-button (click)="adding.set(!adding())">{{ adding() ? 'Close' : 'Add boards' }}</button>
        </div>
      </div>

      @if (adding()) {
        <z-card>
          <z-card-header>
            <z-card-title zTitle="Add boards" />
            <z-card-description zDescription="Paste Greenhouse, Lever or Ashby links, one per line. Each is read once to record what is already open; alerts start with what opens after that." />
          </z-card-header>
          <z-card-content class="grid gap-3">
            <textarea z-textarea rows="5" aria-label="Board links" placeholder="https://job-boards.greenhouse.io/acme&#10;https://jobs.lever.co/acme&#10;https://jobs.ashbyhq.com/acme" [(ngModel)]="links"></textarea>
            <div class="flex flex-wrap items-center gap-3">
              <button z-button [zDisabled]="!links.trim() || saving()" (click)="add()">{{ saving() ? 'Adding…' : 'Add' }}</button>
              <span class="text-sm text-muted-foreground" role="status">{{ addNotice() }}</span>
            </div>
          </z-card-content>
        </z-card>
      }
      @if (actionError()) { <p role="alert" class="text-sm text-destructive">{{ actionError() }}</p> }

      <div class="overflow-x-auto rounded-lg border">
        <table z-table class="min-w-[720px]">
          <thead z-table-header>
            <tr z-table-row>
              <th z-table-head>Board</th><th z-table-head>ATS</th>
              @if (status() === 'pending') { <th z-table-head>Found via</th> } @else { <th z-table-head>Open</th><th z-table-head>Last read</th> }
              <th z-table-head><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody z-table-body>
            @for (b of boards(); track b.ats + ':' + b.board) {
              <tr z-table-row>
                <td z-table-cell>
                  <div class="font-medium">{{ b.company || b.board }}</div>
                  <div class="text-xs text-muted-foreground">{{ b.board }} · {{ b.source === 'seed' ? 'added by you' : 'discovered' }}</div>
                </td>
                <td z-table-cell><z-badge zType="outline">{{ atsName(b.ats) }}</z-badge></td>
                @if (status() === 'pending') {
                  <td z-table-cell>{{ b.sightings | number }} {{ b.sightings === 1 ? 'search posting' : 'search postings' }}</td>
                } @else {
                  <td z-table-cell class="tabular-nums">{{ b.last_ok_at ? (b.open_count | number) : '—' }}</td>
                  <td z-table-cell>
                    {{ b.last_polled_at ? (b.last_polled_at | date: 'MMM d, HH:mm') : 'not yet' }}
                    @if (b.last_error) {
                      <z-badge [zType]="b.status === 'disabled' ? 'outline' : 'destructive'" class="ml-1">{{ b.last_error }}</z-badge>
                    } @else if (b.status === 'active' && !b.last_ok_at) { <z-badge zType="outline" class="ml-1">baseline pending</z-badge> }
                  </td>
                }
                <td z-table-cell class="text-right whitespace-nowrap">
                  @switch (b.status) {
                    @case ('pending') {
                      <button z-button zSize="sm" [zDisabled]="saving()" (click)="setStatus(b, 'active')">Approve</button>
                      <button z-button zSize="sm" zType="outline" class="ml-2" [zDisabled]="saving()" (click)="setStatus(b, 'disabled')">Reject</button>
                    }
                    @case ('active') {
                      <button z-button zSize="sm" zType="outline" [zDisabled]="saving()" (click)="poll(b)">Poll now</button>
                      <button z-button zSize="sm" zType="ghost" class="ml-2" [zDisabled]="saving()" (click)="setStatus(b, 'disabled')">Disable</button>
                    }
                    @case ('disabled') {
                      <button z-button zSize="sm" zType="outline" [zDisabled]="saving()" (click)="setStatus(b, 'active')">Enable</button>
                    }
                  }
                </td>
              </tr>
            } @empty {
              <tr z-table-row><td z-table-cell colspan="5" class="p-6 text-center text-muted-foreground">{{ emptyText() }}</td></tr>
            }
          </tbody>
        </table>
      </div>
      @if (total() > pageSize) {
        <div class="flex items-center justify-end gap-3 text-sm">
          <span class="text-muted-foreground tabular-nums">{{ page() * pageSize + 1 }}–{{ min((page() + 1) * pageSize, total()) }} of {{ total() | number }}</span>
          <button z-button zSize="sm" zType="outline" [zDisabled]="page() === 0" (click)="page.set(page() - 1)">Previous</button>
          <button z-button zSize="sm" zType="outline" [zDisabled]="(page() + 1) * pageSize >= total()" (click)="page.set(page() + 1)">Next</button>
        </div>
      }
      <p class="text-xs text-muted-foreground">Up to {{ (sources()?.boards?.limit ?? 2000) | number }} boards can be active. A board not found for a week, or a discovered one with no new postings in 60 days, is disabled. Rejected boards are never suggested again.</p>
    </section>
  </div>`,
  styles: [`:host { display: block; }`],
})
export class JobSources {
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  private readonly queries = inject(QueryClient);
  protected readonly single = single;
  protected readonly intervals = INTERVALS;
  protected readonly boardSupplier = 'ats:boards';
  protected readonly pageSize = PAGE;
  protected readonly min = Math.min;

  readonly status = signal<Status>('pending');
  readonly page = signal(0);
  readonly adding = signal(false);
  readonly addNotice = signal('');
  readonly actionError = signal('');
  links = '';

  private readonly sourcesQuery = injectQuery(() => ({
    queryKey: sourcesKey(this.auth.identity()),
    enabled: !!this.auth.identity(),
    refetchInterval: 60_000,
    queryFn: async () => Api.ok(await this.api.GET('/v1/admin/job-sources')),
  }));
  private readonly boardsQuery = injectQuery(() => ({
    queryKey: boardsKey(this.auth.identity(), this.status(), this.page()),
    enabled: !!this.auth.identity(),
    queryFn: async () =>
      Api.ok(await this.api.GET('/v1/admin/ats-boards', { params: { query: { status: this.status(), limit: PAGE, offset: this.page() * PAGE } } })),
  }));
  private readonly mutation = injectMutation(() => ({ mutationFn: async (run: () => Promise<unknown>) => run() }));

  readonly sources = computed(() => this.sourcesQuery.data() ?? null);
  readonly boards = computed(() => this.boardsQuery.data()?.boards ?? []);
  readonly total = computed(() => this.boardsQuery.data()?.total ?? 0);
  readonly fetching = computed(() => this.sourcesQuery.isFetching() || this.boardsQuery.isFetching());
  readonly saving = computed(() => this.mutation.isPending());
  readonly error = computed(() => (this.sourcesQuery.error() ?? this.boardsQuery.error())?.message ?? '');
  readonly tabs = computed(() => {
    const c = this.sources()?.boards;
    return [
      { status: 'pending' as Status, label: 'Pending', count: c?.pending ?? 0 },
      { status: 'active' as Status, label: 'Active', count: c?.active ?? 0 },
      { status: 'disabled' as Status, label: 'Disabled', count: c?.disabled ?? 0 },
    ];
  });
  readonly emptyText = computed(() => {
    if (this.boardsQuery.isFetching()) return 'Loading…';
    switch (this.status()) {
      case 'pending': return 'Nothing to review. Boards that search postings link to show up here.';
      case 'active': return 'No active boards. Add some, or approve pending ones.';
      default: return 'No disabled boards.';
    }
  });

  describe(name: string): string {
    switch (name) {
      case 'theirstack:countries': return 'TheirStack · on-site and hybrid in members’ countries';
      case 'theirstack:remote': return 'TheirStack · remote from anywhere';
      case 'ats:boards': return 'Greenhouse, Lever and Ashby boards';
      default: return '';
    }
  }
  atsName(ats: Board['ats']): string { return ATS_NAMES[ats]; }

  show(status: Status): void {
    this.status.set(status);
    this.page.set(0);
    this.actionError.set('');
  }

  refresh(): void {
    const identity = this.auth.identity();
    void this.queries.invalidateQueries({ queryKey: sourcesKey(identity) });
    void this.queries.invalidateQueries({ queryKey: boardsKey(identity) });
  }

  private async run(action: () => Promise<unknown>): Promise<boolean> {
    const identity = this.auth.identity();
    this.actionError.set('');
    try {
      await this.mutation.mutateAsync(action);
      if (identity === this.auth.identity()) this.refresh();
      return true;
    } catch (e) {
      if (identity === this.auth.identity()) this.actionError.set((e as Error).message);
      return false;
    }
  }

  setInterval(name: string, value: string): void {
    const minutes = Number(value) as Interval;
    const current = this.sources()?.suppliers.find((s) => s.name === name)?.interval_minutes;
    if (!minutes || minutes === current) return;
    void this.run(async () => Api.ok(await this.api.PUT('/v1/admin/job-sources/interval', { body: { name, interval_minutes: minutes } })));
  }

  setStatus(b: Board, status: 'active' | 'disabled'): void {
    void this.run(async () =>
      Api.ok(await this.api.PUT('/v1/admin/ats-boards/{ats}/{board}/status', { params: { path: { ats: b.ats, board: b.board } }, body: { status } })),
    );
  }

  poll(b: Board): void {
    void this.run(async () => Api.ok(await this.api.POST('/v1/admin/ats-boards/{ats}/{board}/poll', { params: { path: { ats: b.ats, board: b.board } } })));
  }

  async add(): Promise<void> {
    let result: Schemas['AtsBoardsAdded'] | null = null;
    const ok = await this.run(async () => {
      result = Api.ok(await this.api.POST('/v1/admin/ats-boards', { body: { links: this.links } }));
    });
    if (!ok || !result) return;
    const r: Schemas['AtsBoardsAdded'] = result;
    const parts = [`${r.added} added`];
    if (r.activated) parts.push(`${r.activated} re-enabled`);
    if (r.invalid.length) parts.push(`${r.invalid.length} not recognized: ${r.invalid.slice(0, 3).join(', ')}${r.invalid.length > 3 ? '…' : ''}`);
    this.addNotice.set(parts.join(' · '));
    this.links = r.invalid.join('\n');
    if (r.added || r.activated) this.show('active');
  }
}
