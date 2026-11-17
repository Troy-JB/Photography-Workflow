import type { Decision } from '../selection/engine'

export type Filter = 'all' | 'selected' | 'rejected'

export function matchesFilter(decision: Decision, filter: Filter): boolean {
  return filter === 'all' || decision === filter
}
