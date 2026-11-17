export type NumberInput = { empty: true } | { empty: false; value: number }

// Vue's v-model casts <input type="number"> to a number (or '' when cleared),
// so callers must not assume a string. Returns `empty` for blank input and NaN
// for anything non-numeric, letting callers report their own validation copy.
export function parseNumberInput(value: unknown): NumberInput {
  const raw = String(value ?? '').trim()
  if (raw === '') return { empty: true }
  return { empty: false, value: Number(raw) }
}
