import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api/client'
import type { Album, PageItem, SelectionResult } from '../api/types'
import { TerminalError } from '../state/session'
import { SelectionEngine, type SelectionApi } from './engine'

function album(overrides: Partial<Album> = {}): Album {
  return {
    id: 'gallery-1',
    project_id: 'project-1',
    title: '选片',
    selection_limit: null,
    download_allowed: false,
    selection_version: 0,
    counts: { assets: 10, selected: 0, rejected: 0, undecided: 10 },
    ...overrides,
  }
}

function result(version: number, selected: number, rejected = 0): SelectionResult {
  return {
    selection_version: version,
    counts: { assets: 10, selected, rejected, undecided: 10 - selected - rejected },
  }
}

function item(id: string, decision: '' | 'selected' | 'rejected' = ''): PageItem {
  return {
    asset_id: id,
    filename: `${id}.jpg`,
    position: 1,
    width: 100,
    height: 100,
    is_video: false,
    thumb_url: `${id}-thumb`,
    display_url: `${id}-display`,
    decision,
  }
}

function harness() {
  const apply = vi.fn<SelectionApi['apply']>()
  const refresh = vi.fn<SelectionApi['refresh']>()
  const submit = vi.fn<SelectionApi['submit']>()
  const api: SelectionApi = { apply, refresh, submit }
  return { api, apply, refresh, submit }
}

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('SelectionEngine', () => {
  it('applies decisions optimistically and flushes after the debounce', async () => {
    const { api, apply } = harness()
    apply.mockResolvedValue(result(1, 1))
    const engine = new SelectionEngine(api, album())

    expect(engine.toggle('a1', 'selected')).toEqual({ ok: true })
    expect(engine.optimisticCounts().selected).toBe(1)
    expect(engine.state.pending).toBe(1)
    expect(apply).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(450)

    expect(apply).toHaveBeenCalledTimes(1)
    expect(apply.mock.calls[0][0]).toEqual({
      expected_version: 0,
      items: [{ asset_id: 'a1', decision: 'selected' }],
    })
    expect(engine.state.version).toBe(1)
    expect(engine.state.counts.selected).toBe(1)
    expect(engine.state.pending).toBe(0)
    engine.dispose()
  })

  it('batches writes at 200 items', async () => {
    const { api, apply } = harness()
    apply.mockImplementation(async (body) => result(body.expected_version + 1, body.items.length))
    const engine = new SelectionEngine(api, album({ counts: { assets: 300, selected: 0, rejected: 0, undecided: 300 } }))

    for (let index = 0; index < 205; index++) {
      engine.setDecision(`a${index}`, 'selected')
    }
    await engine.flush()
    expect(apply.mock.calls[0][0].items).toHaveLength(200)

    await vi.advanceTimersByTimeAsync(10)
    expect(apply).toHaveBeenCalledTimes(2)
    expect(apply.mock.calls[1][0].items).toHaveLength(5)
    expect(engine.state.pending).toBe(0)
    engine.dispose()
  })

  it('rejects selections beyond the limit without changing state', () => {
    const { api } = harness()
    const engine = new SelectionEngine(api, album({ selection_limit: 2 }))

    expect(engine.toggle('a1', 'selected').ok).toBe(true)
    expect(engine.toggle('a2', 'selected').ok).toBe(true)
    expect(engine.toggle('a3', 'selected')).toEqual({ ok: false, reason: 'limit' })
    expect(engine.decisionOf('a3')).toBe('none')
    expect(engine.optimisticCounts().selected).toBe(2)
    engine.dispose()
  })

  it('adopts the server version and replays once after a conflict', async () => {
    const { api, apply, refresh } = harness()
    apply
      .mockRejectedValueOnce(new ApiError(409, 'version_conflict', 'conflict'))
      .mockResolvedValueOnce(result(3, 1))
    refresh.mockResolvedValue(album({ selection_version: 2 }))
    const engine = new SelectionEngine(api, album())

    engine.setDecision('a1', 'selected')
    await vi.advanceTimersByTimeAsync(450)

    expect(refresh).toHaveBeenCalledTimes(1)
    expect(apply).toHaveBeenCalledTimes(2)
    expect(apply.mock.calls[1][0].expected_version).toBe(2)
    expect(engine.state.version).toBe(3)
    expect(engine.state.conflict).toBe(false)
    expect(engine.state.lastError).toBe('')
    expect(engine.state.pending).toBe(0)
    engine.dispose()
  })

  it('surfaces a second conflict and keeps the changes queued', async () => {
    const { api, apply, refresh } = harness()
    apply.mockRejectedValue(new ApiError(409, 'version_conflict', 'conflict'))
    refresh.mockResolvedValue(album({ selection_version: 5 }))
    const engine = new SelectionEngine(api, album())

    engine.setDecision('a1', 'selected')
    await vi.advanceTimersByTimeAsync(3000)

    expect(engine.state.conflict).toBe(true)
    expect(engine.state.lastError).toContain('其他设备')
    expect(engine.state.pending).toBe(1)
    expect(engine.decisionOf('a1')).toBe('selected')
    engine.dispose()
  })

  it('reverts decisions when the server rejects the batch as over the limit', async () => {
    const { api, apply, refresh } = harness()
    apply.mockRejectedValueOnce(new ApiError(409, 'selection_limit_exceeded'))
    refresh.mockResolvedValue(album({ selection_version: 7, selection_limit: 1, counts: { assets: 10, selected: 1, rejected: 0, undecided: 9 } }))
    const engine = new SelectionEngine(api, album())
    engine.hydrate([item('a1')])

    engine.setDecision('a1', 'selected')
    await vi.advanceTimersByTimeAsync(450)

    expect(engine.decisionOf('a1')).toBe('none')
    expect(engine.state.lastError).toContain('超出选片上限')
    expect(engine.state.version).toBe(7)
    engine.dispose()
  })

  it('retries transient failures and clears the error on success', async () => {
    const { api, apply } = harness()
    apply
      .mockRejectedValueOnce(new Error('network'))
      .mockRejectedValueOnce(new Error('network'))
      .mockResolvedValueOnce(result(1, 1))
    const engine = new SelectionEngine(api, album())

    engine.setDecision('a1', 'selected')
    await vi.advanceTimersByTimeAsync(3000)

    expect(apply).toHaveBeenCalledTimes(3)
    expect(engine.state.lastError).toBe('')
    expect(engine.state.pending).toBe(0)
    engine.dispose()
  })

  it('requeues changes after exhausting retries and supports a manual retry', async () => {
    const { api, apply } = harness()
    apply.mockRejectedValue(new Error('offline'))
    const engine = new SelectionEngine(api, album())

    engine.setDecision('a1', 'selected')
    await vi.advanceTimersByTimeAsync(5000)

    expect(engine.state.pending).toBe(1)
    expect(engine.state.lastError).toContain('网络异常')
    expect(engine.decisionOf('a1')).toBe('selected')

    apply.mockResolvedValueOnce(result(1, 1))
    engine.retry()
    await vi.advanceTimersByTimeAsync(10)

    expect(engine.state.pending).toBe(0)
    expect(engine.state.lastError).toBe('')
    expect(engine.state.version).toBe(1)
    engine.dispose()
  })

  it('reports terminal link failures instead of retrying', async () => {
    const { api, apply } = harness()
    const onFatal = vi.fn()
    apply.mockRejectedValue(new TerminalError('gallery_expired'))
    const engine = new SelectionEngine(api, album(), { onFatal })

    engine.setDecision('a1', 'selected')
    await vi.advanceTimersByTimeAsync(1000)

    expect(onFatal).toHaveBeenCalledTimes(1)
    expect(apply).toHaveBeenCalledTimes(1)
    expect(engine.state.pending).toBe(0)
    expect(engine.state.saving).toBe(false)
    engine.dispose()
  })

  it('marks the album submitted when another device already submitted', async () => {
    const { api, submit, refresh } = harness()
    submit.mockRejectedValue(new ApiError(409, 'already_submitted'))
    refresh.mockResolvedValue(
      album({ selection_version: 4, submitted_at: '2026-10-01T10:00:00Z' }),
    )
    const engine = new SelectionEngine(api, album())

    const returned = await engine.submit()

    expect(returned.submitted_at).toBe('2026-10-01T10:00:00Z')
    expect(engine.state.submitted).toBe(true)
    expect(engine.setDecision('a1', 'selected')).toEqual({ ok: false, reason: 'submitted' })
    engine.dispose()
  })

  it('permits full edits during the window and append-only selections after it', () => {
    const { api } = harness()
    const engine = new SelectionEngine(api, album({
      submitted_at: '2026-10-01T10:00:00Z',
      selection_edit_until: '2026-10-01T12:00:00Z',
    }))
    engine.hydrate([item('a1', 'selected')])

    expect(engine.canEdit('selected', 'none', Date.parse('2026-10-01T11:00:00Z'))).toBe(true)
    expect(engine.canEdit('selected', 'none', Date.parse('2026-10-01T12:00:00Z'))).toBe(false)
    expect(engine.canEdit('none', 'selected', Date.parse('2026-10-01T12:00:00Z'))).toBe(true)
    expect(engine.canEdit('none', 'rejected', Date.parse('2026-10-01T12:00:00Z'))).toBe(false)
    engine.dispose()
  })

  it('hydrates server decisions without overwriting local ones', () => {
    const { api } = harness()
    const engine = new SelectionEngine(api, album())

    engine.hydrate([item('a1', 'selected'), item('a2', 'rejected')])
    expect(engine.decisionOf('a1')).toBe('selected')
    expect(engine.decisionOf('a2')).toBe('rejected')

    engine.toggle('a1', 'selected')
    expect(engine.decisionOf('a1')).toBe('none')
    engine.hydrate([item('a1', 'selected')])
    expect(engine.decisionOf('a1')).toBe('none')
    engine.dispose()
  })
})
