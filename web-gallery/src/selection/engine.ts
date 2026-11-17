import { reactive } from 'vue'
import { isApiError } from '../api/client'
import type { Album, Counts, PageItem, SelectionResult } from '../api/types'
import { isTerminalError } from '../state/session'
import { requestKey } from '../utils/keys'

export type Decision = 'selected' | 'rejected' | 'none'

export interface SelectionApi {
  apply(
    body: { expected_version: number; items: { asset_id: string; decision: Decision }[] },
    idempotencyKey: string,
  ): Promise<SelectionResult>
  refresh(): Promise<Album>
  submit(expectedVersion: number, idempotencyKey: string): Promise<Album>
}

export interface SelectionOptions {
  /** Reports fatal link/gallery failures that no retry can fix. */
  onFatal?: (error: unknown) => void
}

export interface SelectionState {
  decisions: Record<string, Decision>
  /** Last acknowledged server decision per asset, used to revert on errors. */
  server: Record<string, Decision>
  counts: Counts
  version: number
  limit: number | null
  submitted: boolean
  editUntil: number | null
  saving: boolean
  pending: number
  conflict: boolean
  lastError: string
}

export type ToggleResult = { ok: true } | { ok: false; reason: 'limit' | 'submitted' }

interface Pair {
  asset_id: string
  decision: Decision
}

const BATCH_LIMIT = 200
const FLUSH_DELAY = 450
const MAX_ATTEMPTS = 3
const RETRY_BASE = 700

function sleep(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds))
}

function clamp(value: number, max: number): number {
  if (!Number.isFinite(value) || value < 0) return 0
  return Math.min(value, max)
}

// SelectionEngine keeps decisions optimistic, debounces writes, and resolves
// 409s deterministically: version conflicts replay once against the server's
// version, everything else reverts to the last acknowledged state.
export class SelectionEngine {
  readonly state = reactive<SelectionState>({
    decisions: {},
    server: {},
    counts: { assets: 0, selected: 0, rejected: 0, undecided: 0 },
    version: 0,
    limit: null,
    submitted: false,
    editUntil: null,
    saving: false,
    pending: 0,
    conflict: false,
    lastError: '',
  })

  private readonly pending = new Map<string, Decision>()
  private inflightPairs: Pair[] = []
  private inflight: Promise<void> | null = null
  private timer: ReturnType<typeof setTimeout> | null = null
  private disposed = false
  private fatal = false
  // paused stops the automatic retry loop after a batch failed for good; the
  // next user action or an explicit retry resumes flushing.
  private paused = false

  constructor(
    private readonly api: SelectionApi,
    album: Album,
    private readonly options: SelectionOptions = {},
  ) {
    this.applyAlbum(album)
  }

  decisionOf(assetID: string): Decision {
    return this.state.decisions[assetID] ?? 'none'
  }

  hydrate(items: PageItem[]): void {
    for (const item of items) {
      const decision: Decision = item.decision === 'selected' || item.decision === 'rejected' ? item.decision : 'none'
      if (this.state.server[item.asset_id] === undefined) this.state.server[item.asset_id] = decision
      if (this.state.decisions[item.asset_id] === undefined && decision !== 'none') {
        this.state.decisions[item.asset_id] = decision
      }
    }
  }

  // optimisticCounts adds unsaved changes on top of the acknowledged counts so
  // the header and filters never lag behind the thumbnails.
  optimisticCounts(): Counts {
    const overlay = new Map<string, Decision>()
    for (const [id, decision] of this.pending) overlay.set(id, decision)
    for (const pair of this.inflightPairs) {
      if (!overlay.has(pair.asset_id)) overlay.set(pair.asset_id, pair.decision)
    }
    let selected = this.state.counts.selected
    let rejected = this.state.counts.rejected
    for (const [id, decision] of overlay) {
      const before = this.state.server[id] ?? 'none'
      if (before === 'selected') selected--
      if (before === 'rejected') rejected--
      if (decision === 'selected') selected++
      if (decision === 'rejected') rejected++
    }
    const assets = this.state.counts.assets
    selected = clamp(selected, assets)
    rejected = clamp(rejected, assets)
    return {
      assets,
      selected,
      rejected,
      undecided: Math.max(0, assets - selected - rejected),
    }
  }

