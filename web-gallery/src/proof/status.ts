import type { ProofStatus } from '../api/proof'

export interface ProofCapabilities {
  /** Comments and annotations are allowed until the round is approved. */
  canComment: boolean
  canApprove: boolean
  canRequestChanges: boolean
  /** The round is final and every action is disabled. */
  readOnly: boolean
  hint: string
}

// Retouching blocks decisions but not notes; every delivered revision can be
// reviewed again until the customer approves.
export function proofCapabilities(status: ProofStatus): ProofCapabilities {
  const canComment = status !== 'approved'
  const canApprove =
    status === 'open' ||
    status === 'in_review' ||
    status === 'changes_requested' ||
    status === 'resubmitted'
  const canRequestChanges = status === 'open' || status === 'in_review' || status === 'resubmitted'
  return {
    canComment,
    canApprove,
    canRequestChanges,
    readOnly: status === 'approved',
    hint: statusHint(status),
  }
}

function statusHint(status: ProofStatus): string {
  switch (status) {
    case 'open':
    case 'in_review':
      return '请逐张查看照片；有需要调整的地方可以添加批注，然后提出修改。'
    case 'changes_requested':
      return '修改意见已提交，等待工作室修图。这期间仍可继续补充批注。'
    case 'retouching':
      return '工作室正在修图，回传后即可继续审阅。'
    case 'resubmitted':
      return '修图已回传，请复核。可以确认通过，也可以继续提交修改意见。'
    case 'approved':
      return '本轮审片已通过，感谢您的确认。'
  }
}

export function proofStatusLabel(status: ProofStatus): string {
  switch (status) {
    case 'open':
    case 'in_review':
      return '审片中'
    case 'changes_requested':
      return '已提出修改'
    case 'retouching':
      return '修图中'
    case 'resubmitted':
      return '待复核'
    case 'approved':
      return '已通过'
  }
}

export function proofStatusTone(status: ProofStatus): 'progress' | 'waiting' | 'done' {
  switch (status) {
    case 'changes_requested':
    case 'retouching':
      return 'waiting'
    case 'approved':
      return 'done'
    default:
      return 'progress'
  }
}
