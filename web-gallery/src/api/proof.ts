import { apiRequest } from './client'

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
  version: number
  project_title: string
  gallery_title: string
}

export interface ProofSessionView {
  session: string
  expires_at: string
  round: ProofRound
}

export interface ProofItem {
  asset_id: string
  filename: string
  position: number
  width: number
  height: number
  is_video: boolean
  thumb_url: string
  display_url: string
  lqip?: string
  color?: string
  revision_id?: string
  /** 0 when the item still shows the unretouched preview. */
  revision_no: number
  comments: number
}

export interface ProofManifestPage {
  round: ProofRound
  items: ProofItem[]
  total: number
  limit: number
  next_cursor?: string
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

export interface ProofCommentInput {
  asset_id: string
  parent_id?: string | null
  body: string
  annotation?: Annotation
}

export function createProofSession(token: string): Promise<ProofSessionView> {
  return apiRequest<ProofSessionView>('/g/v1/proof/session', {
    method: 'POST',
    body: { token },
  })
}

export function fetchProofRound(session: string): Promise<{ round: ProofRound }> {
  return apiRequest<{ round: ProofRound }>('/g/v1/proof/session', { session })
}

export function fetchProofManifest(
  session: string,
  roundID: string,
  cursor: string,
  limit: number,
): Promise<ProofManifestPage> {
  const query = new URLSearchParams({ limit: String(limit) })
  if (cursor) query.set('cursor', cursor)
  return apiRequest<ProofManifestPage>(
    `/g/v1/proof/rounds/${encodeURIComponent(roundID)}/manifest?${query}`,
    { session },
  )
}

export async function fetchProofComments(
  session: string,
  roundID: string,
  opts: { assetID?: string; includeResolved?: boolean } = {},
): Promise<ProofComment[]> {
  const query = new URLSearchParams()
  if (opts.assetID) query.set('asset_id', opts.assetID)
  if (opts.includeResolved) query.set('include_resolved', 'true')
  const suffix = query.toString() ? `?${query}` : ''
  const page = await apiRequest<{ items: ProofComment[] }>(
    `/g/v1/proof/rounds/${encodeURIComponent(roundID)}/comments${suffix}`,
    { session },
  )
  return page.items
}

export function createProofComment(
  session: string,
  roundID: string,
  input: ProofCommentInput,
  idempotencyKey: string,
): Promise<ProofComment> {
  return apiRequest<ProofComment>(`/g/v1/proof/rounds/${encodeURIComponent(roundID)}/comments`, {
    method: 'POST',
    session,
    body: input,
    idempotencyKey,
  })
}

export function resolveProofComment(
  session: string,
  roundID: string,
  commentID: string,
): Promise<ProofComment> {
  return apiRequest<ProofComment>(
    `/g/v1/proof/rounds/${encodeURIComponent(roundID)}/comments/${encodeURIComponent(commentID)}/resolve`,
    { method: 'POST', session, body: {} },
  )
}

export function submitProofRound(
  session: string,
  roundID: string,
  decision: 'approved' | 'changes_requested',
  expectedVersion: number,
  idempotencyKey: string,
): Promise<ProofRound> {
  return apiRequest<ProofRound>(`/g/v1/proof/rounds/${encodeURIComponent(roundID)}/submit`, {
    method: 'POST',
    session,
    body: { decision, expected_version: expectedVersion },
    idempotencyKey,
  })
}
