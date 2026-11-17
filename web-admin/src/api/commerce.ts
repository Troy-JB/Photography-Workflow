import { api } from '../state/session'

export interface CommerceProject {
  id: string
  code: string
  title: string
  state: string
  created_at: string
}

export interface QuoteItem {
  id: string
  kind: string
  description: string
  quantity: number
  unit_price_cents: number
  amount_cents: number
}

export interface Quote {
  id: string
  project_id: string
  version: number
  status: string
  currency: string
  total_cents: number
  deposit_cents: number
  row_version: number
  items: QuoteItem[]
}

export interface PaymentOrder {
  id: string
  project_id: string
	kind: 'deposit' | 'balance' | 'full' | 'change' | 'selection_extra'
	status: string
	amount_cents: number
	pay_url: string
	expires_at: string
}

export interface Page<T> {
  items: T[]
  total: number
}

export function listProjects(): Promise<Page<CommerceProject>> {
	return api('/api/v1/projects?limit=100&offset=0&exclude_closed=true')
}

export function getProject(projectID: string): Promise<CommerceProject> {
  return api(`/api/v1/projects/${encodeURIComponent(projectID)}`)
}

export function listProjectQuotes(projectID: string): Promise<Page<Quote>> {
  return api(`/api/v1/quotes?project_id=${encodeURIComponent(projectID)}&limit=100&offset=0`)
}

export function createQuote(input: {
  project_id: string
  currency: string
  deposit_cents: number
  terms: string
  items: { kind: string; description: string; quantity: number; unit_price_cents: number }[]
}): Promise<Quote> {
  return api('/api/v1/quotes', { method: 'POST', body: input })
}

export function sendQuote(quote: Quote): Promise<Quote> {
  return api(`/api/v1/quotes/${quote.id}/send`, {
    method: 'POST',
    body: { expected_version: quote.row_version },
  })
}

export function listProjectOrders(projectID: string): Promise<Page<PaymentOrder>> {
	return api(`/api/v1/payments/orders?project_id=${encodeURIComponent(projectID)}&limit=100&offset=0`)
}

export function createPaymentOrder(input: { project_id: string; kind: 'deposit' | 'full' }): Promise<PaymentOrder> {
	return api('/api/v1/payments/orders', { method: 'POST', body: input })
}
