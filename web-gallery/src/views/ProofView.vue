<script setup lang="ts">
import {
  computed,
  nextTick,
  onBeforeUnmount,
  onMounted,
  provide,
  reactive,
  ref,
  shallowRef,
  watch,
} from 'vue'
import { ApiError, isApiError } from '../api/client'
import {
  createProofComment,
  fetchProofComments,
  fetchProofManifest,
  resolveProofComment,
  submitProofRound,
  type ProofItem,
  type ProofRound,
} from '../api/proof'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import ProofCard from '../components/ProofCard.vue'
import ProofLightbox from '../components/ProofLightbox.vue'
import ProofTerminal from '../components/ProofTerminal.vue'
import { CommentStore, type CommentCount } from '../proof/comments'
import { proofCapabilities, proofStatusLabel, proofStatusTone } from '../proof/status'
import { submitWithReplay } from '../proof/submit'
import {
  isProofTerminalError,
  ProofSession,
  type ProofTerminalCode,
} from '../state/proof'
import { countdown } from '../utils/countdown'
import { createLazyContext, LazyKey } from '../utils/lazy'
import { requestKey } from '../utils/keys'

const props = defineProps<{ token: string }>()

type Phase = 'loading' | 'ready' | 'approved' | 'terminal' | 'error'
type ToastKind = 'info' | 'success' | 'error'

const PAGE_SIZE = 60

const phase = ref<Phase>('loading')
const terminalCode = ref<ProofTerminalCode>('invalid_token')
const bootError = ref('')

const session = shallowRef<ProofSession | null>(null)
const round = ref<ProofRound | null>(null)
const commentStore = shallowRef<CommentStore | null>(null)

const items = ref<ProofItem[]>([])
const total = ref(0)
const nextCursor = ref('')
const manifestDone = ref(false)
const loadingMore = ref(false)
const manifestError = ref('')
const refreshing = ref(false)
const loadedPages = ref(0)
let refreshingImages = false

const now = ref(Date.now())
const submitting = ref(false)
const submitDecision = ref<'approved' | 'changes_requested' | null>(null)
const lightboxIndex = ref<number | null>(null)

const toastState = reactive({ message: '', kind: 'info' as ToastKind, visible: false })
let toastTimer: number | undefined
let clock: number | undefined
let sentinelObserver: IntersectionObserver | null = null

const lazy = createLazyContext(() => null)
provide(LazyKey, lazy)

const toastMessage = (message: string, kind: ToastKind = 'info') => {
  toastState.message = message
  toastState.kind = kind
  toastState.visible = true
  if (toastTimer !== undefined) window.clearTimeout(toastTimer)
  toastTimer = window.setTimeout(() => {
    toastState.visible = false
    toastTimer = undefined
  }, 3600)
}

onMounted(() => {
  void boot()
})

// Hash navigation inside the SPA keeps the component alive, so opening a
// different proof link in the same tab must reboot instead of showing the
// previous link's terminal state.
watch(
  () => props.token,
  (next, prev) => {
    if (next !== prev) void boot()
  },
)

onBeforeUnmount(() => {
  stopClock()
  detachObserver()
  document.body.style.overflow = ''
  if (toastTimer !== undefined) window.clearTimeout(toastTimer)
})

async function boot() {
  stopClock()
  detachObserver()
  phase.value = 'loading'
  bootError.value = ''
  manifestError.value = ''
  items.value = []
  total.value = 0
  nextCursor.value = ''
  manifestDone.value = false
  loadedPages.value = 0
  lightboxIndex.value = null
  submitDecision.value = null

  const current = new ProofSession()
  session.value = current
  try {
    const opened = await current.open(props.token)
    round.value = opened
    if (opened.status === 'approved') {
      phase.value = 'approved'
      return
    }
    commentStore.value = new CommentStore({
      list: () =>
        current.withSession((jwt) =>
          fetchProofComments(jwt, opened.id, { includeResolved: true }),
        ),
      create: (input, key) =>
        current.withSession((jwt) => createProofComment(jwt, opened.id, input, key)),
      resolve: (id) =>
        current.withSession((jwt) => resolveProofComment(jwt, opened.id, id)),
    })
    await loadNext()
    if (round.value?.status === 'approved') return
    phase.value = 'ready'
    startClock()
    await nextTick()
    observeSentinel()
    void maybeLoadMore()
    void loadComments()
  } catch (error) {
    if (isProofTerminalError(error)) {
      fail(error.code)
      return
    }
    bootError.value = describe(error)
    phase.value = 'error'
  }
}

