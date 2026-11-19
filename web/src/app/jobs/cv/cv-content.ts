// The content a CV template compiles (templates/README.md, schema 1), and the
// fitting that runs in the browser (docs/parts/07-rendering.md).

export interface CvDate {
  date: string;
  precision: 'year' | 'month' | 'day';
}

export interface CvHighlight {
  id: string;
  text: string;
}

export interface CvEntry {
  id: string;
  kind: string;
  form: string;
  title: string;
  organization?: string;
  location?: string;
  url?: string;
  start?: CvDate;
  end?: CvDate;
  current?: boolean;
  summary?: string;
  details?: Record<string, unknown>;
  highlights?: CvHighlight[];
  children?: CvEntry[];
}

export interface CvSection {
  key: string;
  entries: CvEntry[];
}

export interface CvSkill {
  id: string;
  name: string;
  type: string;
  proficiency?: string;
}

export interface CvContent {
  schema: number;
  locale: string;
  date: string;
  profile: { name: string; [field: string]: unknown };
  sections: CvSection[];
  skills: CvSkill[];
}

export const sectionLabels: Record<string, string> = {
  experience: 'Experience',
  education: 'Education',
  unpaid: 'Volunteering and other experience',
  credentials: 'Licenses and certifications',
  projects: 'Projects',
  publications: 'Publications',
  presentations: 'Presentations',
  patents: 'Patents',
  creative: 'Creative work',
  awards: 'Awards',
  funding: 'Grants and funding',
};

/**
 * Removes dropped entries, highlights and skills; a section left empty goes
 * too. Mirrors render.Apply on the server, which rebuilds what gets stored.
 */
export function applyDrops(content: CvContent, dropped: Iterable<string>): CvContent {
  const drop = new Set(dropped);
  const entries = (list: CvEntry[] | undefined): CvEntry[] =>
    (list ?? [])
      .filter((e) => !drop.has(e.id))
      .map((e) => {
        const out: CvEntry = { ...e };
        const highlights = (e.highlights ?? []).filter((h) => !drop.has(h.id));
        const children = entries(e.children);
        if (highlights.length) out.highlights = highlights;
        else delete out.highlights;
        if (children.length) out.children = children;
        else delete out.children;
        return out;
      });
  return {
    ...content,
    sections: content.sections
      .map((s) => ({ key: s.key, entries: entries(s.entries) }))
      .filter((s) => s.entries.length > 0),
    skills: content.skills.filter((s) => !drop.has(s.id)),
  };
}

/** Whether two lists of item IDs hold the same IDs, in any order. */
export function sameIds(a: readonly string[], b: readonly string[]): boolean {
  const x = [...a].sort();
  const y = [...b].sort();
  return x.length === y.length && x.every((id, i) => id === y[i]);
}

/**
 * Stands in for the summary while fitting, so the fit never depends on the
 * summary's text: it reserves exactly `rows` lines, the space the summary is
 * written to fill (the server revises or trims summaries that wrap to more).
 */
export function withSummarySpace(content: CvContent, rows: number): CvContent {
  return { ...content, profile: { ...content.profile, summary: Array(rows).fill('W').join('\n') } };
}

export interface Fit {
  /** How many trim-order items to drop. */
  k: number;
  pages: number;
  /** Exact optional IDs omitted after backfilling. */
  dropped?: string[];
}

/**
 * Finds the smallest k in [0, n] whose content fits the page budget, by
 * binary search: about log2(n) compiles. pages(k) compiles with the first k
 * trim-order items dropped. If nothing optional is left and it still
 * overflows, returns n: pinned items are the user's call.
 */
export async function fit(
  n: number,
  budget: number,
  pages: (k: number) => Promise<number>,
): Promise<Fit> {
  const full = await pages(0);
  if (full <= budget || n === 0) return { k: 0, pages: full };
  const least = await pages(n);
  if (least > budget) return { k: n, pages: least };
  let lo = 0;
  let hi = n;
  let hiPages = least;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    const p = await pages(mid);
    if (p <= budget) {
      hi = mid;
      hiPages = p;
    } else {
      lo = mid;
    }
  }
  return { k: hi, pages: hiPages };
}

/**
 * Fit a prefix quickly, then refill spare space in priority order. A large
 * entry that cannot fit must not prevent a smaller entry or bullet fitting.
 * IDs masked by a dropped ancestor are deferred until that ancestor returns.
 */
export async function fitContent(
  content: CvContent,
  trim: string[],
  budget: number,
  pages: (dropped: string[]) => Promise<number>,
): Promise<Fit> {
  const initial = await fit(trim.length, budget, (k) => pages(trim.slice(0, k)));
  const dropped = new Set(trim.slice(0, initial.k));
  if (initial.pages > budget || !initial.k) return { ...initial, dropped: [...dropped] };
  let count = initial.pages;
  const ancestors = new Map<string, string[]>();
  const walk = (entry: CvEntry, parents: string[]) => {
    ancestors.set(entry.id, parents);
    for (const h of entry.highlights ?? []) ancestors.set(h.id, [...parents, entry.id]);
    for (const child of entry.children ?? []) walk(child, [...parents, entry.id]);
  };
  content.sections.forEach((s) => s.entries.forEach((e) => walk(e, [])));
  const remaining = trim.slice(0, initial.k).reverse();
  // Multiple passes handle equally ranked parents that sort before children.
  let restored = true;
  while (restored) {
    restored = false;
    for (let i = 0; i < remaining.length; i++) {
      const id = remaining[i];
      if ((ancestors.get(id) ?? []).some((parent) => dropped.has(parent))) continue;
      const candidate = [...dropped].filter((x) => x !== id);
      const result = await pages(candidate);
      remaining.splice(i--, 1);
      if (result <= budget) {
        dropped.delete(id);
        count = result;
        restored = true;
      }
    }
  }
  return { k: dropped.size, pages: count, dropped: trim.filter((id) => dropped.has(id)) };
}

/** "<person> - <company>.pdf", as the server names it (render.Filename). */
export function filename(person: string, company: string): string {
  const clean = (s: string) => s.trim().replace(/[/\\:*?"<>|\u0000-\u001f]/g, '');
  const parts = [clean(person), clean(company)].filter(Boolean);
  return parts.length ? parts.join(' - ') + '.pdf' : 'CV.pdf';
}
