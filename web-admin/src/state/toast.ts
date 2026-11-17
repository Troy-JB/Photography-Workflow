import { reactive } from 'vue'

export type ToastKind = 'info' | 'success' | 'error'

export interface Toast {
  id: number
  kind: ToastKind
  message: string
}

let nextID = 1

export const toasts = reactive<Toast[]>([])

const MAX_VISIBLE = 4

export function toast(message: string, kind: ToastKind = 'info', ttl = 4200): number {
  const id = nextID++
  toasts.push({ id, kind, message })
  while (toasts.length > MAX_VISIBLE) dismiss(toasts[0].id)
  if (ttl > 0) setTimeout(() => dismiss(id), ttl)
  return id
}

export function dismiss(id: number): void {
  const index = toasts.findIndex((entry) => entry.id === id)
  if (index >= 0) toasts.splice(index, 1)
}
