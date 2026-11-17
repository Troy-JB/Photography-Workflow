import { reactive } from 'vue'
import { apiRequest, apiURL, ApiError, type Problem, type RequestOptions } from '../api/client'
import type { Session, User, Tenant } from '../api/types'
import { readStored, writeStored } from './storage'

const REFRESH_KEY = 'pwf.admin.refresh.v1'
const REFRESH_MARGIN_MS = 30_000

export interface AuthState {
  status: 'anonymous' | 'restoring' | 'ready'
  user: User | null
  tenant: Tenant | null
  role: string
}

export const auth = reactive<AuthState>({
  status: 'anonymous',
  user: null,
  tenant: null,
  role: '',
})

let accessToken: string | null = null
let accessExpiresAt = 0
let refreshToken: string | null = null
let refreshInFlight: Promise<boolean> | null = null

function loadRefreshToken(): string | null {
  // Re-read on first use so a token stored in a previous page load (or a test)
  // is picked up without eagerly touching storage at import time.
  if (!refreshToken) refreshToken = readStored(REFRESH_KEY)
  return refreshToken
}

export function hasRefreshToken(): boolean {
  return Boolean(loadRefreshToken())
}

function adopt(session: Session): void {
  accessToken = session.access_token
  accessExpiresAt = Number.isNaN(Date.parse(session.access_expires_at))
    ? Date.now()
    : Date.parse(session.access_expires_at)
  refreshToken = session.refresh_token
  writeStored(REFRESH_KEY, refreshToken)
  auth.status = 'ready'
  auth.user = session.user
  auth.tenant = session.tenant
  auth.role = session.role
}

export function clearAuth(): void {
  accessToken = null
  accessExpiresAt = 0
  refreshToken = null
  writeStored(REFRESH_KEY, null)
  auth.status = 'anonymous'
  auth.user = null
  auth.tenant = null
  auth.role = ''
}

// refreshSession rotates the stored refresh token. Concurrent callers share one
// request so a burst of 401s cannot invalidate the token chain by double use.
export function refreshSession(): Promise<boolean> {
  if (refreshInFlight) return refreshInFlight
  const token = loadRefreshToken()
  if (!token) return Promise.resolve(false)
  refreshInFlight = apiRequest<Session>('/api/v1/auth/refresh', {
    method: 'POST',
    body: { refresh_token: token },
  })
    .then((session) => {
      adopt(session)
      return true
    })
    .catch((error: unknown) => {
      if (error instanceof ApiError && (error.status === 401 || error.status === 422)) {
        clearAuth()
        return false
      }
      throw error
    })
    .finally(() => {
      refreshInFlight = null
    })
  return refreshInFlight
}

async function ensureAccess(): Promise<string | null> {
  if (accessToken && Date.now() < accessExpiresAt - REFRESH_MARGIN_MS) return accessToken
  if (!hasRefreshToken()) return null
  return (await refreshSession()) ? accessToken : null
}

export async function restoreSession(): Promise<boolean> {
  if (auth.status === 'ready') return true
  if (!hasRefreshToken()) {
    auth.status = 'anonymous'
    return false
  }
  auth.status = 'restoring'
  try {
    return await refreshSession()
  } catch {
    // Network failure: keep the refresh token and show the login screen with a
    // retry path instead of destroying a possibly valid session.
    auth.status = 'anonymous'
    return false
  }
}

export async function signIn(email: string, password: string, tenantSlug: string): Promise<void> {
  const session = await apiRequest<Session>('/api/v1/auth/login', {
    method: 'POST',
    body: { email, password, tenant_slug: tenantSlug },
  })
  adopt(session)
}

export async function signUp(name: string, email: string, password: string, tenantName: string): Promise<void> {
  const session = await apiRequest<Session>('/api/v1/auth/register', {
    method: 'POST',
    body: { name, email, password, tenant_name: tenantName },
  })
  adopt(session)
}

export async function signOut(): Promise<void> {
  const token = loadRefreshToken()
  try {
    if (token) {
      await apiRequest<void>('/api/v1/auth/logout', {
        method: 'POST',
        body: { refresh_token: token },
      })
    }
  } catch {
    // Best effort: the local session is cleared regardless.
  } finally {
    clearAuth()
  }
}

// api is the authenticated seam used by every view. It attaches the access
// token, refreshes once when the access token has expired, and retries exactly
// once after a 401 (a revoked session must not spin).
export async function api<T>(path: string, options: RequestOptions = {}): Promise<T> {
  let token = await ensureAccess()
  if (!token) {
    clearAuth()
    throw new ApiError(401, 'anonymous', '请先登录')
  }
  const call = (session: string) =>
    apiRequest<T>(path, {
      ...options,
      headers: { ...(options.headers ?? {}), Authorization: `Bearer ${session}` },
    })
  try {
    return await call(token)
  } catch (error) {
    if (!(error instanceof ApiError) || error.status !== 401) throw error
    if (!(await refreshSession())) throw error
    token = accessToken
    if (!token) throw error
    return call(token)
  }
}

export async function apiStream(path: string, signal: AbortSignal, extraHeaders: Record<string, string> = {}): Promise<Response> {
  let token = await ensureAccess()
  if (!token) {
    clearAuth()
    throw new ApiError(401, 'anonymous', '请先登录')
  }
  const call = (session: string) => fetch(apiURL(path), {
    headers: { Accept: 'text/event-stream', Authorization: `Bearer ${session}`, ...extraHeaders },
    cache: 'no-store',
    credentials: 'omit',
    signal,
  })
  let response = await call(token)
  if (response.status === 401 && await refreshSession()) {
    token = accessToken
    if (token) response = await call(token)
  }
  if (!response.ok) {
    const text = await response.text()
    let problem: Problem = {}
    try { problem = JSON.parse(text) as Problem } catch { /* response may be plain text */ }
    throw new ApiError(response.status, problem.code ?? `http_${response.status}`, problem.detail ?? problem.title ?? '')
  }
  return response
}
