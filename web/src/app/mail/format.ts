import type { Schemas } from '../core/api';

export type MailAccount = Schemas['MailAccount'];
export type MailCategory = Schemas['MailCategory'];

export const categories: { value: MailCategory; label: string }[] = [
  { value: 'received', label: 'Received' },
  { value: 'assessment', label: 'Assessment' },
  { value: 'interview', label: 'Interview' },
  { value: 'rejection', label: 'Rejection' },
  { value: 'offer', label: 'Offer' },
];

export function categoryLabel(c: MailCategory): string {
  return categories.find((x) => x.value === c)?.label ?? c;
}

export type Tone = 'ok' | 'busy' | 'warn' | 'muted';

type AccountState = Pick<
  MailAccount,
  'status' | 'last_error' | 'retry_at' | 'mails_done' | 'mails_total' | 'mails_relevant'
>;

/** How far a mailbox's mail is classified. `working` is true while some is still to do. */
export function mailProgress(a: Pick<MailAccount, 'status' | 'mails_done' | 'mails_total'>): {
  done: number;
  total: number;
  percent: number;
  working: boolean;
} {
  const done = Math.min(a.mails_done, a.mails_total);
  const total = a.mails_total;
  return {
    done,
    total,
    percent: total > 0 ? Math.round((done / total) * 100) : 0,
    working: total > done,
  };
}

/** The label and colour of a mailbox's status pill. */
export function accountBadge(a: AccountState): { label: string; tone: Tone } {
  switch (a.status) {
    case 'active':
      if (a.retry_at) return { label: 'Waiting for Gmail', tone: 'warn' };
      if (a.last_error) return { label: 'Sync problem', tone: 'warn' };
      if (mailProgress(a).working) return { label: 'Reading mail', tone: 'busy' };
      return { label: 'Active', tone: 'ok' };
    case 'paused':
      return { label: 'Paused', tone: 'muted' };
    case 'needs_reconnect':
      return { label: 'Needs reconnect', tone: 'warn' };
    default:
      return { label: 'Disconnected', tone: 'muted' };
  }
}

/** Short relative time, in the past ("5 min ago") or the future ("in 2 min"). */
export function relativeTime(iso: string, now = Date.now()): string {
  const seconds = Math.round((new Date(iso).getTime() - now) / 1000);
  const span = Math.abs(seconds);
  const unit =
    span < 45
      ? null
      : span < 3600
        ? `${Math.round(span / 60)} min`
        : span < 86400
          ? `${Math.round(span / 3600)} h`
          : `${Math.round(span / 86400)} d`;
  if (!unit) return seconds > 0 ? 'in a moment' : 'just now';
  return seconds > 0 ? `in ${unit}` : `${unit} ago`;
}

/** What the member should know about a mailbox beyond its pill, or '' when nothing. */
export function accountNote(a: AccountState, now = Date.now()): string {
  switch (a.status) {
    case 'paused':
      return 'Nothing is read or sent to the model until you resume.';
    case 'needs_reconnect':
      return a.last_error || 'Gmail access ended. Reconnect to resume.';
    case 'active':
      if (a.retry_at)
        return `Gmail is limiting requests. zapply tries again ${relativeTime(a.retry_at, now)}.`;
      return a.last_error;
    default:
      return '';
  }
}

/** The banner for the page's `?connected=1` or `?error=…` query. */
export function connectNotice(params: Record<string, string | undefined>): {
  kind: 'ok' | 'error';
  text: string;
} | null {
  if (params['connected']) return { kind: 'ok', text: 'Gmail connected. Reading your inbox now.' };
  switch (params['error']) {
    case undefined:
    case '':
      return null;
    case 'denied':
      return { kind: 'error', text: 'Gmail access was not granted, so nothing was connected.' };
    case 'expired':
      return { kind: 'error', text: 'That sign-in link expired. Start again.' };
    case 'in_use':
      return {
        kind: 'error',
        text: 'That Gmail address is already connected to another zapply account.',
      };
    case 'unavailable':
      return { kind: 'error', text: 'Mail is not set up on this server.' };
    default:
      return { kind: 'error', text: 'Could not connect Gmail. Try again.' };
  }
}

/** Splits `Name <addr@x>` into its parts; a bare address comes back as the name. */
export function parseSender(from: string): { name: string; email: string } {
  const m = /^\s*"?([^"<]*?)"?\s*<([^>]+)>\s*$/.exec(from);
  if (!m || !m[1]) return { name: m?.[2] ?? from, email: '' };
  return { name: m[1], email: m[2] };
}

/** "Today", "Yesterday", or a date like "Wed, Nov 5" (with the year when it is not this one). */
export function dayLabel(iso: string, now = Date.now()): string {
  const d = new Date(iso);
  const start = (t: Date) => new Date(t.getFullYear(), t.getMonth(), t.getDate()).getTime();
  const today = new Date(now);
  const days = Math.round((start(today) - start(d)) / 86_400_000);
  if (days === 0) return 'Today';
  if (days === 1) return 'Yesterday';
  return d.toLocaleDateString(undefined, {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    ...(d.getFullYear() !== today.getFullYear() && { year: 'numeric' }),
  });
}
