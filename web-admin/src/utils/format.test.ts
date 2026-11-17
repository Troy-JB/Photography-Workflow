import { describe, expect, it } from 'vitest'
import {
  deadlineLabel,
  decisionLabel,
  formatDateTime,
  fromLocalInput,
  statusLabel,
  toLocalInput,
} from './format'

describe('formatDateTime', () => {
  it('renders local date and time without seconds', () => {
    const value = new Date(2026, 9, 31, 15, 52, 30).toISOString()
    expect(formatDateTime(value)).toBe('2026-10-31 15:52')
  })

  it('returns a dash for missing values', () => {
    expect(formatDateTime(null)).toBe('—')
    expect(formatDateTime(undefined)).toBe('—')
    expect(formatDateTime('not-a-date')).toBe('—')
  })
})

describe('deadlineLabel', () => {
  const now = Date.parse('2026-10-31T08:00:00Z')

  it('counts hours for near deadlines', () => {
    const value = new Date(now + 3 * 3_600_000).toISOString()
    expect(deadlineLabel(value, now)).toContain('剩余 3 小时')
  })

  it('counts days beyond 24 hours', () => {
    const value = new Date(now + 50 * 3_600_000).toISOString()
    expect(deadlineLabel(value, now)).toContain('剩余 2 天')
  })

  it('marks past deadlines as expired', () => {
    const value = new Date(now - 60_000).toISOString()
    expect(deadlineLabel(value, now)).toContain('已过期')
  })

  it('treats an absent deadline as unlimited', () => {
    expect(deadlineLabel(null, now)).toBe('不限时')
  })
})

describe('datetime-local conversion', () => {
  it('round-trips a timestamp', () => {
    const iso = new Date(2026, 10, 2, 9, 5).toISOString()
    const local = toLocalInput(iso)
    expect(local).toBe('2026-11-02T09:05')
    expect(fromLocalInput(local)).toBe(iso)
  })

  it('returns empty values for blank or invalid input', () => {
    expect(toLocalInput(null)).toBe('')
    expect(toLocalInput('nope')).toBe('')
    expect(fromLocalInput('')).toBeNull()
    expect(fromLocalInput('nope')).toBeNull()
  })
})

describe('labels', () => {
  it('maps gallery statuses', () => {
    expect(statusLabel('draft')).toBe('草稿')
    expect(statusLabel('published')).toBe('已发布')
    expect(statusLabel('weird')).toBe('weird')
  })

  it('maps decisions', () => {
    expect(decisionLabel('selected')).toBe('已选')
    expect(decisionLabel('rejected')).toBe('已弃')
    expect(decisionLabel('none')).toBe('未决定')
  })
})
