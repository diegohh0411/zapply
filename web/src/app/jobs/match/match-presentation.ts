import type { Schemas } from '../../core/api';

export type Coverage = Schemas['Coverage'];
export type MatchFilter = 'attention' | 'supported' | 'all';

export function coverageGroup(req: Coverage): 'supported' | 'review' | 'gap' | 'ignored' {
  if (req.class === 'ignore' || req.status === 'ignored') return 'ignored';
  if (req.provisional || req.review || req.status === 'unknown') return 'review';
  if (req.status === 'met') return 'supported';
  if (req.status === 'gap') return 'gap';
  return 'review';
}

export function coverageLabel(req: Coverage): string {
  switch (coverageGroup(req)) {
    case 'supported':
      return 'Supported';
    case 'gap':
      return 'No evidence found';
    case 'ignored':
      return 'Excluded';
    default:
      return req.provisional || req.review || req.status === 'unknown'
        ? 'Needs review'
        : 'Some evidence';
  }
}

export function requirementClass(value: string): string {
  return (
    (
      {
        must: 'Must have',
        preferred: 'Nice to have',
        duty: 'Responsibility',
        trait: 'Personal quality',
        ignore: 'Excluded',
      } as Record<string, string>
    )[value] ?? 'Other'
  );
}

export function primaryRequirements(requirements: Coverage[]): Coverage[] {
  const priority = { gap: 0, review: 1, supported: 2, ignored: 3 };
  return requirements
    .filter((req) => ['must', 'preferred', 'duty'].includes(req.class))
    .sort(
      (a, b) =>
        Number(b.class === 'must') - Number(a.class === 'must') ||
        priority[coverageGroup(a)] - priority[coverageGroup(b)],
    );
}
