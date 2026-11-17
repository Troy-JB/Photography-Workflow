import { reactive } from 'vue'
import { ApiError, isApiError } from '../api/client'
import { createSession, fetchAlbum } from '../api/gallery'
import type { Album, SessionView } from '../api/types'

export type TerminalCode =
  | 'invalid_token'
  | 'expired'
  | 'revoked'
  | 'too_many_visits'
  | 'gallery_expired'
  | 'gallery_revoked'
  | 'balance_unpaid'

// TerminalError ends the visit: retrying cannot help the customer.
export class TerminalError extends Error {
  constructor(
    readonly code: TerminalCode,
    detail = '',
  ) {
    super(detail || code)
    this.name = 'TerminalError'
  }
}

export function isTerminalError(error: unknown): error is TerminalError {
  return error instanceof TerminalError
}

const STORAGE_KEY = 'pwf.gallery.session.v1'
const RENEW_MARGIN = 60_000

interface StoredSession {
  share: string
  jwt: string
  expiresAt: number
}

function safeStorage(): Storage | null {
  try {
    return typeof sessionStorage === 'undefined' ? null : sessionStorage
  } catch {
    return null
  }
}

// mapError converts server problem codes into terminal states. Everything else
// stays an ApiError so the view can retry or show a validation message.
export function mapError(error: unknown): unknown {
  if (error instanceof TerminalError) return error
  if (error instanceof ApiError) {
    switch (error.code) {
      case 'invalid_token':
        return new TerminalError('invalid_token', error.detail)
      case 'expired':
        return new TerminalError('expired', error.detail)
      case 'revoked':
      case 'session_revoked':
        return new TerminalError('revoked', error.detail)
      case 'too_many_visits':
        return new TerminalError('too_many_visits', error.detail)
      case 'gallery_expired':
        return new TerminalError('gallery_expired', error.detail)
      case 'gallery_revoked':
        return new TerminalError('gallery_revoked', error.detail)
      case 'balance_unpaid':
        return new TerminalError('balance_unpaid', error.detail)
    }
  }
  return error
}

function sessionExpired(error: unknown): boolean {
  return (
    isApiError(error, 401) &&
    (error.code === 'session_expired' || error.code === 'invalid_session')
  )
}

export class GallerySession {
  readonly album = reactive<{ value: Album | null }>({ value: null })
  private jwt = ''
  private expiresAt = 0
  private share = ''

  constructor(private readonly storage: Storage | null = safeStorage()) {}

  get token(): string {
    return this.jwt
  }

  // open exchanges the fragment token for a 2h session unless a still-valid one
  // for this exact link is stored in the tab.
  async open(shareToken: string): Promise<Album> {
    this.share = shareToken
    const stored = this.read()
    if (stored && stored.share === shareToken && stored.expiresAt > Date.now() + RENEW_MARGIN) {
      this.jwt = stored.jwt
      this.expiresAt = stored.expiresAt
      try {
        return await this.refreshAlbum()
      } catch (error) {
        if (isTerminalError(error)) throw error
        if (!sessionExpired(error)) throw mapError(error)
        // Stored JWT was rejected; fall through to a fresh exchange.
      }
    }
    return this.exchange()
  }

  // withSession runs an authenticated call and renews the session exactly once
  // when the JWT expired server-side (rotation, restart or clock skew).
  async withSession<T>(call: (jwt: string) => Promise<T>): Promise<T> {
    if (!this.jwt) throw new TerminalError('invalid_token')
    try {
      return await call(this.jwt)
    } catch (error) {
      if (!sessionExpired(error)) throw mapError(error)
    }
    await this.exchange()
    try {
      return await call(this.jwt)
    } catch (error) {
      throw mapError(error)
    }
  }

  async refreshAlbum(): Promise<Album> {
    const album = await this.withSession((jwt) => fetchAlbum(jwt))
    this.album.value = album
    return album
  }

  private async exchange(): Promise<Album> {
    let view: SessionView
    try {
      view = await createSession(this.share)
    } catch (error) {
      throw mapError(error)
    }
    this.jwt = view.session
    this.expiresAt = Date.parse(view.expires_at)
    this.album.value = view.album
    this.persist()
    return view.album
  }

  private read(): StoredSession | null {
    if (!this.storage) return null
    try {
      const raw = this.storage.getItem(STORAGE_KEY)
      if (!raw) return null
      const parsed = JSON.parse(raw) as StoredSession
      if (typeof parsed.share !== 'string' || typeof parsed.jwt !== 'string') return null
      return parsed
    } catch {
      return null
    }
  }

  private persist(): void {
    if (!this.storage) return
    try {
      this.storage.setItem(
        STORAGE_KEY,
        JSON.stringify({ share: this.share, jwt: this.jwt, expiresAt: this.expiresAt }),
      )
    } catch {
      // Private mode may reject writes; the in-memory session still works.
    }
  }
}
