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
import { QueryClient } from '@tanstack/angular-query';
import { DomSanitizer, type SafeResourceUrl } from '@angular/platform-browser';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideChevronDown, lucideDownload, lucideSparkles } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardSelectImports } from '@/shared/components/select';
import { Api, ApiError, type Schemas } from '../../core/api';
import { EventsService } from '../../core/events.service';
import { AuthService } from '../../core/auth.service';
import {
  applyDrops,
  filename,
  fitContent,
  sameIds,
  sectionLabels,
  withSummarySpace,
  type CvContent,
  type CvEntry,
  type Fit,
} from './cv-content';
import { MatchTab } from '../match/match-tab';
import { RelevanceIndicator } from './relevance-indicator';
import { TypstService, type Compiled, type CvTemplate } from './typst.service';
import { RichText } from '@/shared/rich-text/rich-text.component';

/** A job's CV preview and export history, cached so the tab shows them at once on return. */
export const jobCvKey = (identity: string | null, jobId: string) =>
  ['job-cv', identity, jobId] as const;
export const jobCvExportsKey = (identity: string | null, jobId: string) =>
  ['job-cv-exports', identity, jobId] as const;

type Preview = Schemas['CvPreview'];
type Export = Schemas['CvExport'];

interface Row {
  id: string;
  label: string;
  depth: number;
  highlight: boolean;
  organization?: string;
  reason?: string;
  state: 'shown' | 'trimmed' | 'pinned' | 'hidden';
}

