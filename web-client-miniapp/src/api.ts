const ACCESS_KEY = 'pwf.client.access.v1'
const REFRESH_KEY = 'pwf.client.refresh.v1'
const CUSTOMER_KEY = 'pwf.client.profile.v1'
const base = (import.meta.env.VITE_API_BASE ?? '').replace(/\/+$/, '')

export interface CustomerSession {
  access_token: string
  access_expires_at: string
  refresh_token: string
  refresh_expires_at: string
  customer: { id: string; name: string; phone: string }
  role: 'client'
}

export interface PhotographerBookingTarget {
  photographer_code: string
  studio_id: string
  studio_name: string
  services: BookingService[]
}

export interface BookingService {
  id: string
  name: string
  description: string
  duration_minutes: number
  price_cents: number
  included_retouch_count: number
  extra_retouch_unit_price_cents: number
}

export interface Slot {
  starts_at: string
  ends_at: string
}

export function getSession(): CustomerSession | null {
  const access = uni.getStorageSync(ACCESS_KEY)
  const customer = uni.getStorageSync(CUSTOMER_KEY)
  if (!access || !customer) return null
  return { access_token: access, refresh_token: uni.getStorageSync(REFRESH_KEY), customer, role: 'client', access_expires_at: '', refresh_expires_at: '' }
}

function storeSession(session: CustomerSession): void {
  uni.setStorageSync(ACCESS_KEY, session.access_token)
  uni.setStorageSync(REFRESH_KEY, session.refresh_token)
  uni.setStorageSync(CUSTOMER_KEY, session.customer)
}

export function request<T>(path: string, method: 'GET' | 'POST' = 'GET', data?: Record<string, unknown>, authenticated = false): Promise<T> {
  const session = getSession()
  return new Promise((resolve, reject) => {
    uni.request({
      url: base + path,
      method,
      data,
      header: {
        Accept: 'application/json',
        ...(data === undefined ? {} : { 'Content-Type': 'application/json' }),
        ...(authenticated && session ? { Authorization: `Bearer ${session.access_token}` } : {}),
      },
      success: (response) => {
        if (response.statusCode >= 200 && response.statusCode < 300) {
          resolve(response.data as T)
          return
        }
        if (response.statusCode === 401 && authenticated && session?.refresh_token) {
          refresh()
            .then(() => request<T>(path, method, data, true))
            .then(resolve)
            .catch((error: unknown) => {
              clearSession()
              reject(error)
            })
          return
        }
        const problem = response.data as { detail?: string; title?: string; code?: string }
        reject(new Error(problem?.detail || problem?.title || problem?.code || `HTTP ${response.statusCode}`))
      },
      fail: reject,
    })
  })
}

export async function login(name: string, phoneCode: string, devPhone: string, inviteToken: string): Promise<CustomerSession> {
  let code = ''
  // H5 has no WeChat identity provider; local API dev-login is explicitly opt-in.
  if (import.meta.env.DEV && !phoneCode) {
    code = `dev:${devPhone.replace(/\D/g, '').slice(-8) || 'demo'}`
  } else {
    const result = await new Promise<{ code: string }>((resolve, reject) => {
      uni.login({ provider: 'weixin', success: resolve, fail: reject })
    })
    code = result.code
  }
  const session = await request<CustomerSession>('/c/v1/auth/wechat', 'POST', {
    code,
    phone_code: phoneCode,
    phone: devPhone,
    name,
    invite_token: inviteToken,
  })
  storeSession(session)
  return session
}

export async function requestPhoneCode(phone: string): Promise<string> {
  const result = await request<{ development_code?: string }>('/c/v1/auth/phone/code', 'POST', { phone })
  return result.development_code ?? ''
}

export async function loginWithPhone(name: string, phone: string, code: string): Promise<void> {
  const session = await request<CustomerSession>('/c/v1/auth/phone/login', 'POST', { name, phone, code })
  storeSession(session)
}

export function refresh(): Promise<void> {
  const token = uni.getStorageSync(REFRESH_KEY)
  if (!token) return Promise.reject(new Error('请重新授权登录'))
  return request<CustomerSession>('/c/v1/auth/refresh', 'POST', { refresh_token: token }).then(storeSession)
}

export function clearSession(): void {
  uni.removeStorageSync(ACCESS_KEY)
  uni.removeStorageSync(REFRESH_KEY)
  uni.removeStorageSync(CUSTOMER_KEY)
}
