import { isApiError } from '../api/client'
import type { ProofRound } from '../api/proof'

export type ProofDecision = 'approved' | 'changes_requested'

export interface ProofSubmitApi {
  submit(decision: ProofDecision, expectedVersion: number): Promise<ProofRound>
  refresh(): Promise<ProofRound>
}

// submitWithReplay sends the decision and, on a version conflict, refreshes the
// round and replays exactly once against the server's newest version. If the
// refresh shows the round already approved (another device or the studio won the
// race), that state is returned instead of sending a stale decision.
export async function submitWithReplay(
  api: ProofSubmitApi,
  decision: ProofDecision,
  expectedVersion: number,
): Promise<ProofRound> {
  try {
    return await api.submit(decision, expectedVersion)
  } catch (error) {
    if (!isApiError(error, 409, 'version_conflict')) throw error
  }
  const fresh = await api.refresh()
  if (fresh.status === 'approved') return fresh
  return api.submit(decision, fresh.version)
}
