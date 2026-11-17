export interface User {
  id: string
  email: string
  name: string
  photographer_code?: string
}

export interface Tenant {
  id: string
  slug: string
  name: string
}

export interface Session {
  access_token: string
  access_expires_at: string
  refresh_token: string
  refresh_expires_at: string
  user: User
  tenant: Tenant
  role: string
}

export interface Profile {
  user: User
  tenant: Tenant
  role: string
}

export interface Project {
  id: string
  code: string
  title: string
  state: string
  shoot_start_at?: string
  shoot_end_at?: string
  selection_deadline?: string
  created_at: string
}

export type LeadStatus = 'new' | 'contacted' | 'qualified' | 'quoted' | 'won' | 'lost' | 'converted'

export interface Lead {
  id: string
  name: string
  email: string
  company: string
  phone: string
  wechat: string
  source: string
  need: string
  status: LeadStatus
  note: string
  version: number
  created_at: string
  updated_at: string
}

export type GalleryStatus = 'draft' | 'published' | 'expired' | 'revoked'
export type Decision = 'selected' | 'rejected' | 'none'

export interface Gallery {
  id: string
  project_id: string
  title: string
  status: GalleryStatus
  round_no: number
  manifest_version: number
  selection_limit: number | null
  download_allowed: boolean
  expires_at?: string | null
  published_at?: string
  watermark_text: string
  version: number
  selection_version: number
  submitted_at?: string
  revoked_at?: string
  manifest_key?: string
  created_at: string
  updated_at: string
}

export interface GallerySummary extends Gallery {
  asset_count: number
}

export interface Counts {
  assets: number
  selected: number
  rejected: number
  undecided: number
}

export interface Token {
  id: string
  label: string
  max_visits: number
  visits: number
  expires_at?: string
  revoked_at?: string
  last_used_at?: string
  created_at: string
}

export interface IssuedToken extends Token {
  token: string
  gallery_url: string
}

export interface Selection {
  asset_id: string
  filename: string
  position: number
  decision: 'selected' | 'rejected'
  decided_at: string
}

export interface GalleryDetail extends Gallery {
  counts: Counts
  tokens: Token[]
  selections: Selection[]
}

export interface GalleryAsset {
  asset_id: string
  filename: string
  position: number
  width: number
  height: number
  is_video: boolean
  decision: Decision
  preview_ready: boolean
  thumb_url?: string
  lqip?: string
  color?: string
  derive_error?: string
}

export interface DerivativeSummary {
  missing: number
  failed: number
}

export interface GalleryAssetsPage {
  items: GalleryAsset[]
  total: number
  limit: number
  offset: number
  derivatives: DerivativeSummary
}

export interface CandidateAsset {
  asset_id: string
  filename: string
  width: number
  height: number
  is_video: boolean
  status: string
  in_gallery: boolean
  preview_ready: boolean
  thumb_url?: string
  lqip?: string
  color?: string
}

export interface CandidatePage {
  items: CandidateAsset[]
  total: number
  limit: number
  offset: number
}

export interface AssetChangeResult {
  gallery: Gallery
  added: number
  skipped: number
}

export interface DeriveResult {
  candidates: number
  watermark: string
  force: boolean
}

export interface ListResponse<T> {
  items: T[]
  total: number
  limit: number
  offset: number
}
