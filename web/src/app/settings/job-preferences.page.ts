import { DatePipe } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  computed,
  DestroyRef,
  inject,
  signal,
} from '@angular/core';
import { FormsModule } from '@angular/forms';

import { ZardBadgeComponent } from '@/shared/components/badge';
import { ZardButtonComponent } from '@/shared/components/button';
import { ZardCheckboxComponent } from '@/shared/components/checkbox';
import { ZardComboboxImports, type ZardComboboxOption } from '@/shared/components/combobox';
import { ZardSkeletonComponent } from '@/shared/components/skeleton';
import { ZardSonnerService } from '@/shared/components/sonner';
import { ZardSwitchComponent } from '@/shared/components/switch';
import { ZardButtonGroupComponent } from '@/shared/components/button-group';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideUndo2 } from '@ng-icons/lucide';
import { Api, ApiError, type Schemas } from '../core/api';
import {
  AREA_LABELS,
  COUNTRY_CODES,
  EMPLOYMENT_LABELS,
  LEVEL_LABELS,
  countryName,
} from './job-preferences.data';

type Prefs = Schemas['JobPreferences'];
type Level = Schemas['SeniorityLevel'];
type Employment = Schemas['PostingEmploymentType'];
type Field = Schemas['JobPreferenceField'];

interface Draft {
  min: Level | null;
  max: Level | null;
  areas: string[];
  countries: string[];
  remote: boolean;
  skip: Employment[];
}

const LEVELS = Object.keys(LEVEL_LABELS) as Level[];

