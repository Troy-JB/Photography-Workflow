<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, shallowRef, watch } from 'vue'
import { ApiError, isApiError } from '../api/client'
import {
  createDeliveryDownload,
  fetchDeliveryItems,
  type CustomerDelivery,
  type CustomerDeliveryItem,
  type DeliveryDownloadInput,
  type DeliverySpec,
} from '../api/delivery'
import DeliveryTerminal from '../components/DeliveryTerminal.vue'
import {
  applyGrant,
  DeliverySession,
  isDeliveryTerminalError,
  type DeliveryTerminalCode,
} from '../state/delivery'
import { formatBytes } from '../utils/format'
import { requestKey } from '../utils/keys'

const props = defineProps<{ token?: string }>()

type Phase = 'loading' | 'ready' | 'terminal' | 'error'
type ToastKind = 'info' | 'success' | 'error'

const PAGE_SIZE = 40

const phase = ref<Phase>('loading')
const terminalCode = ref<DeliveryTerminalCode>('invalid_link')
const bootError = ref('')

const session = shallowRef<DeliverySession | null>(null)
const delivery = ref<CustomerDelivery | null>(null)

const items = ref<CustomerDeliveryItem[]>([])
const total = ref(0)
const offset = ref(0)
const loadingMore = ref(false)
const itemsError = ref('')
const downloading = ref('')

const toastState = reactive({ message: '', kind: 'info' as ToastKind, visible: false })
let toastTimer: number | undefined

onMounted(() => {
  void boot()
})

// Opening another delivery link in the same tab must reboot instead of showing
// the previous delivery's files.
watch(
  () => props.token,
  (next, prev) => {
    if (next !== prev) void boot()
  },
)

onBeforeUnmount(() => {
  if (toastTimer !== undefined) window.clearTimeout(toastTimer)
})

async function boot() {
  phase.value = 'loading'
  bootError.value = ''
  itemsError.value = ''
  items.value = []
  total.value = 0
  offset.value = 0
  downloading.value = ''

  const current = new DeliverySession()
  session.value = current
  try {
    const view = props.token ? await current.open(props.token) : await current.resume()
    delivery.value = view
    if (props.token) cleanFragment()
    phase.value = 'ready'
    await loadItems(true)
  } catch (error) {
    if (isDeliveryTerminalError(error)) {
      fail(error.code)
      return
    }
    bootError.value = describe(error)
    phase.value = 'error'
  }
}

// cleanFragment drops the link token from the address bar after the exchange.
// The route stays on /d, which resumes from the sessionStorage entry on reload.
function cleanFragment() {
  try {
    window.history.replaceState(window.history.state, '', `${window.location.pathname}${window.location.search}#/d`)
  } catch {
    // The token is only cosmetic at this point; a failed replace is harmless.
  }
}

