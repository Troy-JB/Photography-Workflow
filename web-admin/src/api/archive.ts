import { api } from '../state/session'
import type { ListResponse } from './types'

export type ArchiveStatus = 'pending' | 'verifying' | 'ready' | 'failed' | 'deleting' | 'deleted'
export type ArchiveItemStatus = 'pending' | 'verified' | 'missing' | 'mismatch' | 'deleted'
export type ArchiveItemKind =
  | 'original'
  | 'derivative'
  | 'revision'
  | 'revision_derivative'
  | 'delivery_web'
  | 'delivery_package'
  | 'contract_document'
  | 'contract_signed'
  | 'invoice_file'

export interface ArchiveSummary {
  id: string
  tenant_id: string
  project_id: string
  project_code: string
  project_title: string
  project_state: string
  status: ArchiveStatus
  version: number
  object_count: number
  verified_count: number
  failed_count: number
  total_bytes: number
  keep_days: number
  warn_days: number
  retention_until?: string | null
  warned_at?: string | null
  verified_at?: string | null
  deletion_requested_at?: string | null
  confirmed_by?: string | null
  reason?: string
  deleted_at?: string | null
  last_error?: string
  created_at: string
  updated_at: string
}

export interface ArchiveItem {
  id: string
  archive_id: string
  asset_id?: string | null
  kind: ArchiveItemKind
  object_key: string
  expected_sha256?: string
  expected_size: number
  actual_sha256?: string
  actual_size: number
  status: ArchiveItemStatus
  note?: string
  checked_at?: string | null
  created_at: string
}

export interface ArchiveDetail extends ArchiveSummary {
  items: ArchiveItem[]
}

export interface RetentionPolicy {
  keep_days: number
  warn_days: number
  updated_by?: string | null
  updated_at?: string
}

export interface ArchiveQuery {
  project_id?: string
  status?: string
  limit?: number
  offset?: number
}

export function listArchives(params: ArchiveQuery = {}): Promise<ListResponse<ArchiveSummary>> {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === '') continue
    search.set(key, String(value))
  }
  const encoded = search.toString()
  return api<ListResponse<ArchiveSummary>>(`/api/v1/archives${encoded ? `?${encoded}` : ''}`)
}

export function createArchive(projectId: string): Promise<ArchiveSummary> {
  return api<ArchiveSummary>('/api/v1/archives', { method: 'POST', body: { project_id: projectId } })
}

export function getArchive(id: string): Promise<ArchiveDetail> {
  return api<ArchiveDetail>(`/api/v1/archives/${id}`)
}

export function listArchiveItems(
  id: string,
  params: { status?: string; limit?: number; offset?: number } = {},
): Promise<ListResponse<ArchiveItem>> {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === '') continue
    search.set(key, String(value))
  }
  const encoded = search.toString()
  return api<ListResponse<ArchiveItem>>(`/api/v1/archives/${id}/items${encoded ? `?${encoded}` : ''}`)
}

export function retryArchiveVerification(id: string): Promise<ArchiveSummary> {
  return api<ArchiveSummary>(`/api/v1/archives/${id}/verify`, { method: 'POST', body: {} })
}

export function requestArchiveDeletion(id: string, reason: string): Promise<ArchiveSummary> {
  return api<ArchiveSummary>(`/api/v1/archives/${id}/deletion`, {
    method: 'POST',
    body: { confirm: 'DELETE', reason },
  })
}

export function getRetentionPolicy(): Promise<RetentionPolicy> {
  return api<RetentionPolicy>('/api/v1/retention-policy')
}

export function updateRetentionPolicy(keepDays: number, warnDays: number): Promise<RetentionPolicy> {
  return api<RetentionPolicy>('/api/v1/retention-policy', {
    method: 'PUT',
    body: { keep_days: keepDays, warn_days: warnDays },
  })
}

export const ARCHIVE_STATUS_LABELS: Record<ArchiveStatus, string> = {
  pending: '待核验',
  verifying: '核验中',
  ready: '已归档',
  failed: '核验失败',
  deleting: '删除中',
  deleted: '已删除',
}

const ARCHIVE_STATUS_TONES: Record<ArchiveStatus, string> = {
  pending: 'draft',
  verifying: 'warn',
  ready: 'ok',
  failed: 'danger',
  deleting: 'warn',
  deleted: 'muted',
}

export function archiveStatusLabel(status: string): string {
  return ARCHIVE_STATUS_LABELS[status as ArchiveStatus] ?? status
}

export function archiveStatusTone(status: string): string {
  return ARCHIVE_STATUS_TONES[status as ArchiveStatus] ?? 'muted'
}

export const ARCHIVE_ITEM_STATUS_LABELS: Record<ArchiveItemStatus, string> = {
  pending: '待核验',
  verified: '已校验',
  missing: '缺失',
  mismatch: '哈希不符',
  deleted: '已删除',
}

const ARCHIVE_ITEM_STATUS_TONES: Record<ArchiveItemStatus, string> = {
  pending: 'draft',
  verified: 'ok',
  missing: 'danger',
  mismatch: 'danger',
  deleted: 'muted',
}

export function archiveItemStatusLabel(status: string): string {
  return ARCHIVE_ITEM_STATUS_LABELS[status as ArchiveItemStatus] ?? status
}

export function archiveItemStatusTone(status: string): string {
  return ARCHIVE_ITEM_STATUS_TONES[status as ArchiveItemStatus] ?? 'muted'
}

export const ARCHIVE_ITEM_KIND_LABELS: Record<ArchiveItemKind, string> = {
  original: '原片',
  derivative: '图库派生',
  revision: '修图源文件',
  revision_derivative: '修图派生',
  delivery_web: '交付网页版',
  delivery_package: '交付包',
  contract_document: '合同文档',
  contract_signed: '签署件',
  invoice_file: '发票文件',
}

export function archiveItemKindLabel(kind: string): string {
  return ARCHIVE_ITEM_KIND_LABELS[kind as ArchiveItemKind] ?? kind
}
