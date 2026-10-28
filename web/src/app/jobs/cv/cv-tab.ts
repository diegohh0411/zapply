import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  computed,
  effect,
  inject,
  input,
  signal,
  untracked,
} from '@angular/core';
import { DatePipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { DomSanitizer, type SafeResourceUrl } from '@angular/platform-browser';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideChevronDown,
  lucideDownload,
  lucidePin,
  lucidePlus,
  lucideX,
} from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { Api, ApiError, type Schemas } from '../../core/api';
import { EventsService } from '../../core/events.service';
import { AuthService } from '../../core/auth.service';
import {
  applyDrops,
  filename,
  fit,
  sectionLabels,
  type CvContent,
  type CvEntry,
  type Fit,
} from './cv-content';
import { TypstService, type Compiled, type CvTemplate } from './typst.service';

type Preview = Schemas['CvPreview'];
type Export = Schemas['CvExport'];

interface Row {
  id: string;
  label: string;
  depth: number;
  highlight: boolean;
  state: 'shown' | 'trimmed' | 'pinned';
}

/** A job's CV: preview, fitting, picks and exports (part 7). */
@Component({
  selector: 'app-cv-tab',
  imports: [DatePipe, FormsModule, ZardButtonComponent, NgIcon],
  viewProviders: [
    provideIcons({ lucideChevronDown, lucideDownload, lucidePin, lucidePlus, lucideX }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './cv-tab.html',
})
export class CvTab {
  readonly jobId = input.required<string>();
  readonly company = input('');
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  private readonly typst = inject(TypstService);
  private readonly sanitizer = inject(DomSanitizer);

  protected readonly preview = signal<Preview | undefined>(undefined);
  protected readonly fitted = signal<(Fit & { url: SafeResourceUrl; pdf: Uint8Array }) | undefined>(
    undefined,
  );
  protected readonly fitting = signal(false);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly notice = signal('');
  protected readonly exports = signal<Export[]>([]);
  private templates?: Promise<CvTemplate[]>;
  private url?: string;
  private generation = 0;
  private loadVersion = 0;
  private destroyed = false;
  private exportsVersion = 0;

  private scope(): string {
    return `${this.auth.identity()}|${this.jobId()}`;
  }

  private current(scope: string): boolean {
    return !this.destroyed && scope === this.scope();
  }

  protected readonly content = computed(() => this.preview()?.content as CvContent | undefined);
  protected readonly dropped = computed(() => {
    const p = this.preview();
    const f = this.fitted();
    return new Set(p && f ? p.trim.slice(0, f.k) : []);
  });
  protected readonly rows = computed(() => {
    const p = this.preview();
    const content = this.content();
    if (!p || !content) return [];
    const pinned = new Set(p.draft.picks.include);
    const dropped = this.dropped();
    const state = (id: string): Row['state'] =>
      pinned.has(id) ? 'pinned' : dropped.has(id) ? 'trimmed' : 'shown';
    return content.sections.map((s) => {
      const rows: Row[] = [];
      const walk = (e: CvEntry, depth: number, parentDropped = false) => {
        const trimmed = parentDropped || dropped.has(e.id);
        rows.push({
          id: e.id,
          label: e.title,
          depth,
          highlight: false,
          state: trimmed ? 'trimmed' : state(e.id),
        });
        for (const h of e.highlights ?? [])
          rows.push({
            id: h.id,
            label: h.text,
            depth: depth + 1,
            highlight: true,
            state: trimmed ? 'trimmed' : state(h.id),
          });
        for (const c of e.children ?? []) walk(c, depth + 1, trimmed);
      };
      s.entries.forEach((e) => walk(e, 0));
      return { key: s.key, label: sectionLabels[s.key] ?? s.key, rows };
    });
  });
  protected readonly trimmedCount = computed(() => this.dropped().size);
  protected readonly overflow = computed(() => {
    const f = this.fitted();
    const p = this.preview();
    return !!f && !!p && f.pages > p.draft.page_budget;
  });

  constructor() {
    effect(() => {
      const id = this.jobId();
      const identity = this.auth.identity();
      untracked(() => {
        ++this.generation;
        this.fitting.set(true);
        this.preview.set(undefined);
        this.setPdf(undefined);
        this.exports.set([]);
        this.busy.set(false);
        this.error.set('');
        this.notice.set('');
        if (identity) {
          void this.load(id);
          void this.loadExports(id);
        } else this.fitting.set(false);
      });
    });
    const events = inject(EventsService);
    let timer: ReturnType<typeof setTimeout> | undefined;
    const reload = () => {
      clearTimeout(timer);
      timer = setTimeout(() => void this.load(this.jobId()), 300);
    };
    events.changes.pipe(takeUntilDestroyed()).subscribe((e) => {
      const id = this.jobId();
      if (['evidence.', 'skill.', 'organization.', 'profile.'].some((p) => e.topic.startsWith(p)))
        reload();
      else if (e.topic === 'match_run.changed' && e.data.job_id === id) reload();
      else if (
        e.topic === 'cv_draft.updated' &&
        e.data.id === id &&
        e.data.version !== this.preview()?.draft.version
      )
        reload();
      else if (e.topic === 'cv_export.created' && e.data.job_id === id) void this.loadExports(id);
    });
    events.resync.pipe(takeUntilDestroyed()).subscribe(() => {
      reload();
      void this.loadExports(this.jobId());
    });
    inject(DestroyRef).onDestroy(() => {
      this.destroyed = true;
      ++this.loadVersion;
      ++this.generation;
      clearTimeout(timer);
      this.setPdf(undefined);
    });
  }

  private async load(id: string): Promise<void> {
    if (!this.auth.identity() || this.destroyed) return;
    const scope = this.scope();
    const version = ++this.loadVersion;
    ++this.generation;
    this.setPdf(undefined);
    this.fitting.set(true);
    try {
      const p = Api.ok(await this.api.GET('/v1/jobs/{id}/cv', { params: { path: { id } } }));
      if (!this.current(scope) || version !== this.loadVersion) return;
      this.preview.set(p);
      await this.refit();
    } catch (e) {
      if (this.current(scope) && version === this.loadVersion) {
        this.error.set((e as Error).message);
        this.fitting.set(false);
      }
    }
  }

  private async loadExports(id: string): Promise<void> {
    if (!this.auth.identity() || this.destroyed) return;
    const scope = this.scope();
    const version = ++this.exportsVersion;
    try {
      const page = Api.ok(
        await this.api.GET('/v1/jobs/{id}/cv/exports', { params: { path: { id } } }),
      );
      if (this.current(scope) && version === this.exportsVersion) this.exports.set(page.items);
    } catch {
      /* The history is secondary; the preview reports errors. */
    }
  }

  /** Fits the preview to its page budget, keeping the PDF of the chosen fit. */
  private async refit(): Promise<void> {
    const p = this.preview();
    if (!p) return;
    const generation = ++this.generation;
    const scope = this.scope();
    this.setPdf(undefined);
    const content = p.content as unknown as CvContent;
    const compiled = new Map<number, Compiled>();
    this.fitting.set(true);
    this.error.set('');
    try {
      const result = await fit(p.trim.length, p.draft.page_budget, async (k) => {
        if (generation !== this.generation || !this.current(scope)) throw new Error('superseded');
        const c = await this.typst.compile(p.template, applyDrops(content, p.trim.slice(0, k)));
        compiled.set(k, c);
        return c.pages;
      });
      if (generation !== this.generation || !this.current(scope)) return;
      const pdf = compiled.get(result.k)!.pdf;
      this.setPdf({ ...result, pdf });
    } catch (e) {
      if (generation === this.generation && this.current(scope))
        this.error.set((e as Error).message);
    } finally {
      if (generation === this.generation && this.current(scope)) this.fitting.set(false);
    }
  }

  private setPdf(f: (Fit & { pdf: Uint8Array }) | undefined): void {
    if (this.url) URL.revokeObjectURL(this.url);
    this.url = undefined;
    if (!f) {
      this.fitted.set(undefined);
      return;
    }
    this.url = URL.createObjectURL(new Blob([f.pdf as BlobPart], { type: 'application/pdf' }));
    this.fitted.set({ ...f, url: this.sanitizer.bypassSecurityTrustResourceUrl(this.url) });
  }

  /** Saves draft settings; the response is the rebuilt preview. */
  protected async save(change: Partial<Schemas['CvDraftInput']>): Promise<void> {
    const p = this.preview();
    if (!p || this.busy()) return;
    const id = this.jobId();
    const scope = this.scope();
    const d = p.draft;
    ++this.loadVersion;
    ++this.generation;
    this.setPdf(undefined);
    this.busy.set(true);
    this.notice.set('');
    try {
      const body: Schemas['CvDraftInput'] = {
        template: d.template,
        template_version: d.template_version,
        locale: d.locale,
        page_budget: d.page_budget,
        picks: d.picks,
        ...change,
      };
      const next = Api.ok(
        await this.api.PUT('/v1/jobs/{id}/cv', {
          params: { path: { id }, header: { 'If-Match': `"${d.version}"` } },
          body,
        }),
      );
      if (!this.current(scope)) return;
      ++this.loadVersion;
      this.preview.set(next);
      await this.refit();
    } catch (e) {
      if (!this.current(scope)) return;
      if (e instanceof ApiError && e.status === 412) {
        this.notice.set('This CV changed in another tab. The latest version is shown.');
        await this.load(id);
      } else {
        this.error.set((e as Error).message);
        this.fitting.set(false);
      }
    } finally {
      if (this.current(scope)) this.busy.set(false);
    }
  }

  /** include: pin it in. exclude: leave it out. reset: back to automatic. */
  protected pick(id: string, how: 'include' | 'exclude' | 'reset'): void {
    const picks = this.preview()?.draft.picks;
    if (!picks) return;
    const include = picks.include.filter((x) => x !== id);
    const exclude = picks.exclude.filter((x) => x !== id);
    if (how === 'include') include.push(id);
    if (how === 'exclude') exclude.push(id);
    void this.save({ picks: { include, exclude } });
  }

  protected async export(): Promise<void> {
    const p = this.preview();
    const f = this.fitted();
    if (!p || !f || this.busy() || this.fitting()) return;
    const id = this.jobId();
    const scope = this.scope();
    this.busy.set(true);
    this.notice.set('');
    try {
      const digest = await crypto.subtle.digest('SHA-256', f.pdf as BufferSource);
      if (!this.current(scope)) return;
      const client_sha256 = Array.from(new Uint8Array(digest), (n) =>
        n.toString(16).padStart(2, '0'),
      ).join('');
      const x = Api.ok(
        await this.api.POST('/v1/jobs/{id}/cv/exports', {
          params: { path: { id }, header: { 'Idempotency-Key': crypto.randomUUID() } },
          body: {
            draft_version: p.draft.version,
            content_hash: p.content_hash,
            dropped: p.trim.slice(0, f.k),
            pages: f.pages,
            client_sha256,
          },
        }),
      );
      if (!this.current(scope)) return;
      // Download the stored snapshot, not the preview: it is what the export records.
      await this.downloadContent(p.template, x.content as unknown as CvContent);
      void this.loadExports(id);
    } catch (e) {
      if (!this.current(scope)) return;
      if (e instanceof ApiError && (e.status === 409 || e.status === 412)) {
        this.notice.set(
          'Your library or this CV changed since the preview. Review the updated CV and export again.',
        );
        await this.load(id);
      } else this.error.set((e as Error).message);
    } finally {
      if (this.current(scope)) this.busy.set(false);
    }
  }

  protected async download(x: Export): Promise<void> {
    if (this.busy()) return;
    const scope = this.scope();
    this.busy.set(true);
    try {
      const full = Api.ok(
        await this.api.GET('/v1/cv-exports/{id}', { params: { path: { id: x.id } } }),
      );
      if (!this.current(scope)) return;
      this.templates ??= this.api
        .GET('/v1/cv-templates')
        .then((r) => Api.ok(r))
        .catch((e) => {
          this.templates = undefined;
          throw e;
        });
      const template = (await this.templates).find(
        (t) => t.name === full.template && t.version === full.template_version,
      );
      if (!template)
        throw new Error('This CV used a template version that is no longer available.');
      if (!this.current(scope)) return;
      await this.downloadContent(template, full.content as unknown as CvContent);
    } catch (e) {
      if (this.current(scope)) this.error.set((e as Error).message);
    } finally {
      if (this.current(scope)) this.busy.set(false);
    }
  }

  private async downloadContent(template: CvTemplate, content: CvContent): Promise<void> {
    const scope = this.scope();
    const company = this.company();
    const { pdf } = await this.typst.compile(template, content);
    if (!this.current(scope)) return;
    const url = URL.createObjectURL(new Blob([pdf as BlobPart], { type: 'application/pdf' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = filename(content.profile.name, company);
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 10_000);
  }

  protected readonly reasons: Record<string, string> = {
    excluded: 'Removed by you',
    suggested: 'Relevant, but not auto-included',
    not_selected: 'Not selected for this job',
  };
}
