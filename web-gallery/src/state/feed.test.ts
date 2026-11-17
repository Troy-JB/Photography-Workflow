import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { PageItem } from '../api/types'
import type { GallerySession } from './session'
import { AlbumFeed, PAGE_SIZE } from './feed'

const { fetchManifest } = vi.hoisted(() => ({ fetchManifest: vi.fn() }))

vi.mock('../api/gallery', () => ({ fetchManifest }))

const session = {
  withSession: <T>(call: (jwt: string) => Promise<T>): Promise<T> => call('jwt-1'),
} as unknown as GallerySession

function item(id: string, decision: '' | 'selected' | 'rejected' = ''): PageItem {
  return {
    asset_id: id,
    filename: `${id}.jpg`,
    position: 1,
    width: 100,
    height: 100,
    is_video: false,
    thumb_url: `${id}-thumb`,
    display_url: `${id}-display`,
    decision,
  }
}

beforeEach(() => {
  fetchManifest.mockReset()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('AlbumFeed', () => {
  it('pages sequentially and stops when the server returns no cursor', async () => {
    fetchManifest
      .mockResolvedValueOnce({ items: [item('a1'), item('a2')], next_cursor: 'c1', selection_version: 0 })
      .mockResolvedValueOnce({ items: [item('a3')], selection_version: 0 })

    const feed = new AlbumFeed(session, 'gallery-1')
    await feed.loadNext()
    await feed.loadNext()

    expect(fetchManifest.mock.calls[0]).toEqual(['jwt-1', 'gallery-1', '', PAGE_SIZE])
    expect(fetchManifest.mock.calls[1]).toEqual(['jwt-1', 'gallery-1', 'c1', PAGE_SIZE])
    expect(feed.state.items.map((entry) => entry.asset_id)).toEqual(['a1', 'a2', 'a3'])
    expect(feed.state.done).toBe(true)
    expect(feed.pageCount).toBe(2)

    await feed.loadNext()
    expect(fetchManifest).toHaveBeenCalledTimes(2)
  })

  it('skips assets already present when pages overlap', async () => {
    fetchManifest
      .mockResolvedValueOnce({ items: [item('a1'), item('a2')], next_cursor: 'c1', selection_version: 0 })
      .mockResolvedValueOnce({ items: [item('a2'), item('a3')], selection_version: 0 })

    const feed = new AlbumFeed(session, 'gallery-1')
    await feed.loadNext()
    await feed.loadNext()

    expect(feed.state.items.map((entry) => entry.asset_id)).toEqual(['a1', 'a2', 'a3'])
  })

  it('records load failures and rethrows them', async () => {
    const failure = new Error('manifest unavailable')
    fetchManifest.mockRejectedValueOnce(failure)

    const feed = new AlbumFeed(session, 'gallery-1')
    await expect(feed.loadNext()).rejects.toBe(failure)
    expect(feed.state.error).toBe('manifest unavailable')
    expect(feed.state.loading).toBe(false)

    fetchManifest.mockResolvedValueOnce({ items: [item('a1')], selection_version: 0 })
    await feed.loadNext()
    expect(feed.state.error).toBe('')
    expect(feed.state.items).toHaveLength(1)
  })

  it('re-signs only the page that contains a stale asset', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-31T10:00:00Z'))
    fetchManifest.mockResolvedValueOnce({
      items: [item('a1'), item('a2')],
      next_cursor: 'c1',
      selection_version: 0,
    })

    const feed = new AlbumFeed(session, 'gallery-1')
    await feed.loadNext()
    expect(feed.isStale('a1')).toBe(false)

    vi.setSystemTime(new Date('2026-10-31T10:09:00Z'))
    expect(feed.isStale('a1')).toBe(true)

    fetchManifest.mockResolvedValueOnce({
      items: [
        { ...item('a1'), thumb_url: 'fresh-thumb', display_url: 'fresh-display' },
        { ...item('a2'), thumb_url: 'fresh-thumb-2' },
      ],
      next_cursor: 'c1',
      selection_version: 0,
    })

    await feed.refreshPageOf('a1')
    expect(fetchManifest.mock.calls[1]).toEqual(['jwt-1', 'gallery-1', '', PAGE_SIZE])
    expect(feed.state.items[0].thumb_url).toBe('fresh-thumb')
    expect(feed.state.items[1].thumb_url).toBe('fresh-thumb-2')
    expect(feed.isStale('a1')).toBe(false)
  })

  it('ignores refreshes for unknown assets and loads after dispose', async () => {
    fetchManifest.mockResolvedValueOnce({ items: [item('a1')], selection_version: 0 })

    const feed = new AlbumFeed(session, 'gallery-1')
    await feed.loadNext()
    await feed.refreshPageOf('missing')
    expect(fetchManifest).toHaveBeenCalledTimes(1)

    feed.dispose()
    await feed.loadNext()
    expect(fetchManifest).toHaveBeenCalledTimes(1)
  })
})
