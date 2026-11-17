export interface Problem {
  type?: string
  title?: string
  status?: number
  detail?: string
  code?: string
  current_version?: number
  tenants?: { id: string; slug: string; name: string }[]
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

export function apiURL(path: string): string {
  return base + path
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  body?: unknown
  headers?: Record<string, string>
  signal?: AbortSignal
}

// apiRequest is the raw network seam: JSON in, problem+json out, no caching.
// Authentication and refresh/retry live in state/session.ts so the login and
// refresh calls themselves never recurse through the retry path.
export async function apiRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  // Multipart uploads must carry the browser-generated boundary, so their
  // Content-Type is never set here and the body is passed through untouched.
  const isForm = typeof FormData !== 'undefined' && options.body instanceof FormData
  const headers: Record<string, string> = { Accept: 'application/json', ...(options.headers ?? {}) }
  if (options.body !== undefined && !isForm) headers['Content-Type'] = 'application/json'

  const response = await fetch(apiURL(path), {
    method: options.method ?? 'GET',
    headers,
    body:
      options.body === undefined
        ? undefined
        : isForm
          ? (options.body as FormData)
          : JSON.stringify(options.body),
    signal: options.signal,
    cache: 'no-store',
    credentials: 'omit',
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
    const problem = (payload ?? {}) as Problem
    throw new ApiError(
      response.status,
      problem.code ?? `http_${response.status}`,
      problem.detail ?? problem.title ?? '',
      problem,
    )
  }
  return payload as T
}
