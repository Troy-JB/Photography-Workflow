import { api } from '../state/session'
import type { ListResponse } from './types'

export type DeliveryStatus = 'pending' | 'building' | 'ready' | 'failed' | 'blocked'
export type DeliveryPackageSpec = 'web' | 'original'
export type DeliveryWebStatus = 'pending' | 'ready' | 'skipped'

export interface Delivery {
  id: string
  tenant_id: string
  project_id: string
  proof_round_id: string
  status: DeliveryStatus
  build_version: number
  item_count: number
  total_bytes: number
  blocked_reason?: string
  last_error?: string
  ready_at?: string
  created_by?: string
  created_at: string
  updated_at: string
}

export interface DeliverySummary extends Delivery {
  project_code: string
  project_title: string
  round_no: number
  payment_status: string
  project_state: string
}

export interface DeliveryPackage {
  id: string
  spec: DeliveryPackageSpec
  version: number
  status: 'pending' | 'building' | 'ready' | 'failed'
  object_key?: string
  file_name?: string
  size_bytes: number
  sha256?: string
  entry_count: number
  last_error?: string
  built_at?: string
  created_at: string
}

export interface DeliveryItem {
  id: string
  asset_id: string
  position: number
  original_name: string
  original_mime?: string
  original_size: number
  original_sha256?: string
  revision_id?: string
  revision_no: number
  web_name?: string
  web_size: number
  web_sha256?: string
  web_width: number
  web_height: number
  web_status: DeliveryWebStatus
  web_warning_code?: string
  web_warning?: string
}

export interface DeliveryWarning {
  code: string
  message: string
  count: number
}

export interface DeliveryToken {
  id: string
  label: string
  recipient?: string
  max_downloads: number
  downloads: number
  expires_at: string
  revoked_at?: string
  last_used_at?: string
  last_used_ip?: string
  created_at: string
}

export interface IssuedDeliveryToken extends DeliveryToken {
  token: string
  url: string
}

export interface DeliveryDetail extends DeliverySummary {
  packages: DeliveryPackage[]
  tokens: DeliveryToken[]
  warnings: DeliveryWarning[]
  web_ready: number
  web_skipped: number
  web_pending: number
}

export interface DeliveryNotifyResult {
  message_id: string
  token_id: string
  expires_at: string
}

export interface DeliveryQuery {
  project_id?: string
  status?: string
  limit?: number
  offset?: number
}

export interface IssueTokenInput {
  label?: string
  recipient?: string
  expires_in_days?: number
  max_downloads?: number
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

export function listDeliveries(params: DeliveryQuery = {}): Promise<ListResponse<DeliverySummary>> {
  return api<ListResponse<DeliverySummary>>(`/api/v1/deliveries${query({ ...params })}`)
}

export function createDelivery(proofRoundId: string): Promise<DeliverySummary> {
  return api<DeliverySummary>('/api/v1/deliveries', {
    method: 'POST',
    body: { proof_round_id: proofRoundId },
  })
}

export function getDelivery(id: string): Promise<DeliveryDetail> {
  return api<DeliveryDetail>(`/api/v1/deliveries/${id}`)
}

export function buildDelivery(id: string): Promise<DeliverySummary> {
  return api<DeliverySummary>(`/api/v1/deliveries/${id}/build`, { method: 'POST', body: {} })
}

export function listDeliveryItems(
  id: string,
  params: { web_status?: string; limit?: number; offset?: number } = {},
): Promise<ListResponse<DeliveryItem>> {
  return api<ListResponse<DeliveryItem>>(`/api/v1/deliveries/${id}/items${query({ ...params })}`)
}

export function listDeliveryTokens(id: string): Promise<DeliveryToken[]> {
  return api<{ items: DeliveryToken[] }>(`/api/v1/deliveries/${id}/tokens`).then((page) => page.items)
}

export function issueDeliveryToken(id: string, input: IssueTokenInput): Promise<IssuedDeliveryToken> {
  return api<IssuedDeliveryToken>(`/api/v1/deliveries/${id}/tokens`, { method: 'POST', body: input })
}

export function revokeDeliveryToken(id: string, tokenId: string): Promise<void> {
  return api<void>(`/api/v1/deliveries/${id}/tokens/${tokenId}/revoke`, { method: 'POST', body: {} })
}

export function revokeAllDeliveryTokens(id: string): Promise<{ revoked: number }> {
  return api<{ revoked: number }>(`/api/v1/deliveries/${id}/tokens/revoke-all`, {
    method: 'POST',
    body: {},
  })
}

export function notifyDeliveryCustomer(id: string, recipient?: string): Promise<DeliveryNotifyResult> {
  return api<DeliveryNotifyResult>(`/api/v1/deliveries/${id}/notify`, {
    method: 'POST',
    body: recipient ? { recipient } : {},
  })
}

export const DELIVERY_STATUS_LABELS: Record<DeliveryStatus, string> = {
  pending: '待构建',
  building: '构建中',
  ready: '可下载',
  failed: '构建失败',
  blocked: '已阻断',
}

export const DELIVERY_STATUS_TONES: Record<DeliveryStatus, string> = {
  pending: 'draft',
  building: 'warn',
  ready: 'ok',
  failed: 'danger',
  blocked: 'danger',
}

export function deliveryStatusLabel(status: string): string {
  return DELIVERY_STATUS_LABELS[status as DeliveryStatus] ?? status
}

export function deliveryStatusTone(status: string): string {
  return DELIVERY_STATUS_TONES[status as DeliveryStatus] ?? 'muted'
}

export const PACKAGE_SPEC_LABELS: Record<DeliveryPackageSpec, string> = {
  web: '网页版（长边 2048 JPEG）',
  original: '原图',
}
