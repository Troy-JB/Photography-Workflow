export interface VirtualInput {
  count: number
  columns: number
  rowHeight: number
  gap: number
  scrollTop: number
  viewportHeight: number
  overscan?: number
}

export interface VirtualWindow {
  /** First rendered row (inclusive). */
  startRow: number
  /** Last rendered row (exclusive). */
  endRow: number
  /** Pixel offset of the first rendered row inside the spacer. */
  offsetY: number
  /** Height of the full spacer, matching the virtual row count. */
  totalHeight: number
}

// The grid renders only the rows around the viewport; everything else is
// spacer height. Pure function so the arithmetic is unit-tested.
export function virtualWindow(input: VirtualInput): VirtualWindow {
  const columns = Math.max(1, Math.floor(input.columns) || 1)
  const rowHeight = Math.max(1, input.rowHeight || 1)
  const gap = Math.max(0, input.gap || 0)
  const overscan = Math.max(0, Math.floor(input.overscan ?? 2))
  const stride = rowHeight + gap
  const count = Math.max(0, Math.floor(input.count) || 0)
  const totalRows = Math.ceil(count / columns)
  const totalHeight = totalRows === 0 ? 0 : totalRows * stride - gap
  if (totalRows === 0) {
    return { startRow: 0, endRow: 0, offsetY: 0, totalHeight: 0 }
  }
  const maxScroll = Math.max(0, totalHeight - Math.max(0, input.viewportHeight))
  const scrollTop = Math.min(Math.max(0, input.scrollTop || 0), maxScroll)
  const firstVisible = Math.min(totalRows - 1, Math.floor(scrollTop / stride))
  const lastVisible = Math.min(
    totalRows - 1,
    Math.floor((scrollTop + Math.max(0, input.viewportHeight) - 1) / stride),
  )
  const startRow = Math.max(0, firstVisible - overscan)
  const endRow = Math.min(totalRows, lastVisible + 1 + overscan)
  return { startRow, endRow, offsetY: startRow * stride, totalHeight }
}
