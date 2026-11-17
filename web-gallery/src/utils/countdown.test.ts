import { describe, expect, it } from 'vitest'
import { countdown } from './countdown'

const start = Date.parse('2026-10-31T10:00:00Z')

describe('countdown', () => {
  it('formats multi-day remainders', () => {
    const result = countdown(start + (2 * 86400 + 3 * 3600 + 4 * 60 + 5) * 1000, start)
    expect(result).toMatchObject({ expired: false, days: 2, hours: 3, minutes: 4, seconds: 5 })
    expect(result.text).toBe('2 天 03:04:05')
  })

  it('omits the day prefix under a day', () => {
    const result = countdown(start + (5 * 3600 + 6 * 60 + 7) * 1000, start)
    expect(result.text).toBe('05:06:07')
  })

  it('expires exactly at the deadline', () => {
    expect(countdown(start, start)).toMatchObject({ expired: true, text: '已截止' })
  })

  it('treats past deadlines and invalid input as expired', () => {
    expect(countdown(start - 5000, start).expired).toBe(true)
    expect(countdown(Number.NaN, start).expired).toBe(true)
  })
})