function handleFatal(error: unknown) {
  if (isProofTerminalError(error)) fail(error.code)
}

function fail(code: ProofTerminalCode) {
  stopClock()
  detachObserver()
  submitDecision.value = null
  lightboxIndex.value = null
  terminalCode.value = code
  phase.value = 'terminal'
  document.body.style.overflow = ''
}

function describe(error: unknown): string {
  if (error instanceof ApiError) return error.detail || error.code
  if (error instanceof Error) return error.message
  return '网络异常，请稍后重试'
}

/* ---------- clock ---------- */

const deadline = computed(() => {
  const value = round.value?.due_at
  return value ? Date.parse(value) : null
})

const remaining = computed(() =>
  deadline.value === null ? null : countdown(deadline.value, now.value),
)

const deadlineText = computed(() => {
  const value = remaining.value
  if (!value) return ''
  return value.expired ? '已超过审片截止时间' : `距审片截止 ${value.text}`
})

function startClock() {
  stopClock()
  now.value = Date.now()
  clock = window.setInterval(() => {
    now.value = Date.now()
  }, 1000)
}

function stopClock() {
  if (clock !== undefined) {
    window.clearInterval(clock)
    clock = undefined
  }
}

/* ---------- manifest ---------- */

async function loadNext(): Promise<void> {
  const current = session.value
  const active = round.value
  if (!current || !active || loadingMore.value || manifestDone.value) return
  loadingMore.value = true
  manifestError.value = ''
  try {
    const page = await current.withSession((jwt) =>
      fetchProofManifest(jwt, active.id, nextCursor.value, PAGE_SIZE),
    )
    loadedPages.value += 1
    total.value = page.total
    const seen = new Set(items.value.map((item) => item.asset_id))
    items.value.push(...page.items.filter((item) => !seen.has(item.asset_id)))
    applyRound(page.round)
    if (page.next_cursor) {
      nextCursor.value = page.next_cursor
    } else {
      nextCursor.value = ''
      manifestDone.value = true
    }
  } catch (error) {
    if (isProofTerminalError(error)) {
      fail(error.code)
      return
    }
    manifestError.value = describe(error)
  } finally {
    loadingMore.value = false
  }
}

// maybeLoadMore keeps fetching pages while the end of the grid is near the
// viewport, so short albums fill the screen without user scrolling.
async function maybeLoadMore() {
  for (let guard = 0; guard < 20; guard++) {
    if (phase.value !== 'ready' || manifestDone.value || loadingMore.value) return
    const before = items.value.length
    await loadNext()
    if (phase.value !== 'ready') return
    if (items.value.length === before) return
    await nextTick()
    const sentinel = sentinelRef.value
    if (!sentinel) return
    if (sentinel.getBoundingClientRect().top > window.innerHeight + 600) return
  }
}

function observeSentinel() {
  detachObserver()
  if (!sentinelRef.value || typeof IntersectionObserver === 'undefined') return
  sentinelObserver = new IntersectionObserver(
    (entries) => {
      if (entries.some((entry) => entry.isIntersecting)) void maybeLoadMore()
    },
    { rootMargin: '600px' },
  )
  sentinelObserver.observe(sentinelRef.value)
}

function detachObserver() {
  sentinelObserver?.disconnect()
  sentinelObserver = null
}

const sentinelRef = ref<HTMLElement | null>(null)

// refreshImages re-signs every loaded page after a presigned URL expires. The
// round snapshot is immutable, so the same page count is fetched again.
async function refreshImages() {
  const current = session.value
  const active = round.value
  if (!current || !active || refreshingImages) return
  refreshingImages = true
  try {
    const fresh: ProofItem[] = []
    let cursor = ''
    let done = false
    for (let page = 0; page < loadedPages.value; page++) {
      const result = await current.withSession((jwt) =>
        fetchProofManifest(jwt, active.id, cursor, PAGE_SIZE),
      )
      fresh.push(...result.items)
      cursor = result.next_cursor ?? ''
      done = !result.next_cursor
      if (done) break
    }
    items.value = fresh
    nextCursor.value = cursor
    manifestDone.value = done
    if (lightboxIndex.value !== null && lightboxIndex.value >= items.value.length) {
      lightboxIndex.value = null
    }
    await nextTick()
    observeSentinel()
  } catch (error) {
    if (isProofTerminalError(error)) fail(error.code)
  } finally {
    refreshingImages = false
  }
}

