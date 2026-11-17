import { describe, expect, it } from 'vitest'
import { parseNumberInput } from './number'

describe('parseNumberInput', () => {
  it('treats blank and whitespace as empty', () => {
    expect(parseNumberInput('')).toEqual({ empty: true })
    expect(parseNumberInput('   ')).toEqual({ empty: true })
    expect(parseNumberInput(null)).toEqual({ empty: true })
    expect(parseNumberInput(undefined)).toEqual({ empty: true })
  })

  it('accepts strings and the numbers v-model produces', () => {
    expect(parseNumberInput('3')).toEqual({ empty: false, value: 3 })
    expect(parseNumberInput(3)).toEqual({ empty: false, value: 3 })
    expect(parseNumberInput('0')).toEqual({ empty: false, value: 0 })
  })

  it('surfaces invalid values as NaN for caller-side copy', () => {
    expect(parseNumberInput('3.5').empty).toBe(false)
    expect(Number.isNaN((parseNumberInput('3.5') as { value: number }).value)).toBe(false)
    const bad = parseNumberInput('abc')
    expect(bad.empty).toBe(false)
    expect(Number.isNaN((bad as { value: number }).value)).toBe(true)
  })
})