/** A job's CV: preview, fitting, picks and exports (part 7). */
@Component({
  selector: 'app-cv-tab',
  imports: [
    DatePipe,
    FormsModule,
    ZardButtonComponent,
    ...ZardSelectImports,
    NgIcon,
    RelevanceIndicator,
    MatchTab,
    RichText,
  ],
  viewProviders: [provideIcons({ lucideChevronDown, lucideDownload, lucideSparkles })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './cv-tab.html',
  styleUrl: './cv-tab.css',
})
export class CvTab {
  readonly jobId = input.required<string>();
  readonly company = input('');
  private readonly api = inject(Api).client;
  private readonly auth = inject(AuthService);
  private readonly client = inject(QueryClient);
  private readonly typst = inject(TypstService);
  private readonly sanitizer = inject(DomSanitizer);

  protected readonly preview = signal<Preview | undefined>(undefined);
  protected readonly fitted = signal<(Fit & { url: SafeResourceUrl; pdf: Uint8Array }) | undefined>(
    undefined,
  );
  protected readonly fitting = signal(false);
  protected readonly pdfCurrent = signal(false);
  protected readonly busy = signal(false);
  protected readonly exporting = signal(false);
  protected readonly error = signal('');
  protected readonly notice = signal('');
  protected readonly exports = signal<Export[]>([]);
  protected readonly summaryGenerating = computed(() =>
    ['pending', 'running'].includes(this.preview()?.summary_status ?? ''),
  );
  protected readonly summaryMessage = computed(() => {
    if (this.summaryGenerating()) return 'Generating summary…';
    const p = this.preview();
    switch (p?.summary_status) {
      case 'completed':
        return 'Summary updated from the selected evidence.';
      case 'failed':
        if (p.summary_reason === 'summary_character_limit')
          return 'The generated summary was too long. Your text is preserved. Try another.';
        if (p.summary_reason === 'summary_row_limit')
          return 'The generated summary didn’t fit its rows on the page. Your text is preserved. Try another.';
        if (p.summary_reason === 'summary_empty_output')
          return 'The model returned no summary text. Your text is preserved. Try another.';
        return 'We couldn’t generate a summary. Your current text is preserved. Try another.';
      case 'blocked':
        return p.summary_reason === 'usage_limit'
          ? 'Summary generation is paused because your USD allowance is exhausted.'
          : 'Summary generation is waiting. It will retry automatically when available.';
      case 'needs_review':
        return 'Summary generation needs an administrator’s review. Your current text is preserved.';
      case 'not_configured':
        return 'An administrator needs to configure a summary model before generation is available.';
      case 'waiting_for_input':
        return 'Select library evidence and add a job description to generate a summary.';
      case 'waiting_for_fit':
        return 'The summary is written once the CV fits the page. Your current text is preserved.';
      case 'cancelled':
        return 'Summary generation was cancelled. Try another when you’re ready.';
      default:
        return 'Your summary uses the evidence selected for this job.';
    }
  });
  protected regenerateSummary(): void {
    if (!this.busy() && !this.summaryGenerating()) void this.save({});
  }
  protected discardSummaryEdits(): void {
    this.summaryText = this.loadedSummaryText;
  }
  protected summaryText = '';
  protected readonly summaryCharacterLimit = 500;
  protected get summaryCharacterCount(): number {
    return Array.from(this.summaryText).length;
  }
  private loadedSummaryText = '';
  private summaryPoll?: ReturnType<typeof setTimeout>;
  protected readonly relevance = signal<Record<string, number>>({});
  private templates?: Promise<CvTemplate[]>;
  private url?: string;
  private fittedKey?: string;
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
    return new Set(p && f ? (f.dropped ?? p.trim.slice(0, f.k)) : []);
  });
  protected readonly rows = computed(() => {
    const p = this.preview();
    const content = this.content();
    if (!p || !content) return [];
    const pinned = new Set(p.draft.picks.include);
    const dropped = this.dropped();
    const state = (id: string): Row['state'] =>
      pinned.has(id) ? 'pinned' : dropped.has(id) ? 'trimmed' : 'shown';
    const sections = content.sections.map((s) => {
      const rows: Row[] = [];
      const walk = (e: CvEntry, depth: number, parentDropped = false) => {
        const trimmed = parentDropped || dropped.has(e.id);
        rows.push({
          id: e.id,
          label: e.title,
          organization: e.organization,
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
        for (const h of p.available.highlights.filter((h) => h.evidence_id === e.id))
          rows.push({
            id: h.id,
            label: h.text,
            depth: depth + 1,
            highlight: true,
            state: 'hidden',
          });
        for (const c of e.children ?? []) walk(c, depth + 1, trimmed);
      };
      s.entries.forEach((e) => walk(e, 0));
      return { key: s.key, label: sectionLabels[s.key] ?? s.key, rows };
    });
    for (const e of p.available.entries) {
      let section = sections.find((s) => s.key === e.section);
      if (!section) {
        section = { key: e.section, label: sectionLabels[e.section] ?? e.section, rows: [] };
        sections.push(section);
      }
      section.rows.push({
        id: e.id,
        label: e.title,
        depth: 0,
        highlight: false,
        reason: e.reason,
        state: 'hidden',
      });
    }
    return sections;
  });
  protected readonly selectionGroups = computed(() =>
    this.rows().map((section) => {
      const entries: { entry: Row; highlights: Row[] }[] = [];
      for (const row of section.rows) {
        if (!row.highlight) entries.push({ entry: row, highlights: [] });
        else entries.at(-1)?.highlights.push(row);
      }
      return { ...section, entries };
    }),
  );
  protected readonly selectionCount = computed(() =>
    this.rows().reduce(
      (n, s) =>
        n + s.rows.filter((r) => !r.highlight && ['shown', 'pinned'].includes(r.state)).length,
      0,
    ),
  );
  protected get hasSummaryEdits(): boolean {
    return this.summaryText !== this.loadedSummaryText;
  }

  protected toggle(row: Row, checked: boolean): void {
    this.pick(row.id, checked ? 'include' : 'exclude');
  }

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
        const cached = identity
          ? this.client.getQueryData<Preview>(jobCvKey(identity, id))
          : undefined;
        this.preview.set(cached);
        this.setPdf(undefined);
        this.exports.set(
          (identity && this.client.getQueryData<Export[]>(jobCvExportsKey(identity, id))) || [],
        );
        this.busy.set(false);
        this.exporting.set(false);
        this.error.set('');
        this.notice.set('');
        this.summaryText = this.loadedSummaryText = cached ? summaryOf(cached) : '';
        clearTimeout(this.summaryPoll);
        if (identity) {
          void this.load(id);
          void this.loadExports(id);
        } else this.fitting.set(false);
      });
    });
    effect((onCleanup) => {
      const p = this.preview();
      let active = true;
      this.relevance.set({});
      if (p?.match) {
        void this.api
          .GET('/v1/match-runs/{id}', { params: { path: { id: p.match.run_id } } })
          .then((response) => {
            const run = Api.ok(response);
            if (!active || run.result?.scoring_version !== 3 || run.result.partial) return;
            this.relevance.set(
              Object.fromEntries(
                run.result.evidence.map((item) => [item.evidence_id, item.relevance]),
              ),
            );
          })
          .catch(() => {
            /* Relevance is optional; the CV remains usable. */
          });
      }
      onCleanup(() => {
        active = false;
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
      else if (e.topic === 'model_operation.changed') reload();
      else if (e.topic === 'job.updated' && e.data.id === id) reload();
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
      clearTimeout(this.summaryPoll);
      this.setPdf(undefined);
    });
  }

  private async load(id: string): Promise<void> {
    if (!this.auth.identity() || this.destroyed) return;
    const scope = this.scope();
    const version = ++this.loadVersion;
    ++this.generation;
    this.pdfCurrent.set(false);
    this.fitting.set(true);
    try {
      this.typst.preload();
      const p = Api.ok(await this.api.GET('/v1/jobs/{id}/cv', { params: { path: { id } } }));
      if (!this.current(scope) || version !== this.loadVersion) return;
      this.preview.set(p);
      this.client.setQueryData(jobCvKey(this.auth.identity(), id), p);
      const nextSummary = summaryOf(p);
      if (this.summaryText === this.loadedSummaryText) this.summaryText = nextSummary;
      this.loadedSummaryText = nextSummary;
      clearTimeout(this.summaryPoll);
      this.scheduleSummaryPoll(p, id);
      await this.refit();
    } catch (e) {
      if (this.current(scope) && version === this.loadVersion) {
        this.error.set((e as Error).message);
        this.fitting.set(false);
      }
    }
  }

  private scheduleSummaryPoll(p: Preview, id: string): void {
    clearTimeout(this.summaryPoll);
    if (['pending', 'running', 'blocked', 'needs_review'].includes(p.summary_status ?? ''))
      this.summaryPoll = setTimeout(() => void this.load(id), 2000);
  }

  private async loadExports(id: string): Promise<void> {
    if (!this.auth.identity() || this.destroyed) return;
    const scope = this.scope();
    const version = ++this.exportsVersion;
    try {
      const page = Api.ok(
        await this.api.GET('/v1/jobs/{id}/cv/exports', { params: { path: { id } } }),
      );
      if (this.current(scope) && version === this.exportsVersion) {
        this.exports.set(page.items);
        this.client.setQueryData(jobCvExportsKey(this.auth.identity(), id), page.items);
      }
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
    this.error.set('');
    const content = p.content as unknown as CvContent;
    // Summary status polls often return identical PDF inputs. Keep the iframe
    // and its object URL intact until there is an actual replacement to show.
    const key = JSON.stringify([p.template.assets, content, p.trim, p.draft.page_budget]);
    if (this.fitted() && this.fittedKey === key) {
      this.pdfCurrent.set(true);
      this.fitting.set(false);
      const f = this.fitted();
      if (f) await this.reportFitIfNew(p, f.dropped ?? p.trim.slice(0, f.k));
      return;
    }
    this.pdfCurrent.set(false);
    const compiled = new Map<string, Compiled>();
    this.fitting.set(true);
    // Fitting reserves the summary's rows, so the fit doesn't depend on an
    // automatic summary's text. A manual summary is fitted as written.
    const reserved = p.draft.summary != null ? content : withSummarySpace(content, p.summary_rows);
    let result: Fit | undefined;
    try {
      const compile = async (source: CvContent, dropped: string[]) => {
        if (generation !== this.generation || !this.current(scope)) throw new Error('superseded');
        const fittedContent = applyDrops(source, dropped);
        const key = JSON.stringify(fittedContent);
        let c = compiled.get(key);
        if (!c) {
          c = await this.typst.compile(p.template, fittedContent);
          compiled.set(key, c);
        }
        return c;
      };
      const fitted = await fitContent(
        reserved,
        p.trim,
        p.draft.page_budget,
        async (dropped) => (await compile(reserved, dropped)).pages,
      );
      if (generation !== this.generation || !this.current(scope)) return;
      const { pdf } = await compile(content, fitted.dropped ?? []);
      if (generation !== this.generation || !this.current(scope)) return;
      this.setPdf({ ...fitted, pdf });
      this.fittedKey = key;
      this.pdfCurrent.set(true);
      result = fitted;
    } catch (e) {
      if (generation === this.generation && this.current(scope))
        this.error.set((e as Error).message);
    } finally {
      if (generation === this.generation && this.current(scope)) this.fitting.set(false);
    }
    if (result && generation === this.generation && this.current(scope))
      await this.reportFitIfNew(p, result.dropped ?? []);
  }

  /**
   * The server writes the summary only from a fit it holds for the current
   * content. Reports this one unless it already holds it.
   */
  private async reportFitIfNew(p: Preview, dropped: string[]): Promise<void> {
    if (p.fit && sameIds(p.fit.dropped, dropped)) return;
    const id = this.jobId();
    const scope = this.scope();
    try {
      Api.ok(
        await this.api.PUT('/v1/jobs/{id}/cv/fit', {
          params: { path: { id } },
          body: { fit_key: p.fit_key, dropped },
        }),
      );
    } catch (e) {
      if (!this.current(scope)) return;
      // The CV changed while it was fitted. Load the latest version and fit that.
      if (e instanceof ApiError && e.status === 409) await this.load(id);
      else this.error.set((e as Error).message);
      return;
    }
    if (this.current(scope)) await this.load(id);
  }

  private setPdf(f: (Fit & { pdf: Uint8Array }) | undefined): void {
    if (this.url) URL.revokeObjectURL(this.url);
    this.url = undefined;
    if (!f) {
      this.fittedKey = undefined;
      this.pdfCurrent.set(false);
      this.fitted.set(undefined);
      return;
    }
    this.url = URL.createObjectURL(new Blob([f.pdf as BlobPart], { type: 'application/pdf' }));
    // Asks the viewer to fit the whole page. Chrome's built-in viewer ignored this
    // fragment in testing; Safari's is unverified.
    this.fitted.set({
      ...f,
      url: this.sanitizer.bypassSecurityTrustResourceUrl(`${this.url}#zoom=page-fit`),
    });
  }

  /** Saves draft settings; the response is the rebuilt preview. */
  protected async save(change: Partial<Schemas['CvDraftInput']>): Promise<void> {
    const p = this.preview();
    if (!p || this.busy()) return;
    const id = this.jobId();
    const scope = this.scope();
    const d = p.draft;
    const version = ++this.loadVersion;
    ++this.generation;
    this.pdfCurrent.set(false);
    this.busy.set(true);
    this.notice.set('');
    try {
      const body: Schemas['CvDraftInput'] = {
        template: d.template,
        template_version: d.template_version,
        locale: d.locale,
        page_budget: d.page_budget,
        picks: d.picks,
        summary: d.summary,
        ...change,
      };
      const next = Api.ok(
        await this.api.PUT('/v1/jobs/{id}/cv', {
          params: { path: { id }, header: { 'If-Match': `"${d.version}"` } },
          body,
        }),
      );
      if (!this.current(scope)) return;
      if (
        version !== this.loadVersion ||
        (this.preview()?.draft.version ?? 0) > next.draft.version
      ) {
        this.notice.set('This CV changed while saving. The latest version is shown.');
        await this.load(id);
        return;
      }
      ++this.loadVersion;
      this.preview.set(next);
      this.client.setQueryData(jobCvKey(this.auth.identity(), id), next);
      const nextSummary =
        next.draft.summary ??
        String((next.content as unknown as CvContent).profile['summary'] ?? '');
      if (this.summaryText === this.loadedSummaryText || change.summary !== undefined)
        this.summaryText = nextSummary;
      this.loadedSummaryText = nextSummary;
      this.scheduleSummaryPoll(next, id);
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
    if (!p || !f || !this.pdfCurrent() || this.busy() || this.fitting() || this.hasSummaryEdits)
      return;
    const id = this.jobId();
    const scope = this.scope();
    this.busy.set(true);
    this.exporting.set(true);
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
            dropped: f.dropped ?? p.trim.slice(0, f.k),
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
      if (this.current(scope)) {
        this.busy.set(false);
        this.exporting.set(false);
      }
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

function summaryOf(p: Schemas['CvPreview']): string {
  return p.draft.summary ?? String((p.content as unknown as CvContent).profile['summary'] ?? '');
}