async function refreshAll() {
  const current = session.value
  if (!current || refreshing.value) return
  refreshing.value = true
  try {
    const fresh = await current.refreshRound()
    round.value = fresh.round
    if (fresh.round.status === 'approved') {
      phase.value = 'approved'
      stopClock()
      return
    }
    items.value = []
    nextCursor.value = ''
    manifestDone.value = false
    loadedPages.value = 0
    await loadNext()
    await nextTick()
    observeSentinel()
    await maybeLoadMore()
    void loadComments()
    toastMessage('已刷新', 'info')
  } catch (error) {
    if (isProofTerminalError(error)) {
      fail(error.code)
      return
    }
    toastMessage(describe(error), 'error')
  } finally {
    refreshing.value = false
  }
}

function applyRound(next: Partial<ProofRound>) {
  if (!round.value) return
  round.value = {
    ...round.value,
    status: next.status ?? round.value.status,
    due_at: next.due_at ?? round.value.due_at,
    version: next.version ?? round.value.version,
    project_title: next.project_title ?? round.value.project_title,
    gallery_title: next.gallery_title ?? round.value.gallery_title,
  }
  if (round.value.status === 'approved' && phase.value === 'ready') {
    lightboxIndex.value = null
    submitDecision.value = null
    phase.value = 'approved'
    stopClock()
  }
}

async function loadComments() {
  const store = commentStore.value
  if (!store) return
  try {
    await store.load()
  } catch (error) {
    if (isProofTerminalError(error)) fail(error.code)
  }
}

/* ---------- derived ---------- */

const capabilities = computed(() =>
  proofCapabilities(round.value?.status ?? 'in_review'),
)

const statusLabel = computed(() => proofStatusLabel(round.value?.status ?? 'in_review'))
const statusTone = computed(() => proofStatusTone(round.value?.status ?? 'in_review'))

const urgent = computed(
  () => Boolean(remaining.value && !remaining.value.expired && remaining.value.days === 0),
)

const commentCounts = computed<Record<string, CommentCount>>(
  () => commentStore.value?.counts() ?? {},
)
const openCommentCount = computed(
  () => commentStore.value?.state.items.filter((entry) => !entry.resolved_at).length ?? 0,
)

const showActions = computed(
  () => capabilities.value.canApprove || capabilities.value.canRequestChanges,
)

function countFor(item: ProofItem): CommentCount {
  return commentCounts.value[item.asset_id] ?? { total: 0, open: 0 }
}

const confirmMessage = computed(() => {
  if (submitDecision.value === 'approved') {
    return '确认本轮审片通过？通过后批注与照片将锁定。'
  }
  return `将把当前批注作为修改意见提交（待处理 ${openCommentCount.value} 条）。摄影师回传后可继续复核或再次提出修改。`
})

const confirmTitle = computed(() =>
  submitDecision.value === 'approved' ? '确认通过' : '提出修改',
)

const confirmText = computed(() =>
  submitDecision.value === 'approved' ? '确认通过' : '提交修改',
)

watch(lightboxIndex, (value) => {
  document.body.style.overflow = value === null ? '' : 'hidden'
})

function openLightbox(item: ProofItem) {
  const index = items.value.findIndex((entry) => entry.asset_id === item.asset_id)
  if (index >= 0) lightboxIndex.value = index
}

/* ---------- submit ---------- */

