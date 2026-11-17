<script setup lang="ts">
// Renders stored annotations and the in-progress draft. Coordinates are
// normalised to the image, so a note lands on the same pixels at any preview
// size or after a retouched revision is uploaded.
import { computed } from 'vue'
import type { Annotation } from '../api/proof'
import { annotationColor, clamp01 } from '../proof/annotation'

const props = withDefaults(
  defineProps<{
    annotations: { id: string; annotation: Annotation }[]
    width?: number
    height?: number
    activeId?: string
  }>(),
  { width: 0, height: 0, activeId: '' },
)

const VIEW_WIDTH = 1000

const viewHeight = computed(() => {
  const w = props.width > 0 ? props.width : 3
  const h = props.height > 0 ? props.height : 2
  return (h / w) * VIEW_WIDTH
})

interface Shape {
  id: string
  type: string
  color: string
  active: boolean
  points: { x: number; y: number }[]
  rect?: { x: number; y: number; w: number; h: number }
  ellipse?: { cx: number; cy: number; rx: number; ry: number }
  polyline?: string
}

const shapes = computed<Shape[]>(() =>
  props.annotations
    .filter((entry) => entry.annotation && Array.isArray(entry.annotation.points))
    .map((entry, index) => {
      const a = entry.annotation
      const color = annotationColor(a, index)
      const points = a.points.map((p) => ({
        x: clamp01(p.x) * VIEW_WIDTH,
        y: clamp01(p.y) * viewHeight.value,
      }))
      const shape: Shape = {
        id: entry.id,
        type: a.type,
        color,
        active: entry.id === props.activeId,
        points,
      }
      if (a.type === 'rect' || a.type === 'ellipse') {
        const [p1, p2] = points
        const x = Math.min(p1.x, p2.x)
        const y = Math.min(p1.y, p2.y)
        const w = Math.abs(p2.x - p1.x)
        const h = Math.abs(p2.y - p1.y)
        if (a.type === 'rect') shape.rect = { x, y, w, h }
        else shape.ellipse = { cx: x + w / 2, cy: y + h / 2, rx: w / 2, ry: h / 2 }
      } else if (a.type === 'freehand') {
        shape.polyline = points.map((p) => `${p.x.toFixed(2)},${p.y.toFixed(2)}`).join(' ')
      }
      return shape
    }),
)

function pointRadius(shape: Shape): number {
  return viewHeight.value * (shape.active ? 0.045 : 0.035)
}
</script>

<template>
  <svg
    class="annotation-layer"
    :viewBox="`0 0 ${VIEW_WIDTH} ${viewHeight}`"
    preserveAspectRatio="none"
    aria-hidden="true"
  >
    <template v-for="shape in shapes" :key="shape.id">
      <circle
        v-if="shape.type === 'point'"
        :cx="shape.points[0].x"
        :cy="shape.points[0].y"
        :r="pointRadius(shape)"
        :fill="shape.color"
        fill-opacity="0.28"
        :stroke="shape.color"
        :stroke-width="shape.active ? 4 : 3"
        vector-effect="non-scaling-stroke"
      />
      <rect
        v-else-if="shape.type === 'rect' && shape.rect"
        :x="shape.rect.x"
        :y="shape.rect.y"
        :width="shape.rect.w"
        :height="shape.rect.h"
        :fill="shape.color"
        fill-opacity="0.08"
        :stroke="shape.color"
        :stroke-width="shape.active ? 4 : 2.4"
        vector-effect="non-scaling-stroke"
        rx="4"
      />
      <ellipse
        v-else-if="shape.type === 'ellipse' && shape.ellipse"
        :cx="shape.ellipse.cx"
        :cy="shape.ellipse.cy"
        :rx="shape.ellipse.rx"
        :ry="shape.ellipse.ry"
        :fill="shape.color"
        fill-opacity="0.08"
        :stroke="shape.color"
        :stroke-width="shape.active ? 4 : 2.4"
        vector-effect="non-scaling-stroke"
      />
      <polyline
        v-else-if="shape.type === 'freehand' && shape.polyline"
        :points="shape.polyline"
        fill="none"
        :stroke="shape.color"
        :stroke-width="shape.active ? 4 : 2.4"
        stroke-linecap="round"
        stroke-linejoin="round"
        vector-effect="non-scaling-stroke"
      />
    </template>
  </svg>
</template>