  setDecision(assetID: string, decision: Decision): ToggleResult {
    const current = this.decisionOf(assetID)
    if (!this.canEdit(current, decision)) return { ok: false, reason: 'submitted' }
    if (current === decision) return { ok: true }
    if (decision === 'selected' && current !== 'selected' && !this.withinLimit(assetID)) {
      return { ok: false, reason: 'limit' }
    }
    this.state.decisions[assetID] = decision
    this.pending.set(assetID, decision)
    this.syncPending()
    this.paused = false
    this.schedule(FLUSH_DELAY)
    return { ok: true }
  }

  toggle(assetID: string, kind: 'selected' | 'rejected'): ToggleResult {
    return this.setDecision(assetID, this.decisionOf(assetID) === kind ? 'none' : kind)
  }

  // flush sends the oldest pending batch. Callers may await it; the debounce
  // timer and retry button call it without awaiting.
  async flush(): Promise<void> {
    if (this.fatal || this.disposed) return
    if (this.inflight) return this.inflight
    if (this.pending.size === 0 || (this.state.submitted && !this.canEdit('none', 'selected')) || this.disposed) return
    const pairs: Pair[] = []
    for (const [assetID, decision] of this.pending) {
      pairs.push({ asset_id: assetID, decision })
      if (pairs.length >= BATCH_LIMIT) break
    }
    for (const pair of pairs) this.pending.delete(pair.asset_id)
    this.syncPending()
    this.inflightPairs = pairs
    const run = this.send(pairs).finally(() => {
      this.inflight = null
      this.inflightPairs = []
      if (!this.disposed && this.pending.size > 0) this.schedule(0)
    })
    this.inflight = run
    return run
  }

  retry(): void {
    this.state.lastError = ''
    this.state.conflict = false
    this.paused = false
    this.cancelTimer()
    void this.flush().catch(() => undefined)
  }

  async submit(): Promise<Album> {
    this.cancelTimer()
    await this.flush()
    if (this.pending.size > 0) throw new Error('仍有选片未保存，请稍后重试')
    if (this.state.submitted && !this.canEdit('none', 'selected') && this.pending.size === 0) throw new Error('选片已提交')
    try {
      const album = await this.api.submit(this.state.version, requestKey('submit'))
      this.applyAlbum(album)
      this.state.submitted = true
      return album
    } catch (error) {
      if (isApiError(error, 409, 'already_submitted')) {
        const album = await this.api.refresh().catch(() => null)
        if (album) this.applyAlbum(album)
        this.state.submitted = true
        if (album) return album
      }
      if (isApiError(error, 409, 'version_conflict')) {
        await this.adoptLatest().catch(() => undefined)
        throw new Error('选片刚在其他设备更新，请确认后重新提交')
      }
      throw error
    }
  }

  dispose(): void {
    this.disposed = true
    this.cancelTimer()
  }

  // adopt applies a server album snapshot fetched elsewhere (reopen refresh,
  // manual reload). Unsaved decisions are kept; only limits and submitted
  // state come from the snapshot.
  adopt(album: Album): void {
    this.applyAlbum(album)
  }

  private async send(pairs: Pair[]): Promise<void> {
    try {
      await this.deliver(pairs)
    } catch (error) {
      if (isTerminalError(error)) {
        // The link is dead; further requests would only add load to the API.
        this.fatal = true
        this.paused = true
        this.cancelTimer()
        this.pending.clear()
        this.inflightPairs = []
        this.syncPending()
        this.state.lastError = ''
        this.options.onFatal?.(error)
      }
      throw error
    }
  }

