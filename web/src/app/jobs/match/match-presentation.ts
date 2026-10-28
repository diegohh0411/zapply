export function relevanceLabel(relevance: number): string {
  if (relevance >= 0.75) return 'Strongly relevant';
  if (relevance >= 0.375) return 'Relevant';
  return 'Less relevant';
}
