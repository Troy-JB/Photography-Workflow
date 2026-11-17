import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ProofRound } from '../api/proof'
import { fetchProofRound } from '../api/proof'
import { ProofSession, ProofTerminalError } from './proof'

const STORAGE_KEY = 'pwf.proof.session.v1'

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

function round(overrides: Partial<ProofRound> = {}): ProofRound {
  return {
    id: 'round-1',
    project_id: 'project-1',
    gallery_id: 'gallery-1',
    round_no: 2,
    status: 'in_review',
    due_at: '2026-02-01T00:00:00Z',
    version: 3,
    project_title: '婚礼跟拍',
    gallery_title: '精修候选',
    ...overrides,
  }
}

function sessionView(jwt: string): unknown {
  return {
    session: jwt,
    expires_at: new Date(Date.now() + 4 * 3600_000).toISOString(),
    round: round(),
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

describe('ProofSession', () => {
  it('exchanges the link once and reuses the stored session', async () => {
    const storage = new MemoryStorage()
    let exchanges = 0
    let refreshes = 0
    installFetch((call) => {
      if (call.url === '/g/v1/proof/session' && call.method === 'POST') {
        exchanges++
        return json(sessionView(`jwt-${exchanges}`), 201)
      }
      if (call.url === '/g/v1/proof/session') {
        refreshes++
        return json({ round: round() })
      }
      throw new Error(`unexpected ${call.method} ${call.url}`)
    })

    const session = new ProofSession(storage)
    expect((await session.open('proof-link')).id).toBe('round-1')
    expect((await session.open('proof-link')).id).toBe('round-1')

    expect(exchanges).toBe(1)
    expect(refreshes).toBe(1)
    expect(storage.getItem(STORAGE_KEY)).toContain('jwt-1')
  })

  it('exchanges again when the stored session is about to expire', async () => {
    const storage = new MemoryStorage()
    storage.setItem(
      STORAGE_KEY,
      JSON.stringify({ share: 'proof-link', jwt: 'stale', expiresAt: Date.now() + 30_000 }),
    )
    let exchanges = 0
    installFetch((call) => {
      if (call.method === 'POST') {
        exchanges++
        return json(sessionView(`jwt-${exchanges}`), 201)
      }
      return json({ round: round() })
    })

    const session = new ProofSession(storage)
    await session.open('proof-link')
    expect(exchanges).toBe(1)
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
      return json({ round: round() })
    })

    const session = new ProofSession(new MemoryStorage())
    await session.open('proof-link')
    const fresh = await session.withSession((jwt) => fetchProofRound(jwt))

    expect(fresh.round.id).toBe('round-1')
    expect(exchanges).toBe(2)
    expect(authorizations).toEqual(['Bearer jwt-1', 'Bearer jwt-2'])
  })

  it('maps link failures to terminal codes', async () => {
    const cases: [number, string, string][] = [
      [401, 'invalid_token', 'invalid_token'],
      [410, 'expired', 'expired'],
      [410, 'revoked', 'revoked'],
      [429, 'too_many_visits', 'too_many_visits'],
      [410, 'gallery_revoked', 'gallery_revoked'],
      [410, 'round_closed', 'round_closed'],
      [404, 'round_not_found', 'round_closed'],
    ]
    for (const [status, code, expected] of cases) {
      installFetch(() => problem(status, code))
      const error = await new ProofSession(new MemoryStorage())
        .open('proof-link')
        .catch((reason: unknown) => reason)
      expect(error).toBeInstanceOf(ProofTerminalError)
      expect((error as ProofTerminalError).code).toBe(expected)
    }
  })

  it('treats a token revoked after opening as terminal', async () => {
    installFetch((call) => {
      if (call.method === 'POST') return json(sessionView('jwt-1'), 201)
      return problem(401, 'session_revoked')
    })

    const session = new ProofSession(new MemoryStorage())
    await session.open('proof-link')

    const error = await session.refreshRound().catch((reason: unknown) => reason)
    expect(error).toBeInstanceOf(ProofTerminalError)
    expect((error as ProofTerminalError).code).toBe('revoked')
  })

  it('rejects calls before a session exists', async () => {
    installFetch(() => json({ round: round() }))
    await expect(new ProofSession(new MemoryStorage()).refreshRound()).rejects.toMatchObject({
      code: 'invalid_token',
    })
  })
})
