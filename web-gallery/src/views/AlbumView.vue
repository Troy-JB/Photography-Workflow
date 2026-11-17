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
import AlbumHeader from '../components/AlbumHeader.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import Lightbox from '../components/Lightbox.vue'
import PhotoCard from '../components/PhotoCard.vue'
import TerminalPage from '../components/TerminalPage.vue'
import { ApiError } from '../api/client'
import { applySelections, submitAlbum } from '../api/gallery'
import type { Album, PageItem } from '../api/types'
import type { Filter } from '../state/filter'
import { matchesFilter } from '../state/filter'
import { AlbumFeed } from '../state/feed'
import { GallerySession, isTerminalError, type TerminalCode } from '../state/session'
import { SelectionEngine, type Decision, type SelectionApi } from '../selection/engine'
import { countdown } from '../utils/countdown'
import { createLazyContext, LazyKey } from '../utils/lazy'
import { virtualWindow } from '../utils/virtual'

const props = defineProps<{ token: string }>()

type Phase = 'loading' | 'ready' | 'terminal' | 'error'
type ToastKind = 'info' | 'success' | 'error'

const phase = ref<Phase>('loading')
const terminalCode = ref<TerminalCode>('invalid_token')
const terminalDetail = ref('')
const bootError = ref('')

const album = ref<Album | null>(null)
const session = shallowRef<GallerySession | null>(null)
const feed = shallowRef<AlbumFeed | null>(null)
const engine = shallowRef<SelectionEngine | null>(null)

const filter = ref<Filter>('all')
const now = ref(Date.now())
const submitting = ref(false)
const supplementalPaymentURL = ref('')
const confirmSubmit = ref(false)
const lightboxIndex = ref<number | null>(null)

const toastState = reactive({ message: '', kind: 'info' as ToastKind, visible: false })
let toastTimer: number | undefined
let clock: number | undefined

const GAP = 6

const lazy = createLazyContext(() => null)
provide(LazyKey, lazy)

onMounted(() => {
  void boot()
})

onBeforeUnmount(() => {
  stopClock()
  detachLayout()
  feed.value?.dispose()
  engine.value?.dispose()
  document.body.style.overflow = ''
  if (toastTimer !== undefined) window.clearTimeout(toastTimer)
})

async function boot() {
  phase.value = 'loading'
  bootError.value = ''
  const current = new GallerySession()
  session.value = current
  try {
    const view = await current.open(props.token)
    album.value = view
    const loader = new AlbumFeed(current, view.id)
    feed.value = loader
    const api: SelectionApi = {
      apply: (body, key) => current.withSession((jwt) => applySelections(jwt, body, key)),
      refresh: () => current.refreshAlbum(),
      submit: (version, key) => current.withSession((jwt) => submitAlbum(jwt, view.id, version, key)),
    }
    const selection = new SelectionEngine(api, view, { onFatal: handleFatal })
    engine.value = selection
    await loader.loadNext()
    selection.hydrate(loader.state.items)
    if (deadline.value !== null && now.value >= deadline.value) {
      fail('gallery_expired')
      return
    }
    phase.value = 'ready'
    startClock()
    await nextTick()
    attachLayout()
  } catch (error) {
    if (isTerminalError(error)) {
      fail(error.code, error.message)
      return
    }
    bootError.value = describe(error)
    phase.value = 'error'
  }
}

function handleFatal(error: unknown) {
  if (isTerminalError(error)) fail(error.code, error.message)
}

function fail(code: TerminalCode, detail = '') {
  confirmSubmit.value = false
  lightboxIndex.value = null
  feed.value?.dispose()
  engine.value?.dispose()
  terminalCode.value = code
  terminalDetail.value = detail
  phase.value = 'terminal'
  stopClock()
  detachLayout()
  document.body.style.overflow = ''
}

function describe(error: unknown): string {
  if (error instanceof ApiError) return error.detail || error.code
  if (error instanceof Error) return error.message
  return '网络异常，请稍后重试'
}

function toast(message: string, kind: ToastKind = 'info') {
  toastState.message = message
  toastState.kind = kind
  toastState.visible = true
  if (toastTimer !== undefined) window.clearTimeout(toastTimer)
  toastTimer = window.setTimeout(() => {
    toastState.visible = false
    toastTimer = undefined
  }, 3600)
}

/* ---------- clock ---------- */

const deadline = computed(() => {
  const value = album.value?.expires_at
  return value ? Date.parse(value) : null
})

const remaining = computed(() => (deadline.value === null ? null : countdown(deadline.value, now.value)))

function startClock() {
  stopClock()
  now.value = Date.now()
  clock = window.setInterval(() => {
    now.value = Date.now()
    if (phase.value === 'ready' && deadline.value !== null && now.value >= deadline.value) {
      fail('gallery_expired')
    }
    if (phase.value === 'ready' && editUntil.value !== null && now.value >= editUntil.value && !album.value?.selection_locked_at) void refreshStatus()
  }, 1000)
}

