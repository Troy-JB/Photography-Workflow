import { api } from '../state/session'

export interface Resource {
  id: string
  kind: string
  name: string
  active: boolean
  version: number
}

export interface AvailabilityRule {
  id?: string
  weekday: number
  start_minute: number
  end_minute: number
}

export interface BookingService {
  id: string
  resource_id: string
  name: string
  description: string
  duration_minutes: number
  price_cents: number
  included_retouch_count: number
  extra_retouch_unit_price_cents: number
  active: boolean
  version: number
}

export interface Booking {
  id: string
  project_id: string
  status: 'held' | 'confirmed' | 'cancelled' | 'expired'
  starts_at: string
  ends_at: string
  hold_expires_at?: string
  version: number
}

export interface BookingPage {
  items: Booking[]
  total: number
}

export function listResources(): Promise<{ items: Resource[] }> {
  return api('/api/v1/resources?kind=photographer')
}

export function createPhotographerResource(name: string): Promise<Resource> {
  return api('/api/v1/resources', {
    method: 'POST',
    body: { kind: 'photographer', name, active: true, buffer_before_minutes: 0, buffer_after_minutes: 0 },
  })
}

export function listServices(): Promise<{ items: BookingService[] }> {
  return api('/c/v1/admin/services')
}

export function saveService(service: Partial<BookingService> & Pick<BookingService, 'name' | 'resource_id' | 'duration_minutes' | 'price_cents'>): Promise<BookingService> {
  if (service.id) {
    return api(`/c/v1/admin/services/${service.id}`, {
      method: 'PATCH',
      body: {
        name: service.name,
        description: service.description ?? '',
        duration_minutes: service.duration_minutes,
        price_cents: service.price_cents,
        included_retouch_count: service.included_retouch_count,
        extra_retouch_unit_price_cents: service.extra_retouch_unit_price_cents,
        active: service.active ?? true,
        expected_version: service.version,
      },
    })
  }
  return api('/c/v1/admin/services', { method: 'POST', body: service })
}

export function getAvailability(resourceID: string): Promise<{ resource_id: string; version: number; rules: AvailabilityRule[] }> {
  return api(`/api/v1/resources/${resourceID}/availability`)
}

export function setAvailability(resourceID: string, expectedVersion: number, rules: AvailabilityRule[]): Promise<{ resource_id: string; version: number; rules: AvailabilityRule[] }> {
  return api(`/api/v1/resources/${resourceID}/availability`, {
    method: 'PUT',
    body: { expected_version: expectedVersion, rules },
  })
}

export function listBookings(status = 'held'): Promise<BookingPage> {
  const query = new URLSearchParams({ limit: '100', offset: '0' })
  if (status) query.set('status', status)
  return api(`/api/v1/bookings?${query.toString()}`)
}

export function confirmBooking(booking: Booking): Promise<Booking> {
  return api(`/api/v1/bookings/${booking.id}/confirm`, { method: 'POST', body: { expected_version: booking.version } })
}

export function cancelBooking(booking: Booking): Promise<Booking> {
  return api(`/api/v1/bookings/${booking.id}/cancel`, {
    method: 'POST',
    body: { expected_version: booking.version, reason: 'photographer_cancelled' },
  })
}

export function createCustomerInvite(): Promise<{
  id: string
  token: string
  url: string
  mini_program_path: string
  mini_program_code: string
  expires_at: string
}> {
  return api('/c/v1/admin/invites', { method: 'POST', body: {} })
}
