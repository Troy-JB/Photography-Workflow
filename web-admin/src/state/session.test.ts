import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api/client'
import { api, auth, clearAuth, refreshSession, restoreSession, signIn, signOut, signUp } from './session'
import { readStored, writeStored } from './storage'

const REFRESH_KEY = 'pwf.admin.refresh.v1'

function sessionBody(access = 'a1', refresh = 'r1', expiresInMs = 3_600_000) {
  return {
    access_token: access,
    access_expires_at: new Date(Date.now() + expiresInMs).toISOString(),
    refresh_token: refresh,
    refresh_expires_at: new Date(Date.now() + 7 * 86_400_000).toISOString(),
    user: { id: 'u1', email: 'owner@example.com', name: 'Owner' },
    tenant: { id: 't1', slug: 'studio', name: 'Studio' },
    role: 'owner',
  }
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function problem(status: number, code: string): Response {
  return json(status, { code, title: code, status })
}

beforeEach(() => {
  clearAuth()
  vi.unstubAllGlobals()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('signIn', () => {
  it('keeps the access token in memory and persists only the refresh token', async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => json(200, sessionBody()))
    vi.stubGlobal('fetch', fetchMock)

    await signIn('owner@example.com', 'secret', 'studio')

    expect(auth.status).toBe('ready')
    expect(auth.user?.email).toBe('owner@example.com')
    expect(auth.tenant?.name).toBe('Studio')
    expect(auth.role).toBe('owner')
    expect(readStored(REFRESH_KEY)).toBe('r1')
    const body = JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))
    expect(body).toMatchObject({ email: 'owner@example.com', tenant_slug: 'studio' })
  })

  it('surfaces invalid credentials as ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => problem(401, 'invalid_credentials')))
    await expect(signIn('x', 'y', '')).rejects.toMatchObject({ status: 401 })
    expect(auth.status).toBe('anonymous')
  })
})

describe('signUp', () => {
  it('creates a workspace and adopts the returned owner session', async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => json(201, sessionBody()))
    vi.stubGlobal('fetch', fetchMock)

    await signUp('Owner', 'owner@example.com', 'long-password', 'Studio')

    expect(auth.status).toBe('ready')
    expect(auth.role).toBe('owner')
    expect(readStored(REFRESH_KEY)).toBe('r1')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/auth/register',
      expect.objectContaining({ method: 'POST' }),
    )
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toEqual({
      name: 'Owner',
      email: 'owner@example.com',
      password: 'long-password',
      tenant_name: 'Studio',
    })
  })

  it('does not establish a session when registration fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => problem(409, 'email_already_registered')))

    await expect(signUp('Owner', 'owner@example.com', 'long-password', 'Studio')).rejects.toMatchObject({
      status: 409,
      code: 'email_already_registered',
    })
    expect(auth.status).toBe('anonymous')
    expect(readStored(REFRESH_KEY)).toBeNull()
  })
})