/** Settings › Job preferences: what the member is looking for, pre-filled from their library. */
@Component({
  selector: 'app-job-preferences',
  imports: [
    DatePipe,
    FormsModule,
    ZardBadgeComponent,
    ZardButtonComponent,
    ZardCheckboxComponent,
    ...ZardComboboxImports,
    ZardSkeletonComponent,
    ZardSwitchComponent,
    ZardButtonGroupComponent,
    NgIcon,
  ],
  providers: [provideIcons({ lucideUndo2 })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-[64rem] px-6 pt-7 pb-12">
      <section aria-labelledby="prefs-title" class="rounded-xl border p-5">
        <h2 id="prefs-title" class="text-sm font-semibold">What I'm looking for</h2>
        <p class="mt-1 text-sm text-muted-foreground">
          Suggested from your Experience library. What you change stays yours; the rest keeps
          following your library as it grows.
        </p>

        @if (prefs(); as p) {
          <dl class="mt-4 divide-y">
            <div class="row">
              <dt>
                Seniority
                @if (badge('seniority'); as b) {
                  <z-badge [zType]="b.type">{{ b.label }}</z-badge>
                }
                @if (p.sources.seniority === 'member') {
                  <button
                    z-button
                    zType="ghost"
                    zSize="icon-xs"
                    aria-label="Use the suggestion"
                    title="Use the suggestion"
                    (click)="reset(['seniority'])"
                  >
                    <ng-icon name="lucideUndo2" />
                  </button>
                }
              </dt>
              <dd>
                <z-button-group class="max-w-full overflow-x-auto" aria-label="Seniority range">
                  @for (l of levelItems; track l.value) {
                    <button
                      z-button
                      zSize="sm"
                      [zType]="inRange(l.value) ? 'default' : 'outline'"
                      [attr.aria-pressed]="inRange(l.value)"
                      (click)="pickLevel(l.value)"
                    >
                      {{ l.label }}
                    </button>
                  }
                </z-button-group>
                <span class="text-sm text-muted-foreground">{{ yearsText() }}</span>
              </dd>
            </div>

            <div class="row">
              <dt>
                Areas
                @if (badge('areas'); as b) {
                  <z-badge [zType]="b.type">{{ b.label }}</z-badge>
                }
                @if (p.sources.areas === 'member') {
                  <button
                    z-button
                    zType="ghost"
                    zSize="icon-xs"
                    aria-label="Use the suggestion"
                    title="Use the suggestion"
                    (click)="reset(['areas'])"
                  >
                    <ng-icon name="lucideUndo2" />
                  </button>
                }
              </dt>
              <dd>
                <z-combobox
                  zMultiple
                  class="w-full sm:max-w-xl"
                  ariaLabel="Areas"
                  emptyText="No area matches."
                  [options]="areaOptions"
                  [zValue]="draft().areas"
                  (zValueChange)="setAreas($event)"
                >
                  <z-combobox-chips>
                    @for (a of draft().areas; track a) {
                      <z-combobox-chip [zValue]="a">{{ areaLabel(a) }}</z-combobox-chip>
                    }
                    <input
                      z-combobox-chips-input
                      [placeholder]="
                        draft().areas.length ? 'Add area' : 'Pick the areas you work in'
                      "
                    />
                  </z-combobox-chips>
                  <z-combobox-content>
                    <z-combobox-empty>No area matches.</z-combobox-empty>
                    <z-combobox-list>
                      @for (o of areaOptions; track o.value) {
                        <z-combobox-item [zValue]="o.value" [zLabel]="o.label">{{
                          o.label
                        }}</z-combobox-item>
                      }
                    </z-combobox-list>
                  </z-combobox-content>
                </z-combobox>
                @if (p.deriving) {
                  <span class="text-sm text-muted-foreground">Reading your library…</span>
                }
              </dd>
            </div>

            <div class="row">
              <dt>
                Countries
                @if (badge('location'); as b) {
                  <z-badge [zType]="b.type">{{ b.label }}</z-badge>
                }
                @if (p.sources.location === 'member') {
                  <button
                    z-button
                    zType="ghost"
                    zSize="icon-xs"
                    aria-label="Use the suggestion"
                    title="Use the suggestion"
                    (click)="reset(['location'])"
                  >
                    <ng-icon name="lucideUndo2" />
                  </button>
                }
              </dt>
              <dd>
                <z-combobox
                  zMultiple
                  class="w-full sm:max-w-md"
                  ariaLabel="Countries"
                  emptyText="No country matches."
                  [options]="countryOptions"
                  [zValue]="draft().countries"
                  (zValueChange)="setCountries($event)"
                >
                  <z-combobox-chips>
                    @for (c of draft().countries; track c) {
                      <z-combobox-chip [zValue]="c">{{ countryName(c) }}</z-combobox-chip>
                    }
                    <input
                      z-combobox-chips-input
                      [placeholder]="draft().countries.length ? 'Add country' : 'Any country'"
                    />
                  </z-combobox-chips>
                  <z-combobox-content>
                    <z-combobox-empty>No country matches.</z-combobox-empty>
                    <z-combobox-list>
                      @for (o of countryOptions; track o.value) {
                        <z-combobox-item [zValue]="o.value" [zLabel]="o.label">{{
                          o.label
                        }}</z-combobox-item>
                      }
                    </z-combobox-list>
                  </z-combobox-content>
                </z-combobox>
                <label class="flex items-center gap-2 text-sm">
                  <z-switch
                    zSize="sm"
                    [zChecked]="draft().remote"
                    (zCheckedChange)="patch({ remote: $event })"
                  />
                  Remote roles anywhere
                </label>
              </dd>
            </div>

            <div class="row">
              <dt>
                Skip
                @if (badge('employment'); as b) {
                  <z-badge [zType]="b.type">{{ b.label }}</z-badge>
                }
                @if (p.sources.employment === 'member') {
                  <button
                    z-button
                    zType="ghost"
                    zSize="icon-xs"
                    aria-label="Use the suggestion"
                    title="Use the suggestion"
                    (click)="reset(['employment'])"
                  >
                    <ng-icon name="lucideUndo2" />
                  </button>
                }
              </dt>
              <dd>
                @for (e of employment; track e) {
                  <label class="flex items-center gap-2 text-sm">
                    <z-checkbox
                      [ngModel]="draft().skip.includes(e)"
                      (ngModelChange)="toggleSkip(e, $event)"
                    />
                    {{ employmentLabel(e) }}
                  </label>
                }
              </dd>
            </div>
          </dl>

          <div class="mt-4 flex flex-wrap items-center justify-between gap-3 border-t pt-4">
            <p class="text-xs text-muted-foreground">
              @if (p.derived_at) {
                Last suggested from your library {{ p.derived_at | date: 'MMM d, h:mm a' }}
              }
            </p>
            <div class="flex gap-2">
              <button
                z-button
                zType="outline"
                [zDisabled]="busy() || !anyMember()"
                (click)="reset(fields)"
              >
                Reset all to suggestions
              </button>
              <button z-button [zDisabled]="busy() || !dirty()" (click)="save()">Save</button>
            </div>
          </div>
        } @else {
          <div class="mt-4 space-y-3">
            @for (i of [1, 2, 3, 4]; track i) {
              <z-skeleton class="h-9 w-full" />
            }
          </div>
        }
      </section>
    </div>
  `,
  styles: `
    .row {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem 1rem;
      padding: 0.875rem 0;
    }
    .row dt {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      width: 11rem;
      flex: none;
      font-size: 0.875rem;
      font-weight: 500;
      min-height: 2rem;
    }
    .row dd {
      display: flex;
      flex: 1 1 20rem;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem 1rem;
      min-width: 0;
    }
  `,
})
export class JobPreferencesPage {
  private readonly api = inject(Api).client;
  private readonly toast = inject(ZardSonnerService);

  protected readonly prefs = signal<Prefs | null>(null);
  protected readonly draft = signal<Draft>({
    min: null,
    max: null,
    areas: [],
    countries: [],
    remote: true,
    skip: [],
  });
  protected readonly busy = signal(false);
  /** Field groups the member changed here and hasn't saved. */
  private readonly touched = signal<ReadonlySet<Field>>(new Set());

  protected readonly fields: Field[] = ['seniority', 'areas', 'location', 'employment'];
  protected readonly employment = Object.keys(EMPLOYMENT_LABELS) as Employment[];
  protected readonly levelItems = LEVELS.map((l) => ({ value: l, label: LEVEL_LABELS[l] }));
  protected readonly areaOptions: ZardComboboxOption[] = Object.entries(AREA_LABELS).map(
    ([value, label]) => ({ value, label }),
  );
  protected readonly countryOptions: ZardComboboxOption[] = COUNTRY_CODES.map((c) => ({
    value: c,
    label: countryName(c),
  })).sort((a, b) => a.label.localeCompare(b.label));
  protected readonly countryName = countryName;

  protected readonly dirty = computed(() => this.touched().size > 0);
  protected readonly anyMember = computed(() => {
    const p = this.prefs();
    return !!p && Object.values(p.sources).some((s) => s === 'member');
  });
  protected readonly rangeValue = computed(() => {
    const { min, max } = this.draft();
    if (!min || !max) return [];
    return LEVELS.slice(LEVELS.indexOf(min), LEVELS.indexOf(max) + 1);
  });
  protected readonly yearsText = computed(() => {
    const s = this.prefs()?.suggested;
    if (!s) return '';
    if (s.work_years === 0) return s.enrolled ? 'Studying, no work yet' : 'No dated work yet';
    const y = s.work_years < 1 ? 'Under a year' : `≈ ${Math.round(s.work_years * 10) / 10} years`;
    return `${y} of work${s.enrolled ? ', studying' : ''}`;
  });

  constructor() {
    void this.load();
    // While the area suggestion is being read, check back.
    const timer = setInterval(() => {
      if (this.prefs()?.deriving && !this.busy()) void this.load(true);
    }, 3000);
    inject(DestroyRef).onDestroy(() => clearInterval(timer));
  }

  protected badge(f: Field): { label: string; type: 'secondary' | 'default' } | null {
    const p = this.prefs();
    if (!p) return null;
    if (this.touched().has(f)) return { label: 'edited', type: 'default' };
    return p.sources[f] === 'member'
      ? { label: 'yours', type: 'default' }
      : { label: 'suggested', type: 'secondary' };
  }

  protected areaLabel(a: string): string {
    return AREA_LABELS[a] ?? a;
  }
  protected employmentLabel(e: Employment): string {
    return EMPLOYMENT_LABELS[e];
  }

  protected patch(d: Partial<Draft>): void {
    this.draft.update((v) => ({ ...v, ...d }));
    const f: Field =
      'min' in d ? 'seniority' : 'areas' in d ? 'areas' : 'skip' in d ? 'employment' : 'location';
    this.touched.update((s) => new Set([...s, f]));
  }

  /** Clicking a level outside the range extends it; clicking an end shrinks it. */
  protected inRange(l: Level): boolean {
    return this.rangeValue().includes(l);
  }

  protected pickLevel(clicked: Level): void {
    const i = LEVELS.indexOf(clicked);
    const { min, max } = this.draft();
    if (!min || !max) return this.patch({ min: clicked, max: clicked });
    const lo = LEVELS.indexOf(min),
      hi = LEVELS.indexOf(max);
    let nlo = lo,
      nhi = hi;
    if (i < lo) nlo = i;
    else if (i > hi) nhi = i;
    else if (i === lo && lo < hi) nlo = lo + 1;
    else if (i === hi && lo < hi) nhi = hi - 1;
    else if (lo < i && i < hi) nhi = i;
    this.patch({ min: LEVELS[nlo], max: LEVELS[nhi] });
  }

  protected setAreas(v: string | string[] | null): void {
    this.patch({ areas: Array.isArray(v) ? v : v ? [v] : [] });
  }
  protected setCountries(v: string | string[] | null): void {
    this.patch({ countries: Array.isArray(v) ? v : v ? [v] : [] });
  }
  protected toggleSkip(e: Employment, on: boolean): void {
    const skip = this.draft().skip.filter((x) => x !== e);
    this.patch({ skip: on ? [...skip, e] : skip });
  }

  private apply(p: Prefs, keepEdits = false): void {
    this.prefs.set(p);
    const fresh: Draft = {
      min: p.seniority_min ?? null,
      max: p.seniority_max ?? null,
      areas: [...p.areas],
      countries: [...p.countries],
      remote: p.remote_ok,
      skip: [...p.excluded_employment],
    };
    if (!keepEdits) {
      this.draft.set(fresh);
      this.touched.set(new Set());
      return;
    }
    // A background refresh keeps what the member is editing.
    const t = this.touched(),
      d = this.draft();
    this.draft.set({
      min: t.has('seniority') ? d.min : fresh.min,
      max: t.has('seniority') ? d.max : fresh.max,
      areas: t.has('areas') ? d.areas : fresh.areas,
      countries: t.has('location') ? d.countries : fresh.countries,
      remote: t.has('location') ? d.remote : fresh.remote,
      skip: t.has('employment') ? d.skip : fresh.skip,
    });
  }

  private async load(keepEdits = false): Promise<void> {
    try {
      this.apply(Api.ok(await this.api.GET('/v1/job-preferences')), keepEdits);
    } catch (e) {
      this.toast.error((e as Error).message);
    }
  }

  protected async save(): Promise<void> {
    const p = this.prefs();
    if (!p || this.busy()) return;
    const t = this.touched(),
      d = this.draft();
    const body: Schemas['JobPreferencesInput'] = {};
    if (t.has('seniority') && d.min && d.max) body.seniority = { min: d.min, max: d.max };
    if (t.has('areas')) body.areas = d.areas;
    if (t.has('location')) body.location = { countries: d.countries, remote_ok: d.remote };
    if (t.has('employment')) body.excluded_employment = d.skip;
    await this.write(
      () =>
        this.api.PUT('/v1/job-preferences', {
          params: { header: { 'If-Match': `"${p.version}"` } },
          body,
        }),
      'Preferences saved',
    );
  }

  protected async reset(fields: Field[]): Promise<void> {
    const p = this.prefs();
    if (!p || this.busy()) return;
    await this.write(
      () =>
        this.api.POST('/v1/job-preferences/reset', {
          params: { header: { 'If-Match': `"${p.version}"` } },
          body: { fields },
        }),
      fields.length > 1 ? 'Back to suggestions' : 'Using the suggestion',
    );
  }

  private async write(
    call: () => Promise<{ data?: Prefs; error?: unknown; response: Response }>,
    done: string,
  ): Promise<void> {
    this.busy.set(true);
    try {
      this.apply(Api.ok(await call()));
      this.toast.success(done);
    } catch (e) {
      if (e instanceof ApiError && e.status === 412) {
        this.toast.warning('Your preferences changed elsewhere; reloaded them.');
        await this.load();
      } else {
        this.toast.error((e as Error).message);
      }
    } finally {
      this.busy.set(false);
    }
  }
}
