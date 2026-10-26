import type { JobStatus, JobSummary } from './jobs.store';

type Color = JobStatus['color'];

/** Dot colors for status chips; full class names so Tailwind keeps them. */
export const STATUS_DOT: Record<Color, string> = {
  gray: 'bg-zinc-400',
  blue: 'bg-blue-500',
  yellow: 'bg-amber-400',
  orange: 'bg-orange-500',
  green: 'bg-emerald-500',
  purple: 'bg-violet-500',
  pink: 'bg-pink-500',
  red: 'bg-red-500',
  brown: 'bg-amber-800',
};

export const STATUS_COLORS = Object.keys(STATUS_DOT) as Color[];

export const WORK_MODES = [
  { value: 'onsite', label: 'On-site' },
  { value: 'remote', label: 'Remote' },
  { value: 'hybrid', label: 'Hybrid' },
] as const;

export const EMPLOYMENT_TYPES = [
  { value: 'full_time', label: 'Full-time' },
  { value: 'part_time', label: 'Part-time' },
  { value: 'contract', label: 'Contract' },
  { value: 'temporary', label: 'Temporary' },
  { value: 'internship', label: 'Internship' },
  { value: 'freelance', label: 'Freelance' },
] as const;

export const SALARY_PERIODS = [
  { value: 'year', label: 'per year' },
  { value: 'month', label: 'per month' },
  { value: 'hour', label: 'per hour' },
] as const;

export function label<T extends string>(list: readonly { value: T; label: string }[], v: T | undefined): string {
  return list.find((x) => x.value === v)?.label ?? '';
}

/** "USD 100k–120k / yr" */
export function salary(j: Pick<JobSummary, 'salary_min' | 'salary_max' | 'salary_currency' | 'salary_period'>): string {
  if (j.salary_min == null && j.salary_max == null) return '';
  const fmt = (n: number) =>
    new Intl.NumberFormat(undefined, { notation: n >= 10_000 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(n);
  const range =
    j.salary_min != null && j.salary_max != null && j.salary_min !== j.salary_max
      ? `${fmt(j.salary_min)}–${fmt(j.salary_max)}`
      : fmt((j.salary_min ?? j.salary_max)!);
  const per = { year: ' / yr', month: ' / mo', hour: ' / h' }[j.salary_period ?? 'year'] ?? '';
  return `${j.salary_currency} ${range}${j.salary_period ? per : ''}`;
}

/** "3d", "5w", "2mo" since a timestamp. */
export function ago(iso: string): string {
  const days = Math.floor((Date.now() - new Date(iso).getTime()) / 86_400_000);
  if (days < 1) return 'today';
  if (days < 14) return `${days}d`;
  if (days < 60) return `${Math.floor(days / 7)}w`;
  return `${Math.floor(days / 30)}mo`;
}

export function hostname(url: string): string {
  try {
    return new URL(url).hostname.replace(/^www\./, '');
  } catch {
    return '';
  }
}
