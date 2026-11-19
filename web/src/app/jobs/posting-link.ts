/**
 * Reads what a posting link (and, if pasted, the posting text) tells us about a job.
 * Heuristics only: the review step marks every value it fills so it can be checked.
 */
export interface PostingGuess {
  title?: string;
  company?: string;
  source?: string;
  location?: string;
  work_mode?: 'onsite' | 'remote' | 'hybrid';
  employment_type?:
    'full_time' | 'part_time' | 'contract' | 'temporary' | 'internship' | 'freelance';
  salary_min?: number;
  salary_max?: number;
  salary_currency?: string;
  salary_period?: 'year' | 'month' | 'hour';
}

const SOURCES: [RegExp, string][] = [
  [/(^|\.)linkedin\.com$/, 'LinkedIn'],
  [/(^|\.)indeed\./, 'Indeed'],
  [/(^|\.)glassdoor\./, 'Glassdoor'],
  [/(^|\.)(wellfound|angel)\.co$/, 'Wellfound'],
  [/(^|\.)occ\.com\.mx$/, 'OCC Mundial'],
  [/greenhouse\.io$/, 'Company site'],
  [/lever\.co$/, 'Company site'],
  [/ashbyhq\.com$/, 'Company site'],
  [/workable\.com$/, 'Company site'],
  [/myworkdayjobs\.com$/, 'Company site'],
];

/** Hosts whose first path segment is the company: boards.greenhouse.io/acme/jobs/1 */
const ATS_COMPANY_IN_PATH =
  /(greenhouse\.io|lever\.co|ashbyhq\.com|workable\.com|recruitee\.com|smartrecruiters\.com)$/;

const words = (slug: string) =>
  slug
    .replace(/[-_+]+/g, ' ')
    .replace(/\b\d{3,}\b/g, '')
    .replace(/\s+/g, ' ')
    .trim()
    .replace(/\b\w/g, (c) => c.toUpperCase());

export function readLink(raw: string): PostingGuess {
  let url: URL;
  try {
    url = new URL(raw.trim());
  } catch {
    return {};
  }
  const host = url.hostname.replace(/^www\./, '');
  const parts = url.pathname.split('/').filter(Boolean).map(decodeURIComponent);
  const g: PostingGuess = {};
  g.source = SOURCES.find(([re]) => re.test(host))?.[1] ?? 'Company site';

  if (/linkedin\.com$/.test(host)) {
    // /jobs/view/senior-backend-engineer-at-acme-1234567890
    const slug = parts[parts.indexOf('view') + 1] ?? '';
    const m = /^(.+?)-at-(.+?)(-\d+)?$/.exec(slug);
    if (m) {
      g.title = words(m[1]);
      g.company = words(m[2]);
    }
  } else if (ATS_COMPANY_IN_PATH.test(host) && parts[0]) {
    g.company = words(parts[0]);
    const last = parts[parts.length - 1];
    if (parts.length > 2 && /[a-z]-[a-z]/i.test(last)) g.title = words(last);
  } else if (/(wellfound|angel)\.co$/.test(host) && parts[0] === 'company') {
    g.company = words(parts[1] ?? '');
    const slug = parts[3] ?? '';
    if (slug) g.title = words(slug.replace(/^\d+-/, ''));
  } else if (!SOURCES.some(([re]) => re.test(host))) {
    // careers.stripe.com/jobs/senior-product-designer
    const labels = host.split('.');
    const name = labels.length > 1 ? labels[labels.length - 2] : labels[0];
    if (!/^(jobs|careers|apply)$/.test(name)) g.company = words(name);
    const last = parts[parts.length - 1] ?? '';
    if (/[a-z]+-[a-z]+/i.test(last)) g.title = words(last);
  }
  const text = (parts.join(' ') + ' ' + url.search).toLowerCase();
  if (/\bremote\b/.test(text)) g.work_mode = 'remote';
  else if (/\bhybrid\b/.test(text)) g.work_mode = 'hybrid';
  return g;
}

const CURRENCY: Record<string, string> = { $: 'USD', '€': 'EUR', '£': 'GBP' };

export function readText(text: string): PostingGuess {
  const g: PostingGuess = {};
  if (!text.trim()) return g;
  const first =
    text
      .split('\n')
      .map((l) => l.trim())
      .find(Boolean) ?? '';
  if (first.length > 3 && first.length < 90) g.title = first;
  const t = text.toLowerCase();
  if (/\bhybrid\b/.test(t)) g.work_mode = 'hybrid';
  else if (/\bremote\b/.test(t)) g.work_mode = 'remote';
  else if (/\bon[- ]?site\b/.test(t)) g.work_mode = 'onsite';
  if (/\bpart[- ]time\b/.test(t)) g.employment_type = 'part_time';
  else if (/\bfull[- ]time\b/.test(t)) g.employment_type = 'full_time';
  else if (/\bcontract(or)?\b/.test(t)) g.employment_type = 'contract';
  else if (/\binternship\b/.test(t)) g.employment_type = 'internship';
  const loc = /\b(?:location|based in)\s*[:\-]?\s*([^\n.;]{3,60})/i.exec(text);
  if (loc) g.location = loc[1].trim();
  // "$120,000 – $150,000", "€80k-95k", "USD 120k to 150k per year"
  const money =
    /([$€£]|\b[A-Z]{3}\b)\s?(\d[\d,.]*)\s?(k)?\s*(?:-|–|—|to)\s*(?:[$€£]|[A-Z]{3})?\s?(\d[\d,.]*)\s?(k)?/.exec(
      text,
    );
  if (money) {
    const n = (v: string, k?: string) =>
      Math.round(parseFloat(v.replace(/,/g, '')) * (k ? 1000 : 1));
    g.salary_currency = CURRENCY[money[1]] ?? money[1];
    g.salary_min = n(money[2], money[3]);
    g.salary_max = n(money[4], money[5] ?? money[3]);
    g.salary_period = /per hour|\/\s?h(ou)?r|hourly/.test(t)
      ? 'hour'
      : /per month|\/\s?mo(nth)?|monthly/.test(t)
        ? 'month'
        : 'year';
  }
  return g;
}

/** The link wins for title and company; the posting text fills the rest. */
export function readPosting(url: string, text: string): PostingGuess {
  const fromText = readText(text);
  const fromLink = readLink(url);
  return { ...fromText, ...Object.fromEntries(Object.entries(fromLink).filter(([, v]) => v)) };
}

/** "LinkedIn · Senior Backend Engineer at Acme" for the hint under the link field. */
export function describeLink(url: string): string {
  const g = readLink(url);
  if (!g.source) return '';
  const what = [g.title, g.company && 'at ' + g.company].filter(Boolean).join(' ');
  return what ? g.source + ' · ' + what : g.source;
}
