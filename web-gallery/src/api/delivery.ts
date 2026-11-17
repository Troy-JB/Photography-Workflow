import { apiRequest } from './client'

export type DeliverySpec = 'web' | 'original'

export interface CustomerDeliveryPackage {
  id: string
  spec: DeliverySpec
  file_name?: string
  size_bytes: number
  entry_count: number
}

export interface CustomerDelivery {
  id: string
  project_title: string
  round_no: number
  item_count: number
  downloads_remaining: number
  expires_at: string
  web_ready: number
  web_skipped: number
  packages: CustomerDeliveryPackage[]
}

export interface DeliverySessionView {
  session: string
  expires_at: string
  delivery: CustomerDelivery
}

export interface CustomerDeliveryItem {
  id: string
  position: number
  file_name: string
  original_size: number
  /** 0 when the file is the unretouched original. */
  revision_no: number
  web_available: boolean
  web_width?: number
  web_height?: number
  web_size?: number
  thumb_url?: string
}

export interface CustomerDeliveryItemsPage {
  items: CustomerDeliveryItem[]
  total: number
  limit: number
  offset: number
}

export interface DeliveryDownloadGrant {
  url: string
  file_name: string
  expires_at: string
  downloads_remaining: number
}

export interface DeliveryDownloadInput {
  package_id?: string
  item_id?: string
  spec?: DeliverySpec
}

export function createDeliverySession(token: string): Promise<DeliverySessionView> {
  return apiRequest<DeliverySessionView>('/g/v1/delivery/session', {
    method: 'POST',
    body: { token },
  })
}

export function fetchDeliverySummary(session: string): Promise<CustomerDelivery> {
  return apiRequest<CustomerDelivery>('/g/v1/delivery/session', { session })
}

export function fetchDeliveryItems(
  session: string,
  limit: number,
  offset: number,
): Promise<CustomerDeliveryItemsPage> {
  const query = new URLSearchParams({ limit: String(limit), offset: String(offset) })
  return apiRequest<CustomerDeliveryItemsPage>(`/g/v1/delivery/items?${query}`, { session })
}

export function createDeliveryDownload(
  session: string,
  input: DeliveryDownloadInput,
  idempotencyKey: string,
): Promise<DeliveryDownloadGrant> {
  return apiRequest<DeliveryDownloadGrant>('/g/v1/delivery/downloads', {
    method: 'POST',
    session,
    body: input,
    idempotencyKey,
  })
}