async function doSubmit() {
  const decision = submitDecision.value
  const current = session.value
  const active = round.value
  if (!decision || !current || !active || submitting.value) return
  submitting.value = true
  try {
    const result = await submitWithReplay(
      {
        submit: (next, version) =>
          current.withSession((jwt) =>
            submitProofRound(jwt, active.id, next, version, requestKey('proof-submit')),
          ),
        refresh: async () => {
          const fresh = await current.refreshRound()
          applyRound(fresh.round)
          return fresh.round
        },
      },
      decision,
      active.version,
    )
    applyRound(result)
    submitDecision.value = null
    if (result.status === 'approved') {
      if (decision === 'changes_requested') {
        toastMessage('该轮审片已通过，无需再提交修改意见', 'info')
      }
      phase.value = 'approved'
      stopClock()
    } else {
      toastMessage('修改意见已提交，等待工作室修图', 'success')
    }
  } catch (error) {
    submitDecision.value = null
    if (isProofTerminalError(error)) {
      fail(error.code)
      return
    }
    if (isApiError(error, 409, 'invalid_state')) {
      toastMessage(error.detail || '当前状态暂不能提交', 'error')
    } else if (isApiError(error, 409, 'version_conflict')) {
      toastMessage('审片状态已在其他设备更新，请刷新后重试', 'error')
    } else {
      toastMessage(describe(error), 'error')
    }
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <ProofTerminal v-if="phase === 'terminal'" :code="terminalCode" />

  <div v-else-if="phase === 'loading'" class="page">
    <div class="centered-page">
      <div class="loading-state">
        <span class="spinner" />
        <p>正在打开审片…</p>
      </div>
    </div>
  </div>

  <div v-else-if="phase === 'error'" class="page">
    <div class="centered-page">
      <div class="state-card">
        <div class="state-mark">ERROR</div>
        <h1>审片加载失败</h1>
        <p>{{ bootError }}</p>
        <button type="button" class="btn btn-primary" @click="boot">重新加载</button>
      </div>
    </div>
    <footer class="page-footer">如需帮助，请联系为您服务的摄影师或工作室。</footer>
  </div>

  <div v-else-if="phase === 'approved'" class="page">
    <div class="centered-page">
      <div class="state-card">
        <div class="state-mark">✓</div>
        <h1>本轮审片已通过</h1>
        <p v-if="round">
          {{ round.gallery_title || round.project_title }} · 第 {{ round.round_no }} 轮
        </p>
        <p>感谢您的确认，精修流程已完成。</p>
      </div>
    </div>
    <footer class="page-footer">如需帮助，请联系为您服务的摄影师或工作室。</footer>
  </div>

  <div v-else class="page">
    <header class="topbar">
      <div class="page-inner topbar-inner">
        <div class="topbar-row">
          <div>
            <p class="brand-mark">PWF <strong>PROOF</strong></p>
            <h1 class="album-title">
              {{ round?.gallery_title || round?.project_title || '审片' }}
            </h1>
            <p class="album-sub">
              <span class="proof-status" :class="`is-${statusTone}`">{{ statusLabel }}</span>
              <span
                v-if="remaining"
                class="countdown"
                :class="{ 'is-urgent': urgent, 'is-expired': remaining.expired }"
              >
                {{ deadlineText }}
              </span>
              <span>第 {{ round?.round_no ?? 1 }} 轮 · {{ total || items.length }} 张</span>
              <span v-if="openCommentCount > 0">待处理批注 {{ openCommentCount }} 条</span>
            </p>
          </div>
          <div class="header-actions">
            <button type="button" class="btn btn-ghost" :disabled="refreshing" @click="refreshAll">
              {{ refreshing ? '刷新中…' : '刷新' }}
            </button>
          </div>
        </div>
        <p class="proof-hint">{{ capabilities.hint }}</p>
      </div>
    </header>

    <div class="page-inner">
      <div v-if="remaining?.expired" class="notice is-warn">
        <span>已超过约定的审片截止时间，仍可提交；如需延期或说明，请联系工作室。</span>
      </div>

      <div v-if="commentStore?.state.error" class="notice is-error">
        <span>{{ commentStore.state.error }}</span>
        <div class="notice-actions">
          <button type="button" class="btn btn-ghost" @click="loadComments">重试</button>
        </div>
      </div>

      <div v-if="manifestError" class="notice is-error">
        <span>{{ manifestError }}</span>
        <div class="notice-actions">
          <button type="button" class="btn btn-ghost" @click="maybeLoadMore">重新加载</button>
        </div>
      </div>

      <div v-if="!items.length && !loadingMore" class="grid-empty">
        {{ manifestError ? '照片加载失败，请重试。' : '本轮暂无可审阅的照片。' }}
      </div>

      <section v-else class="proof-grid" aria-label="审片照片列表">
        <ProofCard
          v-for="item in items"
          :key="item.asset_id"
          :item="item"
          :count="countFor(item)"
          @open="openLightbox(item)"
          @stale="refreshImages"
        />
      </section>

      <div ref="sentinelRef" class="proof-sentinel">
        <template v-if="loadingMore">
          <span class="spinner" />
          <span>加载更多照片…</span>
        </template>
      </div>

      <div v-if="showActions" class="proof-actions-spacer" />
    </div>

    <footer class="page-footer">
      <p class="brand-mark">PWF <strong>PROOF</strong></p>
      <p>照片预览带有工作室水印，仅供审片参考，不提供下载。</p>
      <p>摄影师回传后可继续复核或提出修改，直到确认通过。</p>
    </footer>

    <div v-if="showActions" class="proof-actions">
      <button
        v-if="capabilities.canRequestChanges"
        type="button"
        class="btn btn-ghost"
        :disabled="submitting"
        @click="submitDecision = 'changes_requested'"
      >
        提出修改
      </button>
      <button
        v-if="capabilities.canApprove"
        type="button"
        class="btn btn-primary"
        :disabled="submitting"
        @click="submitDecision = 'approved'"
      >
        确认通过
      </button>
    </div>

    <ProofLightbox
      v-if="lightboxIndex !== null && items[lightboxIndex] && commentStore"
      :items="items"
      :index="lightboxIndex"
      :comments="commentStore"
      :can-comment="capabilities.canComment"
      @update:index="lightboxIndex = $event"
      @close="lightboxIndex = null"
      @stale="refreshImages"
      @fatal="handleFatal"
      @notify="toastMessage"
    />

    <ConfirmDialog
      :open="submitDecision !== null"
      :title="confirmTitle"
      :message="confirmMessage"
      :confirm-text="confirmText"
      :busy="submitting"
      @confirm="doSubmit"
      @cancel="submitDecision = null"
    />

    <Transition name="toast">
      <div v-if="toastState.visible" class="toast" :class="`is-${toastState.kind}`" role="status">
        {{ toastState.message }}
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.proof-status {
  display: inline-flex;
  align-items: center;
  padding: 3px 10px;
  border-radius: 999px;
  border: 1px solid var(--line);
  background: var(--bg-soft);
  font-size: 12px;
  color: var(--muted);
  white-space: nowrap;
}

.proof-status.is-progress {
  border-color: rgb(148 97 40 / 30%);
  color: var(--accent-strong);
}

.proof-status.is-waiting {
  border-color: rgb(49 91 87 / 25%);
  color: #315b57;
}

.proof-status.is-done {
  border-color: rgb(52 117 95 / 28%);
  color: #28634b;
}

.proof-hint {
  margin: 0;
  color: var(--muted);
  font-size: 12.5px;
  line-height: 1.6;
}

.proof-grid {
  margin-top: 10px;
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: var(--gap);
}

@media (min-width: 560px) {
  .proof-grid {
    grid-template-columns: repeat(4, minmax(0, 1fr));
  }
}

@media (min-width: 900px) {
  .proof-grid {
    grid-template-columns: repeat(6, minmax(0, 1fr));
  }
}

@media (min-width: 1180px) {
  .proof-grid {
    grid-template-columns: repeat(8, minmax(0, 1fr));
  }
}

.proof-sentinel {
  min-height: 54px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: var(--muted);
  font-size: 12.5px;
}

.proof-actions {
  position: fixed;
  left: 0;
  right: 0;
  bottom: 0;
  z-index: 40;
  display: flex;
  gap: 10px;
  padding: 12px 16px calc(12px + env(safe-area-inset-bottom));
  background: rgb(250 245 239 / 94%);
  backdrop-filter: blur(14px);
  border-top: 1px solid var(--line);
}

.proof-actions .btn {
  flex: 1;
  padding-top: 12px;
  padding-bottom: 12px;
  font-size: 14.5px;
}

.proof-actions-spacer {
  height: calc(84px + env(safe-area-inset-bottom));
}

@media (min-width: 720px) {
  .proof-actions {
    left: 50%;
    right: auto;
    transform: translateX(-50%);
    width: min(560px, calc(100% - 48px));
    bottom: 16px;
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 10px 12px;
  }

  .proof-actions-spacer {
    height: 96px;
  }
}
</style>
