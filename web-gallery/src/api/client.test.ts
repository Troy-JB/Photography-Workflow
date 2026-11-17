import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiRequest, isApiError } from './client'

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('apiRequest', () => {
  it('sends the session, idempotency key and no-store options', async () => {
    const fetchMock = vi.fn(async () => json({ selection_version: 3 }))
    vi.stubGlobal('fetch', fetchMock)

    const result = await apiRequest<{ selection_version: number }>('/g/v1/selections', {
      method: 'POST',
      body: { expected_version: 1, items: [] },
      session: 'jwt-1',
      idempotencyKey: 'key-1',
    })

    expect(result.selection_version).toBe(3)
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    const headers = init.headers as Record<string, string>
    expect(url).toBe('/g/v1/selections')
    expect(init.method).toBe('POST')
    expect(headers.Authorization).toBe('Bearer jwt-1')
    expect(headers['Idempotency-Key']).toBe('key-1')
    expect(headers['Content-Type']).toBe('application/json')
    expect(init.cache).toBe('no-store')
    expect(init.credentials).toBe('omit')
    expect(init.referrerPolicy).toBe('no-referrer')
    expect(init.body).toBe('{"expected_version":1,"items":[]}')
  })

  it('maps application/problem+json into ApiError', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        json({ code: 'version_conflict', detail: 'changed', current_version: 4 }, 409),
      ),
    )

    const error = await apiRequest('/g/v1/selections', { method: 'POST', body: {} }).catch(
      (reason: unknown) => reason,
    )

    expect(error).toBeInstanceOf(ApiError)
    expect(isApiError(error, 409, 'version_conflict')).toBe(true)
    expect((error as ApiError).problem.current_version).toBe(4)
    expect((error as ApiError).detail).toBe('changed')
  })

  it('falls back to the HTTP status when the body is not a problem', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('oops', { status: 502, statusText: 'Bad Gateway' })),
    )

    const error = await apiRequest('/g/v1/session').catch((reason: unknown) => reason)

    expect(isApiError(error, 502)).toBe(true)
    expect((error as ApiError).code).toBe('http_502')
    expect((error as ApiError).detail).toBe('Bad Gateway')
  })

  it('does not set Authorization without a session', async () => {
    const fetchMock = vi.fn(async () => json({ ok: true }))
    vi.stubGlobal('fetch', fetchMock)

    await apiRequest('/g/v1/albums/x/manifest')

    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    const headers = init.headers as Record<string, string>
    expect(headers.Authorization).toBeUndefined()
    expect(init.method).toBe('GET')
    expect(init.body).toBeUndefined()
  })
})
