import type { InjectionKey } from 'vue'

export interface LazyContext {
  /** Calls onEnter once the element approaches the viewport. */
  observe(element: Element, onEnter: () => void): void
  release(element: Element): void
}

export const LazyKey: InjectionKey<LazyContext> = Symbol('pwf.lazy')

const MAX_PREFETCHED = 24

// One observer for the whole grid keeps 2,000 cards cheap: cards register when
// mounted and are unregistered as the virtual window recycles them.
export function createLazyContext(root: () => HTMLElement | null, margin = '500px'): LazyContext {
  const callbacks = new Map<Element, () => void>()
  const observer = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue
        const callback = callbacks.get(entry.target)
        if (!callback) continue
        callbacks.delete(entry.target)
        observer.unobserve(entry.target)
        callback()
      }
    },
    { root: root(), rootMargin: margin },
  )
  return {
    observe(element, onEnter) {
      callbacks.set(element, onEnter)
      observer.observe(element)
    },
    release(element) {
      callbacks.delete(element)
      observer.unobserve(element)
    },
  }
}

const prefetched = new Set<string>()

// Neighbour prefetch warms the lightbox image without blocking the grid. The
// set is bounded so long sessions do not grow it without limit.
export function prefetchImage(url: string): void {
  if (!url || prefetched.has(url)) return
  if (prefetched.size >= MAX_PREFETCHED) prefetched.clear()
  prefetched.add(url)
  const image = new Image()
  image.decoding = 'async'
  image.src = url
}