function stopClock() {
  if (clock !== undefined) {
    window.clearInterval(clock)
    clock = undefined
  }
}

/* ---------- data ---------- */

const items = computed(() => feed.value?.state.items ?? [])

const counts = computed(
  () =>
    engine.value?.optimisticCounts() ??
    album.value?.counts ?? { assets: 0, selected: 0, rejected: 0, undecided: 0 },
)

const filtered = computed<PageItem[]>(() => {
  const selection = engine.value
  if (filter.value === 'all' || !selection) return items.value
  return items.value.filter((item) => matchesFilter(selection.decisionOf(item.asset_id), filter.value))
})

const submittedAt = computed(() => album.value?.submitted_at ?? null)
const editUntil = computed(() => album.value?.selection_edit_until ? Date.parse(album.value.selection_edit_until) : null)
const editCountdown = computed(() => editUntil.value === null || submittedAt.value === null ? null : countdown(editUntil.value, now.value))
const editingWindowOpen = computed(() => editUntil.value !== null && now.value < editUntil.value)
const extraSelected = computed(() => Math.max(0, counts.value.selected - (album.value?.included_retouch_count ?? 0)))
const extraAmount = computed(() => extraSelected.value * (album.value?.extra_retouch_unit_price_cents ?? 0))
const readOnly = computed(() => false)
const decisions = computed(() => engine.value?.state.decisions ?? {})
const canSubmit = computed(() => {
  const selection = engine.value
  if (!selection) return false
  return !selection.state.saving && selection.state.pending === 0 && counts.value.selected > 0
})

function decisionOf(item: PageItem): Decision {
  return engine.value?.decisionOf(item.asset_id) ?? 'none'
}

function onFilterChange(next: Filter) {
  if (filter.value === next) return
  filter.value = next
  void nextTick(() => {
    window.scrollTo({ top: Math.max(0, layout.gridTop - 12), behavior: 'smooth' })
    measure()
  })
}

/* ---------- layout ---------- */

const gridRef = ref<HTMLElement | null>(null)
const layout = reactive({ width: 0, gridTop: 0, scrollTop: 0, viewport: 0 })
let resizeObserver: ResizeObserver | null = null
let frame = 0

const columns = computed(() => {
  const width = layout.width
  if (width >= 1180) return 6
  if (width >= 940) return 5
  if (width >= 640) return 4
  return 3
})

const rowHeight = computed(() => {
  const width = Math.max(0, layout.width - GAP * (columns.value - 1))
  return Math.max(1, Math.floor(width / columns.value))
})

const virtual = computed(() =>
  virtualWindow({
    count: filtered.value.length,
    columns: columns.value,
    rowHeight: rowHeight.value,
    gap: GAP,
    scrollTop: layout.scrollTop,
    viewportHeight: layout.viewport,
    overscan: 2,
  }),
)

const rows = computed<PageItem[][]>(() => {
  const list = filtered.value
  const stride = columns.value
  const out: PageItem[][] = []
  for (let row = virtual.value.startRow; row < virtual.value.endRow; row++) {
    out.push(list.slice(row * stride, row * stride + stride))
  }
  return out
})

const nearEnd = computed(() => {
  const loader = feed.value
  if (!loader || loader.state.done) return false
  const totalRows = Math.ceil(filtered.value.length / Math.max(1, columns.value))
  return virtual.value.endRow >= totalRows - 2
})

watch(nearEnd, (value) => {
  if (value) void loadMore()
})

watch(
  () => filtered.value.length,
  async () => {
    await nextTick()
    measure()
  },
)

// Banners above the grid shift its offset; re-measure whenever one appears or
// disappears so the virtual window keeps tracking the scroll position.
watch(
  () => [submittedAt.value, engine.value?.state.lastError ?? '', feed.value?.state.error ?? ''],
  async () => {
    await nextTick()
    measure()
  },
)

function measure() {
  const element = gridRef.value
  if (!element) return
  layout.width = element.clientWidth
  layout.gridTop = element.getBoundingClientRect().top + window.scrollY
  layout.viewport = window.innerHeight
  layout.scrollTop = Math.max(0, window.scrollY - layout.gridTop)
}

function onScroll() {
  if (frame) return
  frame = requestAnimationFrame(() => {
    frame = 0
    measure()
  })
}

function attachLayout() {
  measure()
  window.addEventListener('scroll', onScroll, { passive: true })
  window.addEventListener('resize', onScroll)
  if (typeof ResizeObserver !== 'undefined' && gridRef.value) {
    resizeObserver = new ResizeObserver(() => measure())
    resizeObserver.observe(gridRef.value)
  }
}

