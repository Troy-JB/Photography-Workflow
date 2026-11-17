import { reactive } from 'vue'
import type { ProofComment, ProofCommentInput } from '../api/proof'
import { mapProofError } from '../state/proof'
import { requestKey } from '../utils/keys'

export interface CommentApi {
  list(): Promise<ProofComment[]>
  create(input: ProofCommentInput, idempotencyKey: string): Promise<ProofComment>
  resolve(id: string): Promise<ProofComment>
}

export interface CommentStoreState {
  items: ProofComment[]
  loading: boolean
  loaded: boolean
  error: string
  saving: number
}

export interface CommentThread {
  root: ProofComment
  replies: ProofComment[]
  resolved: boolean
}

export interface CommentCount {
  total: number
  open: number
}

export function buildThreads(items: ProofComment[], assetID?: string): CommentThread[] {
  const roots = items
    .filter((entry) => !entry.parent_id && (!assetID || entry.asset_id === assetID))
    .sort((a, b) => {
      const openA = a.resolved_at ? 1 : 0
      const openB = b.resolved_at ? 1 : 0
      if (openA !== openB) return openA - openB
      return Date.parse(a.created_at) - Date.parse(b.created_at)
    })
  const replies = new Map<string, ProofComment[]>()
  for (const entry of items) {
    if (!entry.parent_id) continue
    const list = replies.get(entry.parent_id) ?? []
    list.push(entry)
    replies.set(entry.parent_id, list)
  }
  for (const list of replies.values()) {
    list.sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at))
  }
  return roots.map((root) => ({
    root,
    replies: replies.get(root.id) ?? [],
    resolved: Boolean(root.resolved_at),
  }))
}

export function commentCounts(items: ProofComment[]): Record<string, CommentCount> {
  const out: Record<string, CommentCount> = {}
  for (const entry of items) {
    const count = (out[entry.asset_id] ??= { total: 0, open: 0 })
    count.total += 1
    if (!entry.resolved_at) count.open += 1
  }
  return out
}

function nextKey(): string {
  return requestKey('proof-comment')
}

// CommentStore owns the round's comment thread for the whole visit so the grid
// badges and the lightbox panel stay in sync. Writes are optimistic and rolled
// back on failure; the retry keeps its idempotency key, so a request that did
// reach the server is replayed instead of duplicated.
export class CommentStore {
  readonly state = reactive<CommentStoreState>({
    items: [],
    loading: false,
    loaded: false,
    error: '',
    saving: 0,
  })

  constructor(private readonly api: CommentApi) {}

  async load(): Promise<void> {
    if (this.state.loading) return
    this.state.loading = true
    this.state.error = ''
    try {
      const items = await this.api.list()
      this.state.items = items
      this.state.loaded = true
    } catch (error) {
      const mapped = mapProofError(error)
      this.state.error = mapped instanceof Error ? mapped.message : '加载批注失败'
      throw mapped
    } finally {
      this.state.loading = false
    }
  }

  threadsFor(assetID: string): CommentThread[] {
    return buildThreads(this.state.items, assetID)
  }

  counts(): Record<string, CommentCount> {
    return commentCounts(this.state.items)
  }

  // create inserts a placeholder immediately. The composer keeps the returned
  // key so a failed attempt can retry with the exact same idempotency key.
  async create(input: ProofCommentInput, key = nextKey()): Promise<ProofComment> {
    const tempID = `tmp:${key}`
    const now = new Date().toISOString()
    const placeholder: ProofComment = {
      id: tempID,
      proof_round_id: '',
      asset_id: input.asset_id,
      parent_id: input.parent_id ?? undefined,
      author_type: 'client',
      body: input.body.trim(),
      annotation: input.annotation ?? undefined,
      created_at: now,
      updated_at: now,
    }
    this.state.items.push(placeholder)
    this.state.saving += 1
    try {
      const created = await this.api.create(input, key)
      this.replace(tempID, created)
      return created
    } catch (error) {
      this.remove(tempID)
      throw mapProofError(error)
    } finally {
      this.state.saving -= 1
    }
  }

  async resolve(id: string): Promise<void> {
    const root = this.state.items.find((entry) => entry.id === id)
    if (!root || root.resolved_at) return
    const resolvedAt = new Date().toISOString()
    const previous = root.resolved_at
    root.resolved_at = resolvedAt
    this.state.saving += 1
    try {
      const updated = await this.api.resolve(id)
      Object.assign(root, updated)
      // The server resolves a thread as a unit; mirror that locally so replies
      // do not linger as "open" until the next refresh.
      for (const reply of this.state.items) {
        if (reply.parent_id === id && !reply.resolved_at) reply.resolved_at = resolvedAt
      }
    } catch (error) {
      root.resolved_at = previous
      throw mapProofError(error)
    } finally {
      this.state.saving -= 1
    }
  }

  private replace(tempID: string, created: ProofComment): void {
    const index = this.state.items.findIndex((entry) => entry.id === tempID)
    if (index < 0) {
      if (!this.state.items.some((entry) => entry.id === created.id)) {
        this.state.items.push(created)
      }
      return
    }
    const duplicate = this.state.items.find(
      (entry, position) => position !== index && entry.id === created.id,
    )
    if (duplicate) {
      this.state.items.splice(index, 1)
      return
    }
    this.state.items[index] = created
  }

  private remove(id: string): void {
    const index = this.state.items.findIndex((entry) => entry.id === id)
    if (index >= 0) this.state.items.splice(index, 1)
  }
}
