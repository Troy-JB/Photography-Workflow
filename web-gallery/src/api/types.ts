export interface Counts {
  assets: number
  selected: number
  rejected: number
  undecided: number
}

export interface Album {
  id: string
  project_id: string
  title: string
  expires_at?: string
  selection_limit: number | null
  download_allowed: boolean
  submitted_at?: string
  first_submitted_at?: string
  selection_edit_until?: string
  selection_locked_at?: string
  included_retouch_count?: number
  extra_retouch_unit_price_cents?: number
  payment_status?: string
  charge_batch_id?: string
  charge_amount_cents?: number
  payment_url?: string
  payment_order_id?: string
  selection_version: number
  counts: Counts
}

export interface SessionView {
  session: string
  expires_at: string
  album: Album
}

export type Decision = 'selected' | 'rejected' | 'none'

export interface PageItem {
  asset_id: string
  filename: string
  position: number
  /** Display dimensions (the lightbox image), not the original file. */
  width: number
  height: number
  is_video: boolean
  thumb_url: string
  display_url: string
  lqip?: string
  color?: string
  /** '' when the customer has not decided yet. */
  decision: '' | 'selected' | 'rejected'
}

export interface ManifestPage {
  items: PageItem[]
  next_cursor?: string
  selection_version: number
}

export interface SelectionBody {
  expected_version: number
  items: { asset_id: string; decision: Decision }[]
}

export interface SelectionResult {
  selection_version: number
  counts: Counts
}
