import { ApiError, isApiError } from '../api/client'
import {
  createProofSession,
  fetchProofRound,
  type ProofRound,
  type ProofSessionView,
} from '../api/proof'

export type ProofTerminalCode =
  | 'invalid_token'
  | 'expired'
  | 'revoked'
  | 'too_many_visits'
  | 'gallery_revoked'
  | 'round_closed'

// ProofTerminalError ends the visit: the link, the gallery or the round can no
// longer serve this customer, and retrying would only add load to the API.
export class ProofTerminalError extends Error {
  constructor(
    readonly code: ProofTerminalCode,
    detail = '',
  ) {
    super(detail || code)
    this.name = 'ProofTerminalError'
  }
}

export function isProofTerminalError(error: unknown): error is ProofTerminalError {
  return error instanceof ProofTerminalError
}

const STORAGE_KEY = 'pwf.proof.session.v1'
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

export function mapProofError(error: unknown): unknown {
  if (error instanceof ProofTerminalError) return error
  if (error instanceof ApiError) {
    switch (error.code) {
      case 'invalid_token':
      case 'invalid_session':
        return new ProofTerminalError('invalid_token', error.detail)
      case 'expired':
      case 'session_expired':
        return new ProofTerminalError('expired', error.detail)
      case 'revoked':
      case 'session_revoked':
        return new ProofTerminalError('revoked', error.detail)
      case 'too_many_visits':
        return new ProofTerminalError('too_many_visits', error.detail)
      case 'gallery_revoked':
        return new ProofTerminalError('gallery_revoked', error.detail)
      case 'round_closed':
      case 'round_not_found':
        return new ProofTerminalError('round_closed', error.detail)
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

// ProofSession is the customer-side proof session. It is deliberately separate
// from GallerySession: proof links outlive the selection deadline, use their own
// JWT issuer and are revoked per round token.
export class ProofSession {
  private jwt = ''
  private expiresAt = 0
  private share = ''

  constructor(private readonly storage: Storage | null = safeStorage()) {}

  get token(): string {
    return this.jwt
  }

  // open exchanges the fragment token for a session unless a still-valid one for
  // this exact link is stored in the tab.
  async open(shareToken: string): Promise<ProofRound> {
    this.share = shareToken
    const stored = this.read()
    if (stored && stored.share === shareToken && stored.expiresAt > Date.now() + RENEW_MARGIN) {
      this.jwt = stored.jwt
      this.expiresAt = stored.expiresAt
      try {
        return (await this.refreshRound()).round
      } catch (error) {
        if (isProofTerminalError(error)) throw error
        if (!sessionExpired(error)) throw mapProofError(error)
        // The stored session was rejected server-side; exchange again.
      }
    }
    return (await this.exchange()).round
  }

  // withSession runs an authenticated call and renews the session exactly once
  // when the JWT expired server-side.
  async withSession<T>(call: (jwt: string) => Promise<T>): Promise<T> {
    if (!this.jwt) throw new ProofTerminalError('invalid_token')
    try {
      return await call(this.jwt)
    } catch (error) {
      if (!sessionExpired(error)) throw mapProofError(error)
    }
    await this.exchange()
    try {
      return await call(this.jwt)
    } catch (error) {
      throw mapProofError(error)
    }
  }

  refreshRound(): Promise<{ round: ProofRound }> {
    return this.withSession((jwt) => fetchProofRound(jwt))
  }

  private async exchange(): Promise<ProofSessionView> {
    let view: ProofSessionView
    try {
      view = await createProofSession(this.share)
    } catch (error) {
      throw mapProofError(error)
    }
    this.jwt = view.session
    this.expiresAt = Date.parse(view.expires_at)
    this.persist()
    return view
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
