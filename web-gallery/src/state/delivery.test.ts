import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api/client'
import {
  fetchDeliveryItems,
  type CustomerDelivery,
  type DeliveryDownloadGrant,
} from '../api/delivery'
import {
  applyGrant,
  canDownload,
  DeliverySession,
  DeliveryTerminalError,
  mapDeliveryError,
} from './delivery'

const STORAGE_KEY = 'pwf.delivery.session.v1'

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

function delivery(overrides: Partial<CustomerDelivery> = {}): CustomerDelivery {
  return {
    id: 'delivery-1',
    project_title: '婚礼跟拍',
    round_no: 2,
    item_count: 120,
    downloads_remaining: 20,
    expires_at: '2026-11-08T10:30:00Z',
    web_ready: 118,
    web_skipped: 2,
    packages: [],
    ...overrides,
  }
}

function sessionView(jwt: string): unknown {
  return {
    session: jwt,
    expires_at: new Date(Date.now() + 30 * 60_000).toISOString(),
    delivery: delivery(),
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

describe('DeliverySession', () => {
  it('exchanges the link once and reuses the stored session', async () => {
    const storage = new MemoryStorage()
    let exchanges = 0
    let refreshes = 0
    installFetch((call) => {
      if (call.url === '/g/v1/delivery/session' && call.method === 'POST') {
        exchanges++
        return json(sessionView(`jwt-${exchanges}`), 201)
      }
      if (call.url === '/g/v1/delivery/session') {
        refreshes++
        return json(delivery())
      }
      throw new Error(`unexpected ${call.method} ${call.url}`)
    })

    const session = new DeliverySession(storage)
    expect((await session.open('delivery-link')).id).toBe('delivery-1')
    expect((await session.open('delivery-link')).id).toBe('delivery-1')

    expect(exchanges).toBe(1)
    expect(refreshes).toBe(1)
    expect(storage.getItem(STORAGE_KEY)).toContain('jwt-1')
  })

  it('renews once when the server rejects the session as expired', async () => {
    let exchanges = 0
    const authorizations: (string | undefined)[] = []
    installFetch((call) => {
      if (call.method === 'POST') {
        exchanges++
        return json(sessionView(`jwt-${exchanges}`), 201)
      }
      authorizations.push(call.authorization)
      if (authorizations.length === 1) return problem(401, 'session_expired')
      return json(delivery())
    })

    const session = new DeliverySession(new MemoryStorage())
    await session.open('delivery-link')
    const items = await session.withSession((jwt) => fetchDeliveryItems(jwt, 40, 0))

    expect(items).toBeDefined()
    expect(exchanges).toBe(2)
    expect(authorizations).toEqual(['Bearer jwt-1', 'Bearer jwt-2'])
  })

  it('refreshes the summary through the session', async () => {
    installFetch((call) => {
      if (call.method === 'POST') return json(sessionView('jwt-1'), 201)
      return json(delivery({ downloads_remaining: 7 }))
    })

    const session = new DeliverySession(new MemoryStorage())
    await session.open('delivery-link')
    expect((await session.refreshSummary()).downloads_remaining).toBe(7)
  })

  it('resumes the last delivery when the token is gone from the URL', async () => {
    const storage = new MemoryStorage()
    installFetch((call) => {
      if (call.method === 'POST') return json(sessionView('jwt-1'), 201)
      return json(delivery())
    })

    const first = new DeliverySession(storage)
    await first.open('delivery-link')

    const resumed = new DeliverySession(storage)
    expect((await resumed.resume()).id).toBe('delivery-1')
  })

  it('rejects resume when this tab never opened a delivery', async () => {
    installFetch(() => json(delivery()))
    await expect(new DeliverySession(new MemoryStorage()).resume()).rejects.toMatchObject({
      code: 'invalid_link',
    })
  })

  it('rejects calls before a session exists', async () => {
    installFetch(() => json(delivery()))
    await expect(
      new DeliverySession(new MemoryStorage()).refreshSummary(),
    ).rejects.toMatchObject({ code: 'invalid_link' })
  })
})

describe('mapDeliveryError', () => {
  const terminal: [number, string, string][] = [
    [401, 'invalid_link', 'invalid_link'],
    [410, 'link_expired', 'link_expired'],
    [410, 'link_revoked', 'link_revoked'],
    [409, 'payment_required', 'payment_required'],
    [409, 'delivery_not_ready', 'delivery_not_ready'],
    [409, 'round_not_approved', 'round_not_approved'],
    [409, 'downloads_exhausted', 'downloads_exhausted'],
    [410, 'delivery_archived', 'delivery_archived'],
    [410, 'delivery_cancelled', 'delivery_cancelled'],
  ]

  it.each(terminal)('maps %i %s to %s', (status, code, expected) => {
    const mapped = mapDeliveryError(new ApiError(status, code))
    expect(mapped).toBeInstanceOf(DeliveryTerminalError)
    expect((mapped as DeliveryTerminalError).code).toBe(expected)
  })

  it('keeps per-item problems as retryable errors', () => {
    const original = new ApiError(409, 'web_spec_unavailable')
    expect(mapDeliveryError(original)).toBe(original)
  })

  it('passes terminal errors through unchanged', () => {
    const original = new DeliveryTerminalError('link_expired')
    expect(mapDeliveryError(original)).toBe(original)
  })
})

describe('download quota', () => {
  const grant: DeliveryDownloadGrant = {
    url: 'https://files.example/zip?sig=1',
    file_name: 'PWF-001_web_20261101.zip',
    expires_at: '2026-11-01T10:35:00Z',
    downloads_remaining: 3,
  }

  it('folds the grant into the cached delivery view', () => {
    const updated = applyGrant(delivery({ downloads_remaining: 4 }), grant)
    expect(updated.downloads_remaining).toBe(3)
    expect(updated.packages).toEqual([])
  })

  it('never reports a negative remainder', () => {
    const updated = applyGrant(delivery({ downloads_remaining: 1 }), { ...grant, downloads_remaining: -1 })
    expect(updated.downloads_remaining).toBe(0)
    expect(canDownload(updated)).toBe(false)
  })

  it('disables downloads when no session or quota is left', () => {
    expect(canDownload(null)).toBe(false)
    expect(canDownload(delivery({ downloads_remaining: 0 }))).toBe(false)
    expect(canDownload(delivery({ downloads_remaining: 2 }))).toBe(true)
  })
})
