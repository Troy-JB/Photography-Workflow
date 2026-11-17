import { api } from '../state/session'
import type { ListResponse } from './types'

export type ProofStatus =
  | 'open'
  | 'in_review'
  | 'changes_requested'
  | 'retouching'
  | 'resubmitted'
  | 'approved'

export type AnnotationType = 'point' | 'rect' | 'ellipse' | 'freehand'

export interface AnnotationPoint {
  x: number
  y: number
}

export interface Annotation {
  schema: 'annotation.v1'
  type: AnnotationType
  points: AnnotationPoint[]
  color?: string
}

export interface ProofRound {
  id: string
  project_id: string
  gallery_id: string
  round_no: number
  status: ProofStatus
  due_at: string
  viewed_at?: string
  submitted_at?: string
  approved_at?: string
  change_order_id?: string
  override_by?: string
  override_reason?: string
  client_email?: string
  version: number
  created_by?: string
  created_at: string
  updated_at: string
}

export interface ProofRoundSummary extends ProofRound {
  asset_count: number
  comment_count: number
  open_comments: number
}

export interface ProofToken {
  id: string
  label: string
  max_visits: number
  visits: number
  expires_at?: string
  revoked_at?: string
  last_used_at?: string
  created_at: string
}

export interface IssuedProofToken extends ProofToken {
  token: string
  proof_url: string
}

export interface ProofRoundItem {
  asset_id: string
  filename: string
  position: number
  width: number
  height: number
  is_video: boolean
  revision_id?: string
  revision_no: number
  revision_state?: 'uploaded' | 'derivative_ready' | 'failed'
  revision_created_at?: string
  thumb_url?: string
  display_url?: string
  comments: number
  open_comments: number
}

export interface RetouchTaskSummary {
  id: string
  status: 'queued' | 'in_progress' | 'delivered' | 'cancelled'
  provider: string
  due_at?: string
  started_at?: string
  delivered_at?: string
}

export interface ProofRoundDetail extends ProofRound {
  asset_count: number
  comment_count: number
  open_comments: number
  items: ProofRoundItem[]
  tokens: ProofToken[]
  retouch?: RetouchTaskSummary
}

export interface ProofComment {
  id: string
  proof_round_id: string
  asset_id: string
  parent_id?: string
  author_type: 'client' | 'staff'
  author_user_id?: string
  client_ref?: string
  body: string
  annotation?: Annotation
  resolved_at?: string
  resolved_by?: string
  created_at: string
  updated_at: string
}

export interface ImportedRevision {
  id: string
  project_id: string
  asset_id: string
  revision_no: number
  sha256: string
  size_bytes: number
  width: number
  height: number
  status: string
  note?: string
  created_at: string
  duplicate: boolean
}

export interface CreatedProofRound {
  round: ProofRound
  token: IssuedProofToken
  items: number
}

export interface CreateProofRoundInput {
  project_id: string
  gallery_id: string
  due_at: string
  asset_ids?: string[]
  change_order_id?: string
  override_reason?: string
}

export interface IntegrationJobError {
  retouch_task_id: string
  direction: string
  attempts: number
  last_error: string
  updated_at: string
}

export interface IntegrationStatus {
  provider: string
  root?: string
  exports: Record<string, number>
  imports: Record<string, number>
  active_tasks: number
  overdue_tasks: number
  recent_errors: IntegrationJobError[]
}

export interface ProofRoundQuery {
  project_id?: string
  gallery_id?: string
  status?: string
  limit?: number
  offset?: number
}

function query(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === '') continue
    search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded ? `?${encoded}` : ''
}

export function listProofRounds(params: ProofRoundQuery = {}): Promise<ListResponse<ProofRoundSummary>> {
  return api<ListResponse<ProofRoundSummary>>(`/api/v1/proof-rounds${query({ ...params })}`)
}

export function createProofRound(input: CreateProofRoundInput): Promise<CreatedProofRound> {
  return api<CreatedProofRound>('/api/v1/proof-rounds', { method: 'POST', body: input })
}

export function getProofRound(id: string): Promise<ProofRoundDetail> {
  return api<ProofRoundDetail>(`/api/v1/proof-rounds/${id}`)
}

export function extendProofRound(id: string, dueAt: string, expectedVersion: number): Promise<ProofRound> {
  return api<ProofRound>(`/api/v1/proof-rounds/${id}`, {
    method: 'PATCH',
    body: { due_at: dueAt, expected_version: expectedVersion },
  })
}

export function issueProofToken(
  id: string,
  input: { label: string; max_visits: number; expires_at: string | null },
): Promise<IssuedProofToken> {
  return api<IssuedProofToken>(`/api/v1/proof-rounds/${id}/tokens`, { method: 'POST', body: input })
}

export function revokeProofToken(id: string, tokenId: string): Promise<void> {
  return api<void>(`/api/v1/proof-rounds/${id}/tokens/${tokenId}`, { method: 'DELETE' })
}

export function notifyProofCustomer(
  id: string,
  channel: 'email' | 'sms',
): Promise<{ token: string; proof_url: string; expires_at?: string }> {
  return api<{ token: string; proof_url: string; expires_at?: string }>(
    `/api/v1/proof-rounds/${id}/notify`,
    { method: 'POST', body: { channel } },
  )
}

export async function listProofComments(
  id: string,
  opts: { assetId?: string; includeResolved?: boolean } = {},
): Promise<ProofComment[]> {
  const page = await api<{ items: ProofComment[] }>(
    `/api/v1/proof-rounds/${id}/comments${query({
      asset_id: opts.assetId,
      include_resolved: opts.includeResolved ? 'true' : undefined,
    })}`,
  )
  return page.items
}

export function resolveProofComment(id: string, commentId: string): Promise<ProofComment> {
  return api<ProofComment>(`/api/v1/proof-rounds/${id}/comments/${commentId}/resolve`, {
    method: 'POST',
    body: {},
  })
}

export function startRetouch(id: string): Promise<RetouchTaskSummary> {
  return api<RetouchTaskSummary>(`/api/v1/proof-rounds/${id}/retouch`, { method: 'POST', body: {} })
}

export function uploadProofRevision(id: string, form: FormData): Promise<ImportedRevision> {
  return api<ImportedRevision>(`/api/v1/proof-rounds/${id}/revisions`, { method: 'POST', body: form })
}

export function resubmitProofRound(id: string): Promise<ProofRound> {
  return api<ProofRound>(`/api/v1/proof-rounds/${id}/resubmit`, { method: 'POST', body: {} })
}

export function getIntegrationStatus(): Promise<IntegrationStatus> {
  return api<IntegrationStatus>('/api/v1/integrations')
}

export function syncFolderWatch(): Promise<{ imported: number }> {
  return api<{ imported: number }>('/api/v1/integrations/folder-watch/sync', { method: 'POST', body: {} })
}