function detachLayout() {
  window.removeEventListener('scroll', onScroll)
  window.removeEventListener('resize', onScroll)
  resizeObserver?.disconnect()
  resizeObserver = null
  if (frame) {
    cancelAnimationFrame(frame)
    frame = 0
  }
}

/* ---------- loading ---------- */

async function loadMore() {
  const loader = feed.value
  if (!loader || loader.state.loading || loader.state.done) return
  try {
    await loader.loadNext()
    engine.value?.hydrate(loader.state.items)
    await nextTick()
    measure()
    if (nearEnd.value) void loadMore()
  } catch (error) {
    if (isTerminalError(error)) {
      fail(error.code, error.message)
      return
    }
    toast('加载照片失败，请稍后重试', 'error')
  }
}

/* ---------- selection ---------- */

function toggle(item: PageItem, decision: Exclude<Decision, 'none'>) {
  const selection = engine.value
  if (!selection) return
  const result = selection.toggle(item.asset_id, decision)
  if (result.ok) return
  if (result.reason === 'limit') {
    toast(`最多选择 ${selection.state.limit ?? 0} 张照片，请先取消已选的照片`, 'error')
  } else {
    toast(readOnly.value ? '调整时间已结束，只能追加新照片' : '选片提交后两小时内可以调整', 'error')
  }
}

function retrySave() {
  engine.value?.retry()
}

function onStale(item: PageItem | null) {
  if (item) void feed.value?.refreshPageOf(item.asset_id)
}

/* ---------- lightbox ---------- */

const lightboxItem = computed<PageItem | null>(() => {
  const index = lightboxIndex.value
  if (index === null) return null
  return filtered.value[index] ?? null
})

watch(lightboxIndex, (value) => {
  document.body.style.overflow = value === null ? '' : 'hidden'
})

function openLightbox(item: PageItem) {
  const index = filtered.value.findIndex((entry) => entry.asset_id === item.asset_id)
  if (index >= 0) lightboxIndex.value = index
}

/* ---------- submit ---------- */

async function doSubmit() {
  const selection = engine.value
  if (!selection) return
  submitting.value = true
  try {
    const result = await selection.submit()
    album.value = result
    if (session.value) session.value.album.value = result
    supplementalPaymentURL.value = result.payment_url ?? ''
    confirmSubmit.value = false
    await nextTick()
    // Instant reset: smooth scrolling races the banner insertion and can leave
    // the confirmation banner just above the viewport.
    window.scrollTo(0, 0)
    measure()
    toast('选片已提交，感谢您的配合', 'success')
  } catch (error) {
    if (isTerminalError(error)) {
      fail(error.code, error.message)
      return
    }
    toast(describe(error), 'error')
  } finally {
    submitting.value = false
  }
}

const confirmMessage = computed(() => {
  const value = counts.value
  const limit = engine.value?.state.limit
  const limitText = limit === null || limit === undefined ? '' : `（上限 ${limit} 张）`
  const feeText = extraAmount.value > 0 ? `，超出套餐 ${extraSelected.value} 张，补款 ¥${(extraAmount.value / 100).toFixed(2)}` : ''
  return `已选择 ${value.selected} 张${limitText}，未决定 ${value.undecided} 张${feeText}。首次提交后两小时内可调整；超时后只能追加。`
})

async function refreshStatus() {
  const current = session.value
  const selection = engine.value
  if (!current || !selection) return
  try {
    const next = await current.refreshAlbum()
    album.value = next
    selection.adopt(next)
    toast(next.submitted_at ? '状态已更新' : '工作室已重新开放选片，可以继续修改', next.submitted_at ? 'info' : 'success')
  } catch (error) {
    if (isTerminalError(error)) {
      fail(error.code, error.message)
      return
    }
    toast(describe(error), 'error')
  }
}

function formatTime(iso: string): string {
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(iso),
  )
}
</script>

