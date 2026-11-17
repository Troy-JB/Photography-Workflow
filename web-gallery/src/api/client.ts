export interface Problem {
  type?: string
  title?: string
  status?: number
  detail?: string
  code?: string
  current_version?: number
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly detail = '',
    readonly problem: Problem = {},
  ) {
    super(detail || code)
    this.name = 'ApiError'
  }
}

const base = (import.meta.env.VITE_API_BASE ?? '').replace(/\/+$/, '')

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'
  body?: unknown
  session?: string
  idempotencyKey?: string
  signal?: AbortSignal
}

// apiRequest is the only network seam: every call is JSON, problems are
// application/problem+json, and nothing is cached by shared caches.
export async function apiRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'
  if (options.session) headers.Authorization = `Bearer ${options.session}`
  if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey

  const response = await fetch(base + path, {
    method: options.method ?? 'GET',
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    signal: options.signal,
    cache: 'no-store',
    credentials: 'omit',
    referrerPolicy: 'no-referrer',
  })

  const text = await response.text()
  let payload: unknown
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = undefined
    }
  }

  if (!response.ok) {
    const problem = (payload && typeof payload === 'object' ? payload : {}) as Problem
    throw new ApiError(
      response.status,
      problem.code || `http_${response.status}`,
      problem.detail || problem.title || response.statusText,
      problem,
    )
  }
  return payload as T
}

export function isApiError(error: unknown, status?: number, code?: string): error is ApiError {
  if (!(error instanceof ApiError)) return false
  if (status !== undefined && error.status !== status) return false
  if (code !== undefined && error.code !== code) return false
  return true
}
