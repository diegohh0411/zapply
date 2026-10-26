import { ChangeDetectionStrategy, Component, computed, effect, inject, input, model, output, signal, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucidePlus, lucideTrash2 } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSelectImports } from '@/shared/components/select';
import { ZardSheetImports } from '@/shared/components/sheet';
import { ZardSonnerService } from '@/shared/components/sonner';
import { ZardSwitchComponent } from '@/shared/components/switch';
import { ZardTextareaComponent } from '@/shared/components/textarea';

import { Api, ApiError, etagOf, type Schemas } from '../core/api';
import { fieldMap } from './live';

type Kind = Schemas['Kind'];
type Category = Schemas['Category'];
type Evidence = Schemas['Evidence'];

/** Existing evidence, or a new one in a category (optionally under a parent). */
export type EvidenceTarget = { id: string } | { new: true; category: Category; parent?: Evidence };

interface Highlight {
  id?: string;
  text: string;
  metric_value: number | null;
  metric_unit: string;
  auto_include: boolean;
}

interface Form {
  kind: string;
  title: string;
  organization_id: string;
  start: string;
  end: string;
  is_current: boolean;
  location: string;
  url: string;
  summary: string;
  details: Record<string, string>;
  skills: string;
  auto_include: boolean;
  highlights: Highlight[];
}

interface DetailField {
  name: string;
  label: string;
  type: 'text' | 'enum' | 'number' | 'list';
  options: string[];
  required: boolean;
}

export const humanize = (s: string) => (s.charAt(0).toUpperCase() + s.slice(1)).replace(/_/g, ' ');

const DATE_LABELS: Record<Schemas['Form'], [string, string | null]> = {
  engagement: ['Start', 'End'],
  credential: ['Issued', 'Expires'],
  output: ['Published', null],
  recognition: ['Granted', null],
};

