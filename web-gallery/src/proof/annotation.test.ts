import { describe, expect, it } from 'vitest'
import {
  annotationLabel,
  dragAnnotation,
  freehandAnnotation,
  normalizePoint,
  pointAnnotation,
} from './annotation'

const BOX = { left: 100, top: 50, width: 800, height: 400 }

describe('normalizePoint', () => {
  it('maps the image box to 0..1', () => {
    expect(normalizePoint(100, 50, BOX)).toEqual({ x: 0, y: 0 })
    expect(normalizePoint(500, 250, BOX)).toEqual({ x: 0.5, y: 0.5 })
    expect(normalizePoint(900, 450, BOX)).toEqual({ x: 1, y: 1 })
  })

  it('clamps points outside the image', () => {
    expect(normalizePoint(0, 0, BOX)).toEqual({ x: 0, y: 0 })
    expect(normalizePoint(2000, 900, BOX)).toEqual({ x: 1, y: 1 })
  })

  it('rejects a zero-sized box instead of dividing by zero', () => {
    expect(normalizePoint(10, 10, { left: 0, top: 0, width: 0, height: 0 })).toBeNull()
  })
})

describe('dragAnnotation', () => {
  it('sorts corners so x1<x2 and y1<y2 whatever the drag direction', () => {
    const shape = dragAnnotation('rect', { x: 0.8, y: 0.7 }, { x: 0.2, y: 0.1 })
    expect(shape).toMatchObject({
      schema: 'annotation.v1',
      type: 'rect',
      points: [
        { x: 0.2, y: 0.1 },
        { x: 0.8, y: 0.7 },
      ],
    })
  })

  it('drops degenerate drags the server would reject', () => {
    expect(dragAnnotation('ellipse', { x: 0.5, y: 0.5 }, { x: 0.501, y: 0.9 })).toBeNull()
    expect(dragAnnotation('rect', { x: 0.5, y: 0.5 }, { x: 0.9, y: 0.501 })).toBeNull()
  })
})

describe('freehandAnnotation', () => {
  it('thins points that are too close together', () => {
    const shape = freehandAnnotation([
      { x: 0.1, y: 0.1 },
      { x: 0.1001, y: 0.1 },
      { x: 0.3, y: 0.2 },
    ])
    expect(shape?.points).toHaveLength(2)
  })

  it('needs real movement', () => {
    expect(freehandAnnotation([{ x: 0.5, y: 0.5 }])).toBeNull()
    expect(
      freehandAnnotation([
        { x: 0.5, y: 0.5 },
        { x: 0.5005, y: 0.5 },
      ]),
    ).toBeNull()
  })

  it('caps a long stroke at 256 points', () => {
    const points = Array.from({ length: 600 }, (_, index) => ({
      x: index / 600,
      y: (index % 40) / 40,
    }))
    expect(freehandAnnotation(points)?.points.length).toBeLessThanOrEqual(256)
  })
})

describe('pointAnnotation', () => {
  it('clamps out-of-range coordinates', () => {
    expect(pointAnnotation({ x: -1, y: 2 }).points).toEqual([{ x: 0, y: 1 }])
  })
})

describe('annotationLabel', () => {
  it('labels shapes and plain comments', () => {
    expect(annotationLabel(null)).toBe('文字评论')
    expect(annotationLabel(pointAnnotation({ x: 0.5, y: 0.5 }))).toBe('点选批注')
    expect(annotationLabel({ schema: 'annotation.v1', type: 'rect', points: [] })).toBe('矩形批注')
  })
})
