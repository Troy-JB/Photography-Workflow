import { reactive } from 'vue'
import { fetchManifest } from '../api/gallery'
import type { PageItem } from '../api/types'
import { mapError, type GallerySession } from './session'

export const PAGE_SIZE = 60
// Presigned URLs live 10 minutes; refresh a page before customers hit broken
// images while scrolling back up.
const STALE_MS = 8 * 60_000

interface PageRecord {
  startCursor: string
  assetIDs: string[]
  fetchedAt: number
}

export interface FeedState {
  items: PageItem[]
  loading: boolean
  done: boolean
  error: string
}

// AlbumFeed loads manifest pages sequentially and keeps just enough metadata
// (page start cursor) to re-sign URLs of one page when they age out.
export class AlbumFeed {
  readonly state = reactive<FeedState>({
    items: [],
    loading: false,
    done: false,
    error: '',
  })
  private readonly pages: PageRecord[] = []
  private readonly pageOf = new Map<string, number>()
  private readonly refreshing = new Set<number>()
  private lastCursor = ''
  private disposed = false

  constructor(
    private readonly session: GallerySession,
    private readonly albumID: string,
  ) {}

  async loadNext(): Promise<void> {
    if (this.state.loading || this.state.done || this.disposed) return
    this.state.loading = true
    this.state.error = ''
    const cursor = this.lastCursor
    try {
      const page = await this.session.withSession((jwt) =>
        fetchManifest(jwt, this.albumID, cursor, PAGE_SIZE),
      )
      if (this.disposed) return
      const seen = new Set(this.state.items.map((item) => item.asset_id))
      const fresh = page.items.filter((item) => !seen.has(item.asset_id))
      this.state.items.push(...fresh)
      const record: PageRecord = {
        startCursor: cursor,
        assetIDs: page.items.map((item) => item.asset_id),
        fetchedAt: Date.now(),
      }
      const pageIndex = this.pages.push(record) - 1
      for (const id of record.assetIDs) this.pageOf.set(id, pageIndex)
      if (page.next_cursor) {
        this.lastCursor = page.next_cursor
      } else {
        this.state.done = true
      }
    } catch (error) {
      const mapped = mapError(error)
      if (!this.disposed) {
        this.state.error = mapped instanceof Error ? mapped.message : '加载失败'
      }
      throw mapped
    } finally {
      this.state.loading = false
    }
  }

  isStale(assetID: string): boolean {
    const index = this.pageOf.get(assetID)
    if (index === undefined) return false
    return Date.now() - this.pages[index].fetchedAt > STALE_MS
  }

  // refreshPageOf re-signs every URL of the page that contains the asset. Used
  // when an image fails to load because its presigned URL expired.
  async refreshPageOf(assetID: string): Promise<void> {
    const index = this.pageOf.get(assetID)
    if (index === undefined || this.refreshing.has(index)) return
    const page = this.pages[index]
    this.refreshing.add(index)
    try {
      const fresh = await this.session.withSession((jwt) =>
        fetchManifest(jwt, this.albumID, page.startCursor, PAGE_SIZE),
      )
      const byID = new Map(fresh.items.map((item) => [item.asset_id, item]))
      for (const item of this.state.items) {
        const next = byID.get(item.asset_id)
        if (!next) continue
        item.thumb_url = next.thumb_url
        item.display_url = next.display_url
        item.decision = next.decision
      }
      page.fetchedAt = Date.now()
    } catch {
      // A failed refresh surfaces through the card's error state; the next
      // scroll or retry signs it again.
    } finally {
      this.refreshing.delete(index)
    }
  }

  get pageCount(): number {
    return this.pages.length
  }

  dispose(): void {
    this.disposed = true
  }
}
