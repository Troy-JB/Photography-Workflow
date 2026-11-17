import { describe, expect, it } from 'vitest'
import { isOnDate, orderFrequency, type WorkflowItem } from './workflow'

const item = (orderAt: string): WorkflowItem => ({
  id: orderAt,
  code: 'P1',
  title: 'Portrait',
  client_name: 'Lin',
  project_state: 'booked',
  workflow_stage: 'shooting',
  workflow_stage_label: '拍摄中',
  payment_status: 'deposit_paid',
  currency: 'CNY',
  amount_cents: 10000,
  order_at: orderAt,
  created_at: orderAt,
  gallery_submitted: false,
})

describe('workflow dashboard helpers', () => {
  it('matches a local calendar day rather than a fixed 24-hour duration', () => {
    expect(isOnDate('2026-11-04T08:00:00', new Date(2026, 10, 4, 23))).toBe(true)
    expect(isOnDate(undefined, new Date())).toBe(false)
  })

  it('counts order events in each selected day window', () => {
    const now = new Date(2026, 10, 4, 12)
    const rows = orderFrequency([
      item(new Date(2026, 10, 4, 8).toISOString()),
      item(new Date(2026, 10, 3, 8).toISOString()),
      item(new Date(2026, 9, 1, 8).toISOString()),
    ], 7, now)
    expect(rows).toHaveLength(7)
    expect(rows.at(-1)?.count).toBe(1)
    expect(rows.at(-2)?.count).toBe(1)
  })
})