@Component({
  selector: 'app-evidence-sheet',
  imports: [
    FormsModule,
    NgIcon,
    ZardButtonComponent,
    ZardInputComponent,
    ZardTextareaComponent,
    ZardSwitchComponent,
    ...ZardSheetImports,
    ...ZardSelectImports,
  ],
  providers: [provideIcons({ lucidePlus, lucideTrash2 })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './evidence-sheet.html',
})
export class EvidenceSheet {
  private readonly api = inject(Api).client;
  private readonly toast = inject(ZardSonnerService);

  readonly target = input<EvidenceTarget | null>(null);
  readonly kinds = input.required<Kind[]>();
  readonly organizations = input.required<Schemas['Organization'][]>();
  readonly visible = model(false);
  readonly saved = output<void>();

  protected readonly humanize = humanize;
  protected form: Form = blank();
  protected readonly kind = signal('');
  protected readonly category = signal<Category>('paid');
  protected readonly parent = signal<Evidence | null>(null);
  protected readonly existing = signal<Evidence | null>(null);
  protected readonly saving = signal(false);
  protected readonly errors = signal<Record<string, string>>({});
  private etag = '';

  /** Kinds allowed here: by category, and nested kinds only under a parent. */
  protected readonly allowedKinds = computed(() => {
    const cat = this.category();
    const nested = !!this.parent() || !!this.existing()?.parent_id;
    return this.kinds().filter(
      (k) =>
        k.categories.includes(cat) &&
        (nested ? k.form === 'output' || k.form === 'recognition' || k.kind === 'assignment' : !k.child_only),
    );
  });

  protected readonly kindInfo = computed(() => this.kinds().find((k) => k.kind === this.kind()));
  protected readonly dateLabels = computed(() => DATE_LABELS[this.kindInfo()?.form ?? 'engagement']);

  protected readonly fields = computed<DetailField[]>(() => {
    const schema = this.kindInfo()?.details_schema as
      | { properties?: Record<string, { type: string; enum?: string[] }>; required?: string[] }
      | undefined;
    if (!schema?.properties) return [];
    return Object.entries(schema.properties).map(([name, p]) => ({
      name,
      label: humanize(name),
      type: p.enum ? 'enum' : p.type === 'array' ? 'list' : p.type === 'integer' || p.type === 'number' ? 'number' : 'text',
      options: p.enum ?? [],
      required: schema.required?.includes(name) ?? false,
    }));
  });

  constructor() {
    effect(() => {
      const t = this.target();
      untracked(() => void this.load(t));
    });
  }

  private async load(t: EvidenceTarget | null): Promise<void> {
    this.errors.set({});
    if (!t) return;
    if ('new' in t) {
      this.existing.set(null);
      this.parent.set(t.parent ?? null);
      this.category.set(t.parent?.category ?? t.category);
      this.form = blank();
      this.kind.set(this.allowedKinds()[0]?.kind ?? '');
      this.form.kind = this.kind();
      this.form.auto_include = this.form.kind !== 'career_break';
      return;
    }
    try {
      const r = await this.api.GET('/v1/evidences/{id}', { params: { path: { id: t.id } } });
      const e = Api.ok(r);
      this.etag = etagOf(r);
      this.existing.set(e);
      this.parent.set(null);
      this.category.set(e.category);
      this.kind.set(e.kind);
      this.form = toForm(e);
    } catch (e) {
      this.toast.error((e as Error).message);
      this.visible.set(false);
    }
  }

  protected setKind(k: string): void {
    this.kind.set(k);
    this.form.kind = k;
    this.form.details = {};
    if (k === 'career_break') this.form.auto_include = false;
  }

  protected addHighlight(): void {
    this.form.highlights = [...this.form.highlights, { text: '', metric_value: null, metric_unit: '', auto_include: true }];
  }

  protected removeHighlight(i: number): void {
    this.form.highlights = this.form.highlights.filter((_, j) => j !== i);
  }

  protected async save(): Promise<void> {
    this.saving.set(true);
    this.errors.set({});
    const body = this.toBody();
    try {
      const cur = this.existing();
      if (cur) {
        const r = await this.api.PUT('/v1/evidences/{id}', {
          params: { path: { id: cur.id }, header: { 'If-Match': this.etag } },
          body,
        });
        Api.ok(r);
      } else {
        Api.ok(
          await this.api.POST('/v1/evidences', {
            params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
            body: { ...body, parent_id: this.parent()?.id },
          }),
        );
      }
      this.toast.success('Saved');
      this.saved.emit();
      this.visible.set(false);
    } catch (e) {
      if (e instanceof ApiError && e.status === 412) {
        this.toast.warning('This entry changed elsewhere; reloaded it.');
        const cur = this.existing();
        if (cur) await this.load({ id: cur.id });
      } else {
        this.errors.set(fieldMap(e));
        this.toast.error(e instanceof ApiError && e.fieldErrors.length ? 'Check the highlighted fields' : (e as Error).message);
      }
    } finally {
      this.saving.set(false);
    }
  }

  private toBody(): Schemas['EvidenceUpdate'] {
    const f = this.form;
    const details: Record<string, unknown> = {};
    for (const fd of this.fields()) {
      const v = (f.details[fd.name] ?? '').toString().trim();
      if (!v) continue;
      details[fd.name] =
        fd.type === 'number' ? Number(v) : fd.type === 'list' ? v.split(',').map((x) => x.trim()).filter(Boolean) : v;
    }
    const isEngagement = this.kindInfo()?.form === 'engagement';
    return {
      kind: f.kind,
      category: this.category(),
      title: f.title,
      organization_id: f.organization_id || undefined,
      start: f.start.trim() || undefined,
      end: f.is_current ? undefined : f.end.trim() || undefined,
      is_current: isEngagement && f.is_current,
      location: f.location,
      url: f.url,
      summary: f.summary,
      details,
      auto_include: f.auto_include,
      skills: f.skills.split(',').map((s) => s.trim()).filter(Boolean),
      highlights: f.highlights
        .filter((h) => h.text.trim())
        .map((h) => ({
          id: h.id,
          text: h.text.trim(),
          metric_value: h.metric_value === null || (h.metric_value as unknown) === '' ? undefined : +h.metric_value,
          metric_unit: h.metric_unit,
          auto_include: h.auto_include,
        })),
    };
  }
}

function blank(): Form {
  return {
    kind: '',
    title: '',
    organization_id: '',
    start: '',
    end: '',
    is_current: false,
    location: '',
    url: '',
    summary: '',
    details: {},
    skills: '',
    auto_include: true,
    highlights: [],
  };
}

function toForm(e: Evidence): Form {
  const details: Record<string, string> = {};
  for (const [k, v] of Object.entries(e.details ?? {})) details[k] = Array.isArray(v) ? v.join(', ') : String(v);
  return {
    kind: e.kind,
    title: e.title,
    organization_id: e.organization_id ?? '',
    start: e.start ?? '',
    end: e.end ?? '',
    is_current: e.is_current,
    location: e.location,
    url: e.url,
    summary: e.summary,
    details,
    skills: e.skills.join(', '),
    auto_include: e.auto_include,
    highlights: e.highlights.map((h) => ({ id: h.id, text: h.text, metric_value: h.metric_value ?? null, metric_unit: h.metric_unit, auto_include: h.auto_include })),
  };
}
