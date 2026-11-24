/**
 * Preserve the server's exact decimal amount; numbers are used only for progress.
 * Trailing zeros go, but cents always show two digits: $0.50, not $0.5.
 */
export function usd(value: string): string {
  const [whole, fraction = ''] = value.split('.');
  const trimmed = fraction.replace(/0+$/, '');
  return '$' + whole + '.' + (trimmed ? trimmed.padEnd(2, '0') : '00');
}

/** Show usage amounts at cent precision without making sub-cent charges look free. */
export function usageUsd(value: string): string {
  const amount = Number(value);
  if (!Number.isFinite(amount)) return usd(value);
  if (amount !== 0 && Math.abs(amount) < 0.01) {
    return amount < 0 ? '-$0.01' : 'under $0.01';
  }
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(amount);
}
export function usagePercent(spent: string, reserved: string, cap: string): number {
  const limit = Number(cap);
  return limit > 0
    ? Math.min(100, Math.max(0, ((Number(spent) + Number(reserved)) / limit) * 100))
    : 0;
}
const failureMessages: Record<string, string> = {
  encryption_unavailable:
    'The stored provider key could not be decrypted. Check the encryption key ring on the worker.',
  request_rejected:
    'The provider rejected the request. Check the provider key and model configuration.',
  rate_limited: 'The provider stayed busy through every retry.',
  provider_balance: 'The provider account has insufficient funds.',
  provider_budget: 'The provider key reached its spending limit.',
  stale_quote: 'Model pricing changed during every retry.',
};
/** Failed operations get final wording; other reasons describe a pause. */
export function blockMessage(reason: string, status = ''): string {
  if (status === 'failed' && reason !== 'result_unavailable') {
    return (
      (failureMessages[reason] ?? 'The request failed without a charge.') +
      ' It will not be retried.'
    );
  }
  const messages: Record<string, string> = {
    membership_required: 'An owner needs to assign you a membership before you can use models.',
    membership_suspended:
      'Your model access is suspended. Your library, jobs, and existing results remain available.',
    usage_limit:
      'You have used your allowance for now. It grows back gradually as older usage falls outside the last 7 and 30 days.',
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