describe('api', () => {
  it('attaches the bearer token', async () => {
    const calls: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, init?: RequestInit) => {
        if (String(url).endsWith('/auth/login')) return json(200, sessionBody())
        calls.push(String((init?.headers as Record<string, string>).Authorization))
        return json(200, { items: [] })
      }),
    )
    await signIn('a', 'b', '')
    await api('/api/v1/galleries')
    expect(calls).toEqual(['Bearer a1'])
  })

  it('refreshes once after a 401 and retries with the rotated token', async () => {
    const seen: string[] = []
    let protectedCalls = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, init?: RequestInit) => {
        const path = String(url)
        if (path.endsWith('/auth/login')) return json(200, sessionBody())
        if (path.endsWith('/auth/refresh')) return json(200, sessionBody('a2', 'r2'))
        protectedCalls += 1
        const authHeader = (init?.headers as Record<string, string>).Authorization
        seen.push(authHeader)
        if (authHeader === 'Bearer a1') return problem(401, 'session_expired')
        return json(200, { ok: true })
      }),
    )
    await signIn('a', 'b', '')
    const result = await api<{ ok: boolean }>('/api/v1/things')
    expect(result.ok).toBe(true)
    expect(protectedCalls).toBe(2)
    expect(seen).toEqual(['Bearer a1', 'Bearer a2'])
    expect(readStored(REFRESH_KEY)).toBe('r2')
  })

  it('clears the session when the refresh token is rejected', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        const path = String(url)
        if (path.endsWith('/auth/login')) return json(200, sessionBody())
        if (path.endsWith('/auth/refresh')) return problem(401, 'invalid_refresh_token')
        return problem(401, 'session_expired')
      }),
    )
    await signIn('a', 'b', '')
    await expect(api('/api/v1/things')).rejects.toBeInstanceOf(ApiError)
    expect(auth.status).toBe('anonymous')
    expect(readStored(REFRESH_KEY)).toBeNull()
  })

  it('shares a single refresh request between concurrent 401 retries', async () => {
    let refreshCalls = 0
    let protectedCalls = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, init?: RequestInit) => {
        const path = String(url)
        if (path.endsWith('/auth/login')) return json(200, sessionBody())
        if (path.endsWith('/auth/refresh')) {
          refreshCalls += 1
          return json(200, sessionBody('a2', 'r2'))
        }
        protectedCalls += 1
        const authHeader = (init?.headers as Record<string, string>).Authorization
        return authHeader === 'Bearer a1' ? problem(401, 'session_expired') : json(200, { ok: true })
      }),
    )
    await signIn('a', 'b', '')
    await Promise.all([api('/api/v1/a'), api('/api/v1/b')])
    expect(protectedCalls).toBe(4)
    expect(refreshCalls).toBe(1)
  })

  it('refreshes before calling when the access token has expired', async () => {
    const paths: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        const path = String(url)
        paths.push(path)
        if (path.endsWith('/auth/login')) return json(200, sessionBody('old', 'r1', -5_000))
        if (path.endsWith('/auth/refresh')) return json(200, sessionBody('fresh', 'r2'))
        return json(200, { ok: true })
      }),
    )
    await signIn('a', 'b', '')
    await api('/api/v1/things')
    expect(paths).toEqual([
      expect.stringContaining('/auth/login'),
      expect.stringContaining('/auth/refresh'),
      expect.stringContaining('/api/v1/things'),
    ])
    expect(readStored(REFRESH_KEY)).toBe('r2')
  })
})

describe('restoreSession', () => {
  it('stays anonymous without a stored refresh token', async () => {
    expect(await restoreSession()).toBe(false)
    expect(auth.status).toBe('anonymous')
  })

  it('restores a stored session once', async () => {
    const fetchMock = vi.fn(async (url: string) => {
      expect(String(url)).toContain('/auth/refresh')
      return json(200, sessionBody('a9', 'r9'))
    })
    vi.stubGlobal('fetch', fetchMock)
    // Simulate a previous page load: only the refresh token survived.
    writeStored(REFRESH_KEY, 'r9')

    expect(await restoreSession()).toBe(true)
    expect(auth.status).toBe('ready')
    expect(fetchMock).toHaveBeenCalledTimes(1)
    // A second call reuses the in-memory session.
    expect(await restoreSession()).toBe(true)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})

it('keeps the photographer code returned by sign-in and refresh sessions', async () => {
  const body = sessionBody('access', 'refresh') as ReturnType<typeof sessionBody> & { user: { id: string; email: string; name: string; photographer_code?: string } }
  body.user.photographer_code = 'PH1234567890ABCDEF'
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(200, body)))
  await signIn('owner@example.com', 'secret', 'studio')
  expect(auth.user?.photographer_code).toBe('PH1234567890ABCDEF')
})

describe('refreshSession', () => {
  it('returns false without a stored token', async () => {
    expect(await refreshSession()).toBe(false)
  })
})

describe('signOut', () => {
  it('revokes the refresh token and clears local state', async () => {
    const fetchMock = vi.fn(async (url: string) => {
      if (String(url).endsWith('/auth/login')) return json(200, sessionBody())
      return new Response(null, { status: 204 })
    })
    vi.stubGlobal('fetch', fetchMock)
    await signIn('a', 'b', '')
    await signOut()
    expect(auth.status).toBe('anonymous')
    expect(auth.user).toBeNull()
    expect(readStored(REFRESH_KEY)).toBeNull()
    expect(fetchMock.mock.calls.at(-1)?.[0]).toEqual(expect.stringContaining('/auth/logout'))
  })
})
