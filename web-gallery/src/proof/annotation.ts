import type { Annotation, AnnotationPoint, AnnotationType } from '../api/proof'

export const ANNOTATION_COLORS = ['#e0b45c', '#5fbf87', '#7fb6ff', '#e0665c', '#c8b6ff', '#f08f4f']

// Drag shapes smaller than this (in image-relative units) are treated as taps so
// the server never receives a degenerate rect/ellipse.
const MIN_DRAG = 0.008
const FREEHAND_STEP = 0.0025
const MAX_FREEHAND = 256

export interface Box {
  left: number
  top: number
  width: number
  height: number
}

export function clamp01(value: number): number {
  if (!Number.isFinite(value)) return 0
  return Math.min(1, Math.max(0, value))
}

// normalizePoint converts a pointer position into annotation space: 0,0 is the
// top-left of the displayed image, 1,1 the bottom-right, so the note lands on
// the same pixels whatever preview size or revision the round renders later.
export function normalizePoint(clientX: number, clientY: number, box: Box): AnnotationPoint | null {
  if (!(box.width > 0) || !(box.height > 0)) return null
  const x = clamp01((clientX - box.left) / box.width)
  const y = clamp01((clientY - box.top) / box.height)
  return { x, y }
}

export function pointAnnotation(point: AnnotationPoint, color = ANNOTATION_COLORS[0]): Annotation {
  return {
    schema: 'annotation.v1',
    type: 'point',
    points: [{ x: clamp01(point.x), y: clamp01(point.y) }],
    color,
  }
}

// dragAnnotation builds a rect/ellipse from any drag direction. The validator
// requires x1<x2 and y1<y2, so corners are sorted here and tiny drags dropped.
export function dragAnnotation(
  type: 'rect' | 'ellipse',
  start: AnnotationPoint,
  end: AnnotationPoint,
  color = ANNOTATION_COLORS[0],
): Annotation | null {
  const x1 = clamp01(Math.min(start.x, end.x))
  const y1 = clamp01(Math.min(start.y, end.y))
  const x2 = clamp01(Math.max(start.x, end.x))
  const y2 = clamp01(Math.max(start.y, end.y))
  if (x2 - x1 < MIN_DRAG || y2 - y1 < MIN_DRAG) return null
  return {
    schema: 'annotation.v1',
    type,
    points: [
      { x: x1, y: y1 },
      { x: x2, y: y2 },
    ],
    color,
  }
}

export function freehandAnnotation(
  points: AnnotationPoint[],
  color = ANNOTATION_COLORS[0],
): Annotation | null {
  const cleaned: AnnotationPoint[] = []
  for (const raw of points) {
    const point = { x: clamp01(raw.x), y: clamp01(raw.y) }
    const last = cleaned[cleaned.length - 1]
    if (last && Math.hypot(point.x - last.x, point.y - last.y) < FREEHAND_STEP) continue
    cleaned.push(point)
    if (cleaned.length >= MAX_FREEHAND) break
  }
  if (cleaned.length < 2) return null
  const first = cleaned[0]
  const last = cleaned[cleaned.length - 1]
  if (Math.hypot(last.x - first.x, last.y - first.y) < MIN_DRAG) return null
  return { schema: 'annotation.v1', type: 'freehand', points: cleaned, color }
}

export function annotationColor(annotation: Annotation, index = 0): string {
  return annotation.color || ANNOTATION_COLORS[index % ANNOTATION_COLORS.length]
}

export function annotationLabel(annotation: Annotation | null | undefined): string {
  switch (annotation?.type) {
    case 'point':
      return '点选批注'
    case 'rect':
      return '矩形批注'
    case 'ellipse':
      return '椭圆批注'
    case 'freehand':
      return '手绘批注'
    default:
      return '文字评论'
  }
}

export function annotationTypes(): { key: AnnotationType; label: string }[] {
  return [
    { key: 'point', label: '点' },
    { key: 'rect', label: '框' },
    { key: 'ellipse', label: '椭圆' },
    { key: 'freehand', label: '手绘' },
  ]
}
