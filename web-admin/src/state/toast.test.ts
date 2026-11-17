import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { dismiss, toast, toasts } from './toast'

describe('toasts', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    toasts.splice(0, toasts.length)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('keeps only the newest toasts', () => {
    for (let i = 1; i <= 6; i++) toast(`消息 ${i}`, 'info', 0)
    expect(toasts.map((entry) => entry.message)).toEqual(['消息 3', '消息 4', '消息 5', '消息 6'])
  })

  it('dismisses after the ttl', () => {
    toast('保存成功', 'success', 3000)
    vi.advanceTimersByTime(2999)
    expect(toasts).toHaveLength(1)
    vi.advanceTimersByTime(1)
    expect(toasts).toHaveLength(0)
  })

  it('ignores dismissal of an already removed toast', () => {
    const first = toast('第一条', 'info', 0)
    const second = toast('第二条', 'info', 0)
    dismiss(first)
    dismiss(first)
    expect(toasts.map((entry) => entry.id)).toEqual([second])
  })
})
