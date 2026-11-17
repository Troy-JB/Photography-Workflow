import { api } from '../state/session'

export interface IngestAsset {
  id: string
  project_id: string
  filename: string
  content_hash: string
  size_bytes: number
  status: string
}

export interface IngestPage<T> {
  items: T[]
  total: number
}

export interface RegisteredAsset {
  rel_path: string
  asset_id: string
  duplicated: boolean
  status: string
}

export interface UploadPlan {
  mode: 'single' | 'multipart'
  asset_id: string
  url?: string
  upload_id?: string
  part_size?: number
  parts?: { part_number: number; url: string }[]
}

export interface UploadedPart {
  part_number: number
  etag: string
}

export interface BatchStatus {
  batch: { id: string; status: string }
  assets: { total: number; registered: number; uploaded: number; verified: number; missing: number; corrupt: number }
}

export function listProjectAssets(projectID: string, offset = 0): Promise<IngestPage<IngestAsset>> {
  return api(`/api/v1/projects/${encodeURIComponent(projectID)}/assets?limit=200&offset=${offset}`)
}

export function createIngestBatch(projectID: string): Promise<{ id: string }> {
  return api('/agent/v1/ingest-batches', {
    method: 'POST',
    body: { project_id: projectID, device_id: 'web-admin', card_fingerprint: 'browser-upload' },
  })
}

export function registerIngestFiles(batchID: string, files: {
  rel_path: string
  size: number
  xxh64: string
  sha256: string
  mime: string
  width: number
  height: number
  exif: Record<string, never>
  is_video: boolean
}[]): Promise<{ assets: RegisteredAsset[] }> {
  return api(`/agent/v1/ingest-batches/${batchID}/assets/batch`, { method: 'POST', body: { files } })
}

export function presignUpload(assetID: string, size: number): Promise<UploadPlan> {
  return api('/agent/v1/uploads/presign', { method: 'POST', body: { asset_id: assetID, size } })
}

export function completeUpload(assetID: string, uploadID?: string, parts?: UploadedPart[]): Promise<IngestAsset> {
  return api(`/agent/v1/uploads/${assetID}/complete`, {
    method: 'POST',
    body: uploadID ? { upload_id: uploadID, parts: parts ?? [] } : {},
  })
}

export function finalizeIngestBatch(batchID: string): Promise<unknown> {
  return api(`/agent/v1/ingest-batches/${batchID}/finalize`, { method: 'POST', body: {} })
}

export function getIngestBatch(batchID: string): Promise<BatchStatus> {
  return api(`/agent/v1/ingest-batches/${batchID}`)
}

export function enqueueProjectDerivatives(projectID: string, assetIDs: string[]): Promise<{ candidates: number }> {
  return api(`/api/v1/projects/${encodeURIComponent(projectID)}/derivatives:derive`, {
    method: 'POST',
    body: { asset_ids: assetIDs, force: false },
  })
}

export async function uploadPresignedFile(plan: UploadPlan, file: File): Promise<UploadedPart[]> {
  if (plan.mode === 'single' && plan.url) {
    const response = await fetch(plan.url, { method: 'PUT', body: file })
    if (!response.ok) throw new Error(`图片上传失败（HTTP ${response.status}）`)
    return []
  }
  if (plan.mode !== 'multipart' || !plan.upload_id || !plan.part_size || !plan.parts?.length) {
    throw new Error('服务器没有返回有效的分片上传计划')
  }
  const uploaded: UploadedPart[] = []
  for (const part of plan.parts) {
    const start = (part.part_number - 1) * plan.part_size
    const response = await fetch(part.url, {
      method: 'PUT',
      body: file.slice(start, Math.min(start + plan.part_size, file.size)),
    })
    if (!response.ok) throw new Error(`图片分片上传失败（HTTP ${response.status}）`)
    const etag = response.headers.get('ETag') ?? response.headers.get('etag')
    if (!etag) throw new Error('对象存储没有返回分片校验标识，请检查 MinIO CORS 暴露配置')
    uploaded.push({ part_number: part.part_number, etag })
  }
  return uploaded
}
