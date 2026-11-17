import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api/client'
import type { ProofComment, ProofCommentInput } from '../api/proof'
import { buildThreads, commentCounts, CommentStore, type CommentApi } from './comments'

function comment(overrides: Partial<ProofComment> = {}): ProofComment {
  return {
    id: 'c1',
    proof_round_id: 'round-1',
    asset_id: 'asset-1',
    author_type: 'client',
    body: '',
    created_at: '2026-01-01T10:00:00Z',
    updated_at: '2026-01-01T10:00:00Z',
    ...overrides,
  }
}

function fakeApi(overrides: Partial<CommentApi> = {}): CommentApi {
  return {
    list: vi.fn(async () => []),
    create: vi.fn(async (input: ProofCommentInput) => comment({ body: input.body })),
    resolve: vi.fn(async () => comment({ resolved_at: '2026-01-01T11:00:00Z' })),
    ...overrides,
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

const input: ProofCommentInput = { asset_id: 'asset-1', body: '这里需要修一下' }

describe('buildThreads', () => {
  it('nests replies and lists open threads before resolved ones', () => {
    const items = [
      comment({ id: 'resolved', resolved_at: '2026-01-01T12:00:00Z' }),
      comment({ id: 'open-b', created_at: '2026-01-01T10:05:00Z' }),
      comment({ id: 'open-a', created_at: '2026-01-01T10:01:00Z' }),
      comment({ id: 'reply-late', parent_id: 'open-a', created_at: '2026-01-01T10:09:00Z' }),
      comment({ id: 'reply-early', parent_id: 'open-a', created_at: '2026-01-01T10:07:00Z' }),
    ]
    const threads = buildThreads(items, 'asset-1')
    expect(threads.map((thread) => thread.root.id)).toEqual(['open-a', 'open-b', 'resolved'])
    expect(threads[0].replies.map((reply) => reply.id)).toEqual(['reply-early', 'reply-late'])
    expect(threads[2].resolved).toBe(true)
  })

  it('filters by asset', () => {
    const threads = buildThreads(
      [comment({ id: 'a' }), comment({ id: 'b', asset_id: 'asset-2' })],
      'asset-2',
    )
    expect(threads.map((thread) => thread.root.id)).toEqual(['b'])
  })
})

describe('commentCounts', () => {
  it('counts totals and unresolved notes per asset', () => {
    const counts = commentCounts([
      comment({ id: '1' }),
      comment({ id: '2', resolved_at: '2026-01-01T12:00:00Z' }),
      comment({ id: '3', asset_id: 'asset-2' }),
    ])
    expect(counts['asset-1']).toEqual({ total: 2, open: 1 })
    expect(counts['asset-2']).toEqual({ total: 1, open: 1 })
  })
})

describe('CommentStore', () => {
  it('inserts an optimistic comment and replaces it with the server row', async () => {
    const pending = deferred<ProofComment>()
    const created = comment({ id: 'server-1', body: input.body })
    const create = vi.fn(() => pending.promise)
    const store = new CommentStore(fakeApi({ create }))

    const promise = store.create(input, 'key-1')
    expect(store.state.items).toHaveLength(1)
    expect(store.state.items[0].id).toBe('tmp:key-1')
    expect(store.state.saving).toBe(1)

    pending.resolve(created)
    await promise
    expect(store.state.items).toHaveLength(1)
    expect(store.state.items[0].id).toBe('server-1')
    expect(store.state.saving).toBe(0)
    expect(create).toHaveBeenCalledWith(input, 'key-1')
  })

  it('rolls back the optimistic comment when the request fails', async () => {
    const create = vi.fn(async () => {
      throw new ApiError(422, 'invalid_annotation', 'bad shape')
    })
    const store = new CommentStore(fakeApi({ create }))
    await expect(store.create(input, 'key-2')).rejects.toMatchObject({
      code: 'invalid_annotation',
    })
    expect(store.state.items).toHaveLength(0)
  })

  it('retries a failed attempt with the same idempotency key', async () => {
    const create = vi
      .fn<CommentApi['create']>()
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValueOnce(comment({ id: 'server-3' }))
    const store = new CommentStore(fakeApi({ create }))

    await expect(store.create(input, 'key-3')).rejects.toThrow('offline')
    await store.create(input, 'key-3')

    expect(create).toHaveBeenCalledTimes(2)
    expect(create.mock.calls[1][1]).toBe('key-3')
    expect(store.state.items.map((entry) => entry.id)).toEqual(['server-3'])
  })

  it('resolves a thread locally and mirrors the cascade to its replies', async () => {
    const root = comment({ id: 'root' })
    const reply = comment({ id: 'reply', parent_id: 'root' })
    const resolved = comment({ id: 'root', resolved_at: '2026-01-01T11:00:00Z' })
    const api = fakeApi({
      list: vi.fn(async () => [root, reply]),
      resolve: vi.fn(async () => resolved),
    })
    const store = new CommentStore(api)
    await store.load()

    await store.resolve('root')
    expect(store.state.items.find((entry) => entry.id === 'root')?.resolved_at).toBeTruthy()
    expect(store.state.items.find((entry) => entry.id === 'reply')?.resolved_at).toBeTruthy()
  })

  it('reverts the optimistic resolve on failure', async () => {
    const root = comment({ id: 'root' })
    const api = fakeApi({
      list: vi.fn(async () => [root]),
      resolve: vi.fn(async () => {
        throw new Error('offline')
      }),
    })
    const store = new CommentStore(api)
    await store.load()

    await expect(store.resolve('root')).rejects.toThrow('offline')
    expect(store.state.items[0].resolved_at).toBeUndefined()
  })

  it('is a no-op when resolving an already resolved thread', async () => {
    const root = comment({ id: 'root', resolved_at: '2026-01-01T11:00:00Z' })
    const api = fakeApi({ list: vi.fn(async () => [root]) })
    const store = new CommentStore(api)
    await store.load()

    await store.resolve('root')
    expect(api.resolve).not.toHaveBeenCalled()
  })
})
