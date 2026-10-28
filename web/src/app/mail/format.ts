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

/** One line telling the member what state a connected mailbox is in. */
export function accountStatus(a: Pick<MailAccount, 'status' | 'last_error'>): string {
  switch (a.status) {
    case 'active':
      return a.last_error || 'Reading new mail';
    case 'paused':
      return 'Paused. Nothing is read or sent to the model.';
    case 'needs_reconnect':
      return a.last_error || 'Gmail access ended. Reconnect to resume.';
    default:
      return 'Disconnected';
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
