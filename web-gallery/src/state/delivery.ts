import { ApiError, isApiError } from '../api/client'
import {
  createDeliverySession,
  fetchDeliverySummary,
  type CustomerDelivery,
  type DeliveryDownloadGrant,
  type DeliverySessionView,
} from '../api/delivery'

export type DeliveryTerminalCode =
  | 'invalid_link'
  | 'link_expired'
  | 'link_revoked'
  | 'payment_required'
  | 'delivery_not_ready'
  | 'round_not_approved'
  | 'downloads_exhausted'
  | 'delivery_archived'
  | 'delivery_cancelled'

// DeliveryTerminalError ends the visit: retrying cannot help the customer.
export class DeliveryTerminalError extends Error {
  constructor(
    readonly code: DeliveryTerminalCode,
    detail = '',
  ) {
    super(detail || code)
    this.name = 'DeliveryTerminalError'
  }
}

export function isDeliveryTerminalError(error: unknown): error is DeliveryTerminalError {
  return error instanceof DeliveryTerminalError
}

const STORAGE_KEY = 'pwf.delivery.session.v1'
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

// mapDeliveryError converts server problem codes into terminal states. Codes
// such as web_spec_unavailable stay ApiErrors so the view can show them inline
// without ending the visit.
export function mapDeliveryError(error: unknown): unknown {
  if (error instanceof DeliveryTerminalError) return error
  if (error instanceof ApiError) {
    switch (error.code) {
      case 'invalid_link':
      case 'invalid_token':
        return new DeliveryTerminalError('invalid_link', error.detail)
      case 'link_expired':
      case 'expired':
        return new DeliveryTerminalError('link_expired', error.detail)
      case 'link_revoked':
      case 'revoked':
        return new DeliveryTerminalError('link_revoked', error.detail)
      case 'payment_required':
        return new DeliveryTerminalError('payment_required', error.detail)
      case 'delivery_not_ready':
        return new DeliveryTerminalError('delivery_not_ready', error.detail)
      case 'round_not_approved':
        return new DeliveryTerminalError('round_not_approved', error.detail)
      case 'downloads_exhausted':
        return new DeliveryTerminalError('downloads_exhausted', error.detail)
      case 'delivery_archived':
        return new DeliveryTerminalError('delivery_archived', error.detail)
      case 'delivery_cancelled':
        return new DeliveryTerminalError('delivery_cancelled', error.detail)
    }
  }
  return error
}

// applyGrant folds a successful download grant into the cached delivery view so
// the remaining counter stays correct without refetching the summary.
export function applyGrant(delivery: CustomerDelivery, grant: DeliveryDownloadGrant): CustomerDelivery {
  return {
    ...delivery,
    downloads_remaining: Math.max(0, grant.downloads_remaining),
  }
}

export function canDownload(delivery: CustomerDelivery | null): boolean {
  return Boolean(delivery && delivery.downloads_remaining > 0)
}

function sessionExpired(error: unknown): boolean {
  return (
    isApiError(error, 401) &&
    (error.code === 'session_expired' || error.code === 'invalid_session')
  )
}

// DeliverySession is the customer-side download session. It is separate from
// the gallery and proof sessions: a delivery link uses its own JWT issuer, has
// its own token lifecycle and is capped at the link expiry.
export class DeliverySession {
  private jwt = ''
  private expiresAt = 0
  private share = ''

  constructor(private readonly storage: Storage | null = safeStorage()) {}

  get token(): string {
    return this.jwt
  }

  // open exchanges the fragment token for a session unless a still-valid one
  // for this exact link is stored in the tab.
  async open(shareToken: string): Promise<CustomerDelivery> {
    this.share = shareToken
    const stored = this.read()
    if (stored && stored.share === shareToken && stored.expiresAt > Date.now() + RENEW_MARGIN) {
      this.jwt = stored.jwt
      this.expiresAt = stored.expiresAt
      try {
        return await this.refreshSummary()
      } catch (error) {
        if (isDeliveryTerminalError(error)) throw error
        if (!sessionExpired(error)) throw mapDeliveryError(error)
        // The stored session was rejected server-side; exchange again.
      }
    }
    return (await this.exchange()).delivery
  }

  // resume reopens the last delivery when the URL no longer carries the token
  // (the view strips it after the exchange). An expired session is renewed from
  // the token kept in sessionStorage for this tab.
  async resume(): Promise<CustomerDelivery> {
    const stored = this.read()
    if (!stored || !stored.share) throw new DeliveryTerminalError('invalid_link')
    return this.open(stored.share)
  }

  // withSession runs an authenticated call and renews the session exactly once
  // when the JWT expired server-side.
  async withSession<T>(call: (jwt: string) => Promise<T>): Promise<T> {
    if (!this.jwt) throw new DeliveryTerminalError('invalid_link')
    try {
      return await call(this.jwt)
    } catch (error) {
      if (!sessionExpired(error)) throw mapDeliveryError(error)
    }
    await this.exchange()
    try {
      return await call(this.jwt)
    } catch (error) {
      throw mapDeliveryError(error)
    }
  }

  refreshSummary(): Promise<CustomerDelivery> {
    return this.withSession((jwt) => fetchDeliverySummary(jwt))
  }

  private async exchange(): Promise<DeliverySessionView> {
    let view: DeliverySessionView
    try {
      view = await createDeliverySession(this.share)
    } catch (error) {
      throw mapDeliveryError(error)
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
