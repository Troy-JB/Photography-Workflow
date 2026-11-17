// Small storage wrapper so the session module also runs under Vitest's node
// environment, where localStorage does not exist.
const memory = new Map<string, string>()

function backing(): Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> {
  try {
    if (typeof localStorage !== 'undefined') return localStorage
  } catch {
    // Private mode may throw on access; fall through to memory.
  }
  return {
    getItem: (key) => memory.get(key) ?? null,
    setItem: (key, value) => void memory.set(key, value),
    removeItem: (key) => void memory.delete(key),
  }
}

export function readStored(key: string): string | null {
  try {
    return backing().getItem(key)
  } catch {
    return null
  }
}

export function writeStored(key: string, value: string | null): void {
  try {
    if (value === null) backing().removeItem(key)
    else backing().setItem(key, value)
  } catch {
    // Storage full or unavailable: the session simply is not persisted.
  }
}
