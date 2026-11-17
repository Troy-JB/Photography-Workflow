import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchManifest } from '../api/gallery'
import type { Album } from '../api/types'
import { GallerySession, TerminalError } from './session'

const STORAGE_KEY = 'pwf.gallery.session.v1'

class MemoryStorage implements Storage {
  private readonly data = new Map<string, string>()

  get length(): number {
    return this.data.size
  }

  clear(): void {
    this.data.clear()
  }

  getItem(key: string): string | null {
    return this.data.get(key) ?? null
  }

  key(index: number): string | null {
    return [...this.data.keys()][index] ?? null
  }

  removeItem(key: string): void {
    this.data.delete(key)
  }

  setItem(key: string, value: string): void {
    this.data.set(key, value)
  }
}

function album(overrides: Partial<Album> = {}): Album {
  return {
    id: 'gallery-1',
    project_id: 'project-1',
    title: '选片',
    selection_limit: 30,
    download_allowed: false,
    selection_version: 0,
    counts: { assets: 4, selected: 0, rejected: 0, undecided: 4 },
    ...overrides,
  }
}

function sessionView(jwt: string): unknown {
  return {
    session: jwt,
    expires_at: new Date(Date.now() + 2 * 3600_000).toISOString(),
    album: album(),
  }
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function problem(status: number, code: string): Response {
  return new Response(JSON.stringify({ status, code, title: code }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

interface Call {
  url: string
  method: string
  authorization: string | undefined
}

function installFetch(
  handler: (call: Call) => Response | Promise<Response>,
): ReturnType<typeof vi.fn> {
  const mock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const headers = (init?.headers ?? {}) as Record<string, string>
    return handler({
      url: String(input),
      method: init?.method ?? 'GET',
      authorization: headers.Authorization,
    })
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('GallerySession', () => {
  it('exchanges the share token once and reuses the stored session', async () => {
    const storage = new MemoryStorage()
    let exchanges = 0
    const mock = installFetch((call) => {
      if (call.url === '/g/v1/session' && call.method === 'POST') {
        exchanges++
        return json(sessionView(`jwt-${exchanges}`))
      }
      if (call.url === '/g/v1/session') return json(album())
      throw new Error(`unexpected ${call.method} ${call.url}`)
    })

    const session = new GallerySession(storage)
    expect((await session.open('share-1')).id).toBe('gallery-1')
    expect((await session.open('share-1')).id).toBe('gallery-1')

    expect(exchanges).toBe(1)
    expect(mock).toHaveBeenCalledTimes(2)
    expect(storage.getItem(STORAGE_KEY)).toContain('jwt-1')
    const refresh = mock.mock.calls[1] as unknown as [string, RequestInit]
    expect((refresh[1].headers as Record<string, string>).Authorization).toBe('Bearer jwt-1')
  })

  it('exchanges again when the stored session is about to expire', async () => {
    const storage = new MemoryStorage()
    storage.setItem(
      STORAGE_KEY,
      JSON.stringify({ share: 'share-1', jwt: 'stale', expiresAt: Date.now() + 30_000 }),
    )
    let exchanges = 0
    installFetch((call) => {
      if (call.method === 'POST') {
        exchanges++
        return json(sessionView(`jwt-${exchanges}`))
      }
      return json(album())
    })

    const session = new GallerySession(storage)
    await session.open('share-1')
    expect(exchanges).toBe(1)
  })

  it('renews once when the server rejects the session as expired', async () => {
    let exchanges = 0
    const authorizations: (string | undefined)[] = []
    installFetch((call) => {
      if (call.method === 'POST') {
        exchanges++
        return json(sessionView(`jwt-${exchanges}`))
      }
      if (call.url.includes('/manifest')) {
        authorizations.push(call.authorization)
        if (authorizations.length === 1) return problem(401, 'session_expired')
        return json({ items: [], selection_version: 0 })
      }
      return json(album())
    })

    const session = new GallerySession(new MemoryStorage())
    await session.open('share-1')
    const page = await session.withSession((jwt) =>
      fetchManifest(jwt, 'gallery-1', '', 10),
    )

    expect(page.items).toEqual([])
    expect(exchanges).toBe(2)
    expect(authorizations).toEqual(['Bearer jwt-1', 'Bearer jwt-2'])
  })

  it('maps revoked and expired links to terminal errors', async () => {
    installFetch(() => problem(410, 'revoked'))
    await expect(new GallerySession(new MemoryStorage()).open('share-1')).rejects.toMatchObject({
      code: 'revoked',
    })

    installFetch(() => problem(429, 'too_many_visits'))
    const error = await new GallerySession(new MemoryStorage())
      .open('share-1')
      .catch((reason: unknown) => reason)
    expect(error).toBeInstanceOf(TerminalError)
    expect((error as TerminalError).code).toBe('too_many_visits')
  })

  it('treats a session revoked after opening as terminal', async () => {
    installFetch((call) => {
      if (call.method === 'POST') return json(sessionView('jwt-1'))
      return problem(401, 'session_revoked')
    })

    const session = new GallerySession(new MemoryStorage())
    await session.open('share-1')

    const error = await session.refreshAlbum().catch((reason: unknown) => reason)
    expect(error).toBeInstanceOf(TerminalError)
    expect((error as TerminalError).code).toBe('revoked')
  })

  it('rejects calls before a session exists', async () => {
    installFetch(() => json(album()))
    const session = new GallerySession(new MemoryStorage())
    await expect(session.refreshAlbum()).rejects.toMatchObject({ code: 'invalid_token' })
  })
})
