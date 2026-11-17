const galleryBase = (import.meta.env.VITE_GALLERY_BASE_URL || (import.meta.env.DEV ? 'http://localhost:5174' : '')).replace(/\/+$/, '')

export function isCustomerShareURL(value: string): boolean {
  if (!galleryBase || !value.startsWith(`${galleryBase}/#/`)) return false
  return /^https?:\/\/[^/]+\/#\/(a|p|d)\/[A-Za-z0-9_-]+(?:[/?#].*)?$/.test(value)
}

export function allowedShareBase(): string {
  return galleryBase
}

export function selectionShareToken(value: string): string | null {
  if (!isCustomerShareURL(value)) return null
  return value.match(/\/#\/a\/([A-Za-z0-9_-]+)/)?.[1] ?? null
}
