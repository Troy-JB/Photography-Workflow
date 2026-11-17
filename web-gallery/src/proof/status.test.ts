import { describe, expect, it } from 'vitest'
import type { ProofStatus } from '../api/proof'
import { proofCapabilities, proofStatusLabel, proofStatusTone } from './status'

describe('proofCapabilities', () => {
  it('allows comments and both decisions while the round is in review', () => {
    for (const status of ['open', 'in_review'] as ProofStatus[]) {
      const caps = proofCapabilities(status)
      expect(caps).toMatchObject({
        canComment: true,
        canApprove: true,
        canRequestChanges: true,
        readOnly: false,
      })
      expect(caps.hint.length).toBeGreaterThan(0)
    }
  })

  it('keeps notes open but decisions closed during retouching', () => {
    expect(proofCapabilities('retouching')).toMatchObject({
      canComment: true,
      canApprove: false,
      canRequestChanges: false,
      readOnly: false,
    })
  })

  it('allows either decision after every resubmit', () => {
    expect(proofCapabilities('resubmitted')).toMatchObject({
      canComment: true,
      canApprove: true,
      canRequestChanges: true,
    })
    expect(proofCapabilities('resubmitted').hint).toContain('继续提交修改')
  })

  it('still allows approval once changes were requested', () => {
    expect(proofCapabilities('changes_requested')).toMatchObject({
      canApprove: true,
      canRequestChanges: false,
    })
  })

  it('locks an approved round', () => {
    expect(proofCapabilities('approved')).toMatchObject({
      canComment: false,
      canApprove: false,
      canRequestChanges: false,
      readOnly: true,
    })
  })
})

describe('status copy', () => {
  it('labels every status', () => {
    const statuses: ProofStatus[] = [
      'open',
      'in_review',
      'changes_requested',
      'retouching',
      'resubmitted',
      'approved',
    ]
    for (const status of statuses) {
      expect(proofStatusLabel(status)).not.toBe('')
    }
    expect(proofStatusLabel('in_review')).toBe('审片中')
    expect(proofStatusLabel('resubmitted')).toBe('待复核')
  })

  it('groups tones for chips', () => {
    expect(proofStatusTone('in_review')).toBe('progress')
    expect(proofStatusTone('retouching')).toBe('waiting')
    expect(proofStatusTone('approved')).toBe('done')
  })
})
