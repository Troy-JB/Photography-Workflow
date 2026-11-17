import { describe, expect, it } from 'vitest'
import { virtualWindow } from './virtual'

const base = {
  columns: 3,
  rowHeight: 100,
  gap: 10,
  viewportHeight: 300,
  overscan: 1,
}

describe('virtualWindow', () => {
  it('returns an empty window for an empty grid', () => {
    expect(virtualWindow({ ...base, count: 0, scrollTop: 0 })).toEqual({
      startRow: 0,
      endRow: 0,
      offsetY: 0,
      totalHeight: 0,
    })
  })

  it('computes total height without a trailing gap', () => {
    // 7 items / 3 columns = 3 rows; 3 * 110 - 10
    const result = virtualWindow({ ...base, count: 7, scrollTop: 0 })
    expect(result.totalHeight).toBe(320)
  })

  it('renders visible rows plus overscan', () => {
    const result = virtualWindow({ ...base, count: 60, scrollTop: 0 })
    expect(result.startRow).toBe(0)
    // viewport 300 -> rows 0..2 visible, +1 overscan
    expect(result.endRow).toBe(4)
    expect(result.offsetY).toBe(0)
  })

  it('moves the window while scrolling', () => {
    // stride 110, scrollTop 1000 -> first visible row 9
    const result = virtualWindow({ ...base, count: 300, scrollTop: 1000 })
    expect(result.startRow).toBe(8)
    expect(result.endRow).toBe(13)
    expect(result.offsetY).toBe(880)
  })

  it('clamps scroll past the end to the last rows', () => {
    const result = virtualWindow({ ...base, count: 6, scrollTop: 100000 })
    expect(result.startRow).toBe(0)
    expect(result.endRow).toBe(2)
  })

  it('treats invalid columns and counts defensively', () => {
    const result = virtualWindow({
      ...base,
      columns: 0,
      count: Number.NaN,
      scrollTop: -50,
      viewportHeight: 0,
    })
    expect(result).toEqual({ startRow: 0, endRow: 0, offsetY: 0, totalHeight: 0 })
  })
})
