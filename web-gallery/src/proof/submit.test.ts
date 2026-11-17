import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api/client'
import type { ProofRound } from '../api/proof'
import { submitWithReplay, type ProofSubmitApi } from './submit'

function round(overrides: Partial<ProofRound> = {}): ProofRound {
  return {
    id: 'round-1',
    project_id: 'project-1',
    gallery_id: 'gallery-1',
    round_no: 1,
    status: 'in_review',
    due_at: '2026-02-01T00:00:00Z',
    version: 2,
    project_title: '婚礼',
    gallery_title: '精修',
    ...overrides,
  }
}

function conflict(current: number): ApiError {
  return new ApiError(409, 'version_conflict', 'stale', { current_version: current })
}

describe('submitWithReplay', () => {
  it('returns the first successful decision without refreshing', async () => {
    const api: ProofSubmitApi = {
      submit: vi.fn(async () => round({ status: 'changes_requested', version: 3 })),
      refresh: vi.fn(async () => round()),
    }
    const result = await submitWithReplay(api, 'changes_requested', 2)
    expect(result.status).toBe('changes_requested')
    expect(api.refresh).not.toHaveBeenCalled()
  })

  it('refreshes and replays once against the newest version', async () => {
    const submit = vi
      .fn<ProofSubmitApi['submit']>()
      .mockRejectedValueOnce(conflict(4))
      .mockResolvedValueOnce(round({ status: 'approved', version: 5 }))
    const api: ProofSubmitApi = {
      submit,
      refresh: vi.fn(async () => round({ version: 4 })),
    }

    const result = await submitWithReplay(api, 'approved', 2)

    expect(result.status).toBe('approved')
    expect(submit).toHaveBeenCalledTimes(2)
    expect(submit.mock.calls[1]).toEqual(['approved', 4])
  })

  it('adopts an approval that happened elsewhere instead of replaying', async () => {
    const submit = vi.fn<ProofSubmitApi['submit']>().mockRejectedValueOnce(conflict(3))
    const api: ProofSubmitApi = {
      submit,
      refresh: vi.fn(async () => round({ status: 'approved', version: 3 })),
    }

    const result = await submitWithReplay(api, 'changes_requested', 2)

    expect(result.status).toBe('approved')
    expect(submit).toHaveBeenCalledTimes(1)
  })

  it('surfaces a second conflict instead of looping', async () => {
    const submit = vi
      .fn<ProofSubmitApi['submit']>()
      .mockRejectedValueOnce(conflict(3))
      .mockRejectedValueOnce(conflict(4))
    const api: ProofSubmitApi = {
      submit,
      refresh: vi.fn(async () => round({ version: 3 })),
    }

    await expect(submitWithReplay(api, 'approved', 2)).rejects.toMatchObject({
      code: 'version_conflict',
    })
    expect(submit).toHaveBeenCalledTimes(2)
  })

  it('does not refresh on unrelated failures', async () => {
    const api: ProofSubmitApi = {
      submit: vi.fn(async () => {
        throw new ApiError(409, 'invalid_state')
      }),
      refresh: vi.fn(async () => round()),
    }
    await expect(submitWithReplay(api, 'changes_requested', 2)).rejects.toMatchObject({
      code: 'invalid_state',
    })
    expect(api.refresh).not.toHaveBeenCalled()
  })
})