  private async deliver(pairs: Pair[]): Promise<void> {
    this.state.saving = true
    let attempt = 0
    try {
      for (;;) {
        try {
          const result = await this.api.apply(
            { expected_version: this.state.version, items: pairs },
            requestKey('sel'),
          )
          this.state.version = result.selection_version
          this.state.counts = result.counts
          for (const pair of pairs) this.state.server[pair.asset_id] = pair.decision
          this.state.conflict = false
          this.state.lastError = ''
          return
        } catch (error) {
          if (isTerminalError(error)) {
            this.requeue(pairs)
            throw error
          }
          if (isApiError(error, 409, 'version_conflict')) {
            await this.adoptLatest()
            if (attempt === 0) {
              attempt++
              continue
            }
            this.requeue(pairs)
            this.state.conflict = true
            this.state.lastError = '选片已在其他设备更新，请刷新页面后重试'
            this.pause()
            throw error
          }
          if (isApiError(error, 409, 'selection_limit_exceeded')) {
            this.revert(pairs)
            await this.adoptLatest().catch(() => undefined)
            this.state.lastError = '超出选片上限，已撤销本次操作'
            this.pause()
            throw error
          }
          if (isApiError(error, 409, 'already_submitted')) {
            this.state.submitted = true
            await this.adoptLatest().catch(() => undefined)
            this.state.lastError = '选片已提交，不能继续修改'
            this.pause()
            throw error
          }
          attempt++
          if (attempt < MAX_ATTEMPTS) {
            await sleep(RETRY_BASE * attempt)
            continue
          }
          this.requeue(pairs)
          this.state.lastError = '网络异常，选片暂未保存，请点击重试'
          this.pause()
          throw error
        }
      }
    } finally {
      this.state.saving = false
      this.syncPending()
    }
  }

  private withinLimit(assetID: string): boolean {
    if (this.state.limit === null) return true
    const counts = this.optimisticCounts()
    const projected = counts.selected + (this.decisionOf(assetID) === 'selected' ? 0 : 1)
    return projected <= this.state.limit
  }

  private revert(pairs: Pair[]): void {
    for (const pair of pairs) {
      if (this.pending.has(pair.asset_id)) continue
      const server = this.state.server[pair.asset_id] ?? 'none'
      if (server === 'none') delete this.state.decisions[pair.asset_id]
      else this.state.decisions[pair.asset_id] = server
    }
  }

  private requeue(pairs: Pair[]): void {
    for (const pair of pairs) {
      if (!this.pending.has(pair.asset_id)) this.pending.set(pair.asset_id, pair.decision)
    }
    this.syncPending()
  }

  private async adoptLatest(): Promise<void> {
    const album = await this.api.refresh()
    this.applyAlbum(album)
  }

  private applyAlbum(album: Album): void {
    this.state.version = album.selection_version
    this.state.counts = album.counts
    this.state.limit = album.selection_limit
    this.state.submitted = Boolean(album.submitted_at)
    this.state.editUntil = album.selection_edit_until ? Date.parse(album.selection_edit_until) : null
  }

  canEdit(current: Decision, next: Decision, now = Date.now()): boolean {
    if (!this.state.submitted) return true
    if (this.state.editUntil === null) return false
    if (now < this.state.editUntil) return true
    return next === 'selected' && current !== 'selected'
  }

  private syncPending(): void {
    this.state.pending = this.pending.size
  }

  private pause(): void {
    this.paused = true
    this.cancelTimer()
  }

  private schedule(delay: number): void {
    if (this.disposed || this.fatal || this.paused) return
    this.cancelTimer()
    this.timer = setTimeout(() => {
      this.timer = null
      void this.flush().catch(() => undefined)
    }, delay)
  }

  private cancelTimer(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer)
      this.timer = null
    }
  }
}