<template>
  <TerminalPage v-if="phase === 'terminal'" :code="terminalCode" :detail="terminalDetail" />

  <div v-else-if="phase === 'loading'" class="page">
    <div class="centered-page">
      <div class="loading-state">
        <span class="spinner" />
        <p>正在打开相册…</p>
      </div>
    </div>
  </div>

  <div v-else-if="phase === 'error'" class="page">
    <div class="centered-page">
      <div class="state-card">
        <div class="state-mark">ERROR</div>
        <h1>相册加载失败</h1>
        <p>{{ bootError }}</p>
        <button type="button" class="btn btn-primary" @click="boot">重新加载</button>
      </div>
    </div>
    <footer class="page-footer">如需帮助，请联系为您服务的摄影师或工作室。</footer>
  </div>

  <div v-else class="page">
    <AlbumHeader
      :title="album?.title ?? ''"
      :submitted-at="submittedAt"
      :countdown="remaining"
      :limit="engine?.state.limit ?? null"
      :counts="counts"
      :saving="engine?.state.saving ?? false"
      :pending="engine?.state.pending ?? 0"
      :can-submit="canSubmit"
      :submitting="submitting"
      :filter="filter"
      :edit-countdown="editCountdown"
      :submitted="Boolean(submittedAt)"
      :extra-selected="extraSelected"
      :extra-amount-cents="extraAmount"
      :payment-url="supplementalPaymentURL"
      @submit="confirmSubmit = true"
      @filter-change="onFilterChange"
    />

    <div class="page-inner">
      <div v-if="submittedAt" class="sent-banner">
        <span>
          选片已于 <strong>{{ formatTime(submittedAt) }}</strong> 提交。<template v-if="editingWindowOpen">还可调整 {{ editCountdown?.text }}，当前超出套餐 {{ extraSelected }} 张<template v-if="extraAmount">，补款 ¥{{ (extraAmount / 100).toFixed(2) }}</template>。</template><template v-else>调整期已结束，只能追加新照片。当前超出套餐 {{ extraSelected }} 张<template v-if="extraAmount">，补款 ¥{{ (extraAmount / 100).toFixed(2) }}</template>。</template>
        </span>
        <button type="button" class="btn btn-ghost" @click="refreshStatus">刷新状态</button>
      </div>

      <div v-if="engine?.state.lastError" class="notice is-error">
        <span>{{ engine.state.lastError }}</span>
        <div class="notice-actions">
          <button type="button" class="btn btn-ghost" @click="retrySave">重试</button>
        </div>
      </div>

      <div v-if="feed?.state.error" class="notice is-error">
        <span>{{ feed.state.error }}</span>
        <div class="notice-actions">
          <button type="button" class="btn btn-ghost" @click="loadMore">重新加载</button>
        </div>
      </div>

      <div v-if="filter !== 'all' && feed && !feed.state.done" class="notice">
        <span>仅显示已加载的照片，继续向下滚动可加载更多。</span>
      </div>

      <div v-if="!filtered.length" class="grid-empty">
        <template v-if="feed?.state.loading">正在加载照片…</template>
        <template v-else-if="filter === 'selected'">还没有收藏任何照片。</template>
        <template v-else-if="filter === 'rejected'">还没有标记不选的照片。</template>
        <template v-else>相册中暂时没有照片。</template>
      </div>

      <section v-else ref="gridRef" class="grid" aria-label="照片列表">
        <div class="grid-spacer" :style="{ height: `${virtual.totalHeight}px` }">
          <div class="grid-rows" :style="{ transform: `translateY(${virtual.offsetY}px)` }">
            <div
              v-for="(row, rowIndex) in rows"
              :key="rowIndex"
              class="grid-row"
              :style="{ gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))` }"
            >
              <PhotoCard
                v-for="item in row"
                :key="item.asset_id"
                :item="item"
                :decision="decisionOf(item)"
                :disabled="readOnly"
                @open="openLightbox(item)"
                @select="toggle(item, 'selected')"
                @reject="toggle(item, 'rejected')"
                @stale="onStale(item)"
              />
            </div>
          </div>
        </div>
        <div v-if="feed?.state.loading" class="grid-loading">
          <span class="spinner" />
          加载更多照片…
        </div>
      </section>
    </div>

    <footer class="page-footer">
      <p class="brand-mark">PWF <strong>SELECTION</strong></p>
      <p v-if="album?.download_allowed">原片下载将在交付阶段开放，届时会另行通知。</p>
      <p v-else>本相册仅用于选片，不提供下载。</p>
      <p>照片预览带有工作室水印，仅供选片参考。</p>
    </footer>

    <Lightbox
      v-if="lightboxIndex !== null && lightboxItem"
      :items="filtered"
      :index="lightboxIndex"
      :decisions="decisions"
      :disabled="readOnly"
      @update:index="lightboxIndex = $event"
      @close="lightboxIndex = null"
      @select="lightboxItem && toggle(lightboxItem, 'selected')"
      @reject="lightboxItem && toggle(lightboxItem, 'rejected')"
      @stale="onStale(lightboxItem)"
    />

    <ConfirmDialog
      :open="confirmSubmit"
      title="提交选片"
      :message="confirmMessage"
      confirm-text="确认提交"
      :busy="submitting"
      @confirm="doSubmit"
      @cancel="confirmSubmit = false"
    />

    <Transition name="toast">
      <div v-if="toastState.visible" class="toast" :class="`is-${toastState.kind}`" role="status">
        {{ toastState.message }}
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.state-mark {
  letter-spacing: 0.18em;
  font-size: 10px;
  font-weight: 600;
}

.page-footer p {
  margin: 2px 0;
}
</style>