function fail(code: DeliveryTerminalCode) {
  terminalCode.value = code
  phase.value = 'terminal'
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

/* ---------- items ---------- */

async function loadItems(reset = false): Promise<void> {
  const current = session.value
  if (!current || loadingMore.value) return
  if (reset) {
    items.value = []
    total.value = 0
    offset.value = 0
    itemsError.value = ''
  }
  loadingMore.value = true
  try {
    const page = await current.withSession((jwt) => fetchDeliveryItems(jwt, PAGE_SIZE, offset.value))
    total.value = page.total
    const seen = new Set(items.value.map((item) => item.id))
    items.value.push(...page.items.filter((item) => !seen.has(item.id)))
    offset.value += page.items.length
  } catch (error) {
    if (isDeliveryTerminalError(error)) {
      fail(error.code)
      return
    }
    itemsError.value = describe(error)
  } finally {
    loadingMore.value = false
  }
}

const hasMore = computed(() => items.value.length < total.value)

/* ---------- downloading ---------- */

const exhausted = computed(() => Boolean(delivery.value && delivery.value.downloads_remaining <= 0))

const busy = computed(() => downloading.value !== '')

async function download(target: {
  key: string
  input: DeliveryDownloadInput
}): Promise<void> {
  const current = session.value
  const view = delivery.value
  if (!current || !view || busy.value) return
  downloading.value = target.key
  try {
    const grant = await current.withSession((jwt) =>
      createDeliveryDownload(jwt, target.input, requestKey('delivery-download')),
    )
    delivery.value = applyGrant(view, grant)
    openUrl(grant.url)
    toast(`已开始下载：${grant.file_name}`, 'success')
  } catch (error) {
    if (isDeliveryTerminalError(error)) {
      fail(error.code)
      return
    }
    if (isApiError(error, 409, 'web_spec_unavailable')) {
      toast('该文件只有原图版本，请下载原图', 'error')
    } else {
      toast(describe(error), 'error')
    }
  } finally {
    downloading.value = ''
  }
}

function downloadPackage(pkg: { id: string; spec: DeliverySpec }): void {
  void download({ key: `pkg:${pkg.id}`, input: { package_id: pkg.id } })
}

function downloadItem(item: CustomerDeliveryItem, spec: DeliverySpec): void {
  void download({ key: `item:${item.id}:${spec}`, input: { item_id: item.id, spec } })
}

// openUrl navigates to the presigned URL. The server sets attachment
// disposition, so the browser downloads without leaving this page.
function openUrl(url: string): void {
  window.location.assign(url)
}

/* ---------- copy ---------- */

const specLabel = (spec: DeliverySpec): string => (spec === 'web' ? '网页版' : '原图')

const specHint = (spec: DeliverySpec): string =>
  spec === 'web' ? '长边 2048 像素 JPEG，适合分享和查看' : '原始文件，保留完整画质与元数据'

const expiryText = computed(() => {
  const raw = delivery.value?.expires_at
  if (!raw) return ''
  const date = new Date(raw)
  if (Number.isNaN(date.getTime())) return ''
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(date)
})

const itemMeta = (item: CustomerDeliveryItem): string => {
  const size = formatBytes(item.original_size)
  return item.revision_no > 0 ? `${size} · 修图版` : size
}
</script>

<template>
  <DeliveryTerminal v-if="phase === 'terminal'" :code="terminalCode" />

  <div v-else-if="phase === 'loading'" class="page">
    <div class="centered-page">
      <div class="loading-state">
        <span class="spinner" />
        <p>正在打开交付文件…</p>
      </div>
    </div>
  </div>

  <div v-else-if="phase === 'error'" class="page">
    <div class="centered-page">
      <div class="state-card">
        <div class="state-mark">ERROR</div>
        <h1>交付加载失败</h1>
        <p>{{ bootError }}</p>
        <button type="button" class="btn btn-primary" @click="boot">重新加载</button>
      </div>
    </div>
    <footer class="page-footer">如需帮助，请联系为您服务的摄影师或工作室。</footer>
  </div>

  <div v-else class="page">
    <header class="topbar">
      <div class="page-inner topbar-inner">
        <div class="topbar-row">
          <div>
            <p class="brand-mark">PWF <strong>DELIVERY</strong></p>
            <h1 class="album-title">{{ delivery?.project_title || '交付下载' }}</h1>
            <p class="album-sub">
              <span>第 {{ delivery?.round_no ?? 1 }} 轮 · {{ delivery?.item_count ?? 0 }} 个文件</span>
              <span class="delivery-quota" :class="{ 'is-empty': exhausted }">
                剩余 {{ delivery?.downloads_remaining ?? 0 }} 次下载
              </span>
            </p>
          </div>
          <div class="header-actions">
            <button type="button" class="btn btn-ghost" :disabled="busy" @click="boot">
              刷新
            </button>
          </div>
        </div>
        <p class="delivery-hint">
          每个链接有独立下载次数；每次下载一个交付包或一张照片都会消耗 1 次。
        </p>
      </div>
    </header>

    <div class="page-inner">
      <div v-if="exhausted" class="notice is-warn">
        <span>本链接的下载次数已用完。如需继续下载，请联系工作室重新发送。</span>
      </div>
      <div v-else-if="delivery && delivery.web_skipped > 0" class="notice is-warn">
        <span>
          {{ delivery.web_skipped }} 个文件为 RAW / HEIC 等格式，没有网页版，请下载原图包获取完整文件。
        </span>
      </div>

      <section class="delivery-packages" aria-label="交付包">
        <article
          v-for="pkg in delivery?.packages ?? []"
          :key="pkg.id"
          class="package-card"
          :class="`is-${pkg.spec}`"
        >
          <div class="package-head">
            <h2>{{ specLabel(pkg.spec) }}</h2>
            <span class="package-size">{{ formatBytes(pkg.size_bytes) }}</span>
          </div>
          <p class="package-hint">{{ specHint(pkg.spec) }}</p>
          <p class="package-meta">
            {{ pkg.entry_count }} 个文件<span v-if="pkg.file_name"> · {{ pkg.file_name }}</span>
          </p>
          <button
            type="button"
            class="btn btn-primary btn-block"
            :disabled="busy || exhausted"
            @click="downloadPackage(pkg)"
          >
            {{ downloading === `pkg:${pkg.id}` ? '正在生成链接…' : `下载${specLabel(pkg.spec)}包` }}
          </button>
        </article>
      </section>

      <div v-if="itemsError" class="notice is-error">
        <span>{{ itemsError }}</span>
        <div class="notice-actions">
          <button type="button" class="btn btn-ghost" @click="loadItems(true)">重新加载</button>
        </div>
      </div>

      <h2 class="delivery-section-title">逐张下载</h2>

      <div v-if="!items.length && !loadingMore" class="grid-empty">
        {{ itemsError ? '文件列表加载失败，请重试。' : '暂无交付文件。' }}
      </div>

      <section v-else class="delivery-grid" aria-label="交付文件列表">
        <article v-for="item in items" :key="item.id" class="file-card">
          <div class="file-thumb">
            <img v-if="item.thumb_url" :src="item.thumb_url" :alt="item.file_name" loading="lazy" />
            <span v-else class="file-thumb-empty">无预览</span>
          </div>
          <div class="file-body">
            <p class="file-name" :title="item.file_name">{{ item.file_name }}</p>
            <p class="file-meta">
              {{ itemMeta(item) }}
              <span v-if="!item.web_available" class="file-badge">仅原图</span>
            </p>
            <div class="file-actions">
              <button
                v-if="item.web_available"
                type="button"
                class="btn btn-ghost sm"
                :disabled="busy || exhausted"
                @click="downloadItem(item, 'web')"
              >
                {{ downloading === `item:${item.id}:web` ? '生成中…' : '网页版' }}
              </button>
              <button
                type="button"
                class="btn btn-ghost sm"
                :disabled="busy || exhausted"
                @click="downloadItem(item, 'original')"
              >
                {{ downloading === `item:${item.id}:original` ? '生成中…' : '原图' }}
              </button>
            </div>
          </div>
        </article>
      </section>

      <div class="delivery-more">
        <button
          v-if="hasMore"
          type="button"
          class="btn btn-ghost"
          :disabled="loadingMore"
          @click="loadItems(false)"
        >
          {{ loadingMore ? '加载中…' : `加载更多（${items.length}/${total}）` }}
        </button>
      </div>
    </div>

    <footer class="page-footer">
      <p class="brand-mark">PWF <strong>DELIVERY</strong></p>
      <p v-if="expiryText">本链接有效期至 {{ expiryText }}，过期后请联系工作室重新发送。</p>
      <p>交付文件仅供您本人使用，请勿公开传播。</p>
    </footer>

    <Transition name="toast">
      <div v-if="toastState.visible" class="toast" :class="`is-${toastState.kind}`" role="status">
        {{ toastState.message }}
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.delivery-quota {
  display: inline-flex;
  align-items: center;
  padding: 3px 10px;
  border-radius: 999px;
  border: 1px solid rgb(148 97 40 / 30%);
  background: var(--bg-soft);
  color: var(--accent-strong);
  font-size: 12px;
  white-space: nowrap;
}

.delivery-quota.is-empty {
  border-color: var(--line);
  color: var(--muted);
}

.delivery-hint {
  margin: 0;
  color: var(--muted);
  font-size: 12.5px;
  line-height: 1.6;
}

.delivery-packages {
  margin-top: 14px;
  display: grid;
  gap: var(--gap);
  grid-template-columns: minmax(0, 1fr);
}

@media (min-width: 640px) {
  .delivery-packages {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

.package-card {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 16px;
  border: 1px solid var(--line);
  border-radius: var(--radius);
  background: var(--bg-soft);
}

.package-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 10px;
}

.package-head h2 {
  margin: 0;
  font-size: 16px;
  letter-spacing: 0.02em;
}

.package-size {
  color: var(--muted);
  font-size: 12.5px;
  font-variant-numeric: tabular-nums;
}

.package-hint,
.package-meta {
  margin: 0;
  color: var(--muted);
  font-size: 12.5px;
  line-height: 1.6;
}

.package-meta {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.package-card .btn {
  margin-top: 8px;
}

.delivery-section-title {
  margin: 22px 0 10px;
  font-size: 14px;
  letter-spacing: 0.04em;
  color: var(--muted);
  font-weight: 600;
}

.delivery-grid {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: var(--gap);
}

@media (min-width: 560px) {
  .delivery-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (min-width: 960px) {
  .delivery-grid {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }
}

.file-card {
  display: flex;
  gap: 12px;
  padding: 10px;
  border: 1px solid var(--line);
  border-radius: var(--radius);
  background: var(--bg-soft);
}

.file-thumb {
  flex: 0 0 88px;
  height: 88px;
  border-radius: 8px;
  overflow: hidden;
  background: var(--panel-strong);
  display: flex;
  align-items: center;
  justify-content: center;
}

.file-thumb img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}

.file-thumb-empty {
  color: var(--muted);
  font-size: 11px;
}

.file-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.file-name {
  margin: 0;
  font-size: 13px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.file-meta {
  margin: 0;
  color: var(--muted);
  font-size: 12px;
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.file-badge {
  padding: 1px 7px;
  border-radius: 999px;
  border: 1px solid var(--line);
  font-size: 11px;
}

.file-actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: auto;
}

.delivery-more {
  display: flex;
  justify-content: center;
  padding: 18px 0 8px;
}
</style>
