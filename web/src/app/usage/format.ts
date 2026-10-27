/** Preserve the server's exact decimal amount; numbers are used only for progress. */
export function usd(value: string): string {
  const [whole, fraction = ''] = value.split('.');
  const trimmed = fraction.replace(/0+$/, '');
  return '$' + whole + (trimmed ? '.' + trimmed : '.00');
}
export function usagePercent(spent: string, reserved: string, cap: string): number {
  const limit = Number(cap);
  return limit > 0
    ? Math.min(100, Math.max(0, ((Number(spent) + Number(reserved)) / limit) * 100))
    : 0;
}
export function blockMessage(reason: string): string {
  const messages: Record<string, string> = {
    membership_required: 'An owner needs to assign you a membership before you can use models.',
    membership_suspended:
      'Your model access is suspended. Your library, jobs, and existing results remain available.',
    usage_limit:
      'Your model allowance is currently used up. Capacity returns as older usage leaves the rolling windows.',
    request_exceeds_plan: 'This request needs a larger reservation than your plan allows.',
    accounting_pending:
      'A model charge is being reconciled. Its reservation stays in place until the charge is confirmed.',
    in_flight: 'Another model request is still running or being reconciled.',
    provider_key_missing:
      'Model access is waiting for the platform owner to configure a provider key.',
    provider_metadata_unavailable:
      'Model access is temporarily unavailable. The worker will check again.',
    provider_balance:
      'Model access is waiting for the platform owner to fund the provider account.',
    encryption_unavailable: 'Model access is waiting for platform credential configuration.',
    request_rejected:
      'The provider rejected the request. The platform owner may need to update the key or model.',
    rate_limited: 'The provider is busy. The request will be checked again shortly.',
    result_unavailable: 'Accounting is complete, but the model result could not be recovered.',
  };
  return messages[reason] ?? (reason ? 'Model work is paused. The worker will check again.' : '');
}
