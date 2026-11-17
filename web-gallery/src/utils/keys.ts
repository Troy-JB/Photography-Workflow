// Every mutation carries a fresh key. Reusing one across attempts would let the
// server replay a stored error for a request the client has since retried.
export function requestKey(scope: string): string {
  const random =
    typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
      ? crypto.randomUUID()
      : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  return `${scope}:${random}`
}
