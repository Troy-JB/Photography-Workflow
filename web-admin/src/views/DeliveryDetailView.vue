<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { ApiError } from '../api/client'
import type { DeliveryDetail, DeliveryItem } from '../api/delivery'
import {
  PACKAGE_SPEC_LABELS,
  buildDelivery,
  deliveryStatusLabel,
  deliveryStatusTone,
  getDelivery,
  listDeliveryItems,
  notifyDeliveryCustomer,
} from '../api/delivery'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import DownloadTokensTab from '../components/DownloadTokensTab.vue'
import PagerBar from '../components/PagerBar.vue'
import { auth } from '../state/session'
import { toast } from '../state/toast'
import { bytes, formatDateTime } from '../utils/format'

const props = defineProps<{ id: string }>()

const detail = ref<DeliveryDetail | null>(null)
const loading = ref(true)
const notFound = ref(false)
const error = ref('')

const items = ref<DeliveryItem[]>([])
const itemsTotal = ref(0)
const itemsOffset = ref(0)
const itemsLimit = 50
const webFilter = ref('')
const itemsLoading = ref(false)

const building = ref(false)
const buildAsk = ref(false)
const buildError = ref('')

const notifyBusy = ref(false)
const notifyRecipient = ref('')

let poll: number | null = null

const canManage = computed(() => ['owner', 'staff', 'assistant'].includes(auth.role))
const projectClosed = computed(() => detail.value?.project_state === 'cancelled' || detail.value?.project_state === 'archived')
const paymentOk = computed(() => detail.value?.payment_status === 'paid')
const isReady = computed(() => detail.value?.status === 'ready')
const isBusy = computed(() => detail.value?.status === 'building')
const currentPackages = computed(() => detail.value?.packages ?? [])
const activeTokens = computed(() => (detail.value?.tokens ?? []).filter((token) => !token.revoked_at))

function schedulePoll(): void {
  if (poll !== null) {
    window.clearInterval(poll)
    poll = null
  }
  const status = detail.value?.status
  if (status !== 'building' && status !== 'pending') return
  poll = window.setInterval(() => {
    void load(false)
  }, 4000)
}

async function load(showSpinner = true): Promise<void> {
  if (showSpinner) loading.value = true
  error.value = ''
  try {
    detail.value = await getDelivery(props.id)
    schedulePoll()
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) notFound.value = true
    else error.value = err instanceof ApiError ? err.detail || err.message : '加载交付失败'
  } finally {
    loading.value = false
  }
}

async function loadItems(): Promise<void> {
  if (!detail.value) return
  itemsLoading.value = true
  try {
    const page = await listDeliveryItems(props.id, {
      web_status: webFilter.value || undefined,
      limit: itemsLimit,
      offset: itemsOffset.value,
    })
    items.value = page.items
    itemsTotal.value = page.total
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '加载文件清单失败', 'error')
  } finally {
    itemsLoading.value = false
  }
}

watch(
  () => props.id,
  () => {
    detail.value = null
    items.value = []
    itemsTotal.value = 0
    itemsOffset.value = 0
    notFound.value = false
    void load().then(loadItems)
  },
)

watch(webFilter, () => {
  itemsOffset.value = 0
  void loadItems()
})

async function confirmBuild(): Promise<void> {
  if (building.value) return
  building.value = true
  buildError.value = ''
  try {
    await buildDelivery(props.id)
    buildAsk.value = false
    toast('构建任务已提交，完成后刷新即可签发链接', 'success')
    await load(false)
  } catch (err) {
    buildAsk.value = false
    buildError.value = err instanceof ApiError ? err.detail || err.message : '提交构建失败'
    toast(buildError.value, 'error')
  } finally {
    building.value = false
  }
}

async function notify(): Promise<void> {
  if (notifyBusy.value) return
  notifyBusy.value = true
  try {
    const result = await notifyDeliveryCustomer(props.id, notifyRecipient.value.trim() || undefined)
    toast(`交付邮件已加入发送队列（消息 ${result.message_id.slice(0, 8)}）`, 'success')
    notifyRecipient.value = ''
    await load(false)
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '发送失败', 'error')
  } finally {
    notifyBusy.value = false
  }
}

function packageTone(status: string): string {
  if (status === 'ready') return 'ok'
  if (status === 'failed') return 'danger'
  if (status === 'building') return 'warn'
  return 'muted'
}

function shortHash(hash?: string): string {
  if (!hash) return '—'
  return `${hash.slice(0, 12)}…${hash.slice(-8)}`
}

onMounted(async () => {
  await load()
  await loadItems()
})

onUnmounted(() => {
  if (poll !== null) window.clearInterval(poll)
})
</script>

<template>
  <section class="page detail">
    <RouterLink to="/deliveries" class="back-link">← 交付列表</RouterLink>

    <p v-if="notFound" class="alert error">交付不存在，或已被删除。</p>
    <p v-else-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-else-if="loading && !detail" class="empty muted">加载中…</p>

    <template v-if="detail">
      <header class="detail-head">
        <div class="detail-title">
          <div class="title-row">
            <h1>{{ detail.project_code }} · 第 {{ detail.round_no }} 轮交付</h1>
            <span class="pill" :class="deliveryStatusTone(detail.status)">{{ deliveryStatusLabel(detail.status) }}</span>
            <span class="pill" :class="paymentOk ? 'ok' : 'warn'">{{ paymentOk ? '尾款已结清' : '尾款未结清' }}</span>
            <span v-if="detail.blocked_reason" class="pill danger">阻断：{{ detail.blocked_reason }}</span>
          </div>
          <p class="muted">
            {{ detail.project_title }} · 构建版本 v{{ detail.build_version }}
            <template v-if="detail.ready_at"> · 就绪于 {{ formatDateTime(detail.ready_at) }}</template>
          </p>
        </div>
        <div class="detail-actions">
          <button
            class="btn primary"
            type="button"
            :disabled="building || isBusy || !canManage || !paymentOk || projectClosed"
            :title="projectClosed ? '项目已关闭，交付仅保留历史记录' : !paymentOk ? '尾款未结清，不能构建' : isBusy ? '构建进行中' : ''"
            @click="buildAsk = true"
          >
            {{ building ? '提交中…' : isBusy ? '构建中…' : detail.build_version > 1 || detail.packages.length > 0 ? '重新构建' : '开始构建' }}
          </button>
          <button class="btn ghost" type="button" :disabled="loading" @click="load()">刷新</button>
        </div>
      </header>

      <p v-if="!paymentOk" class="alert warn">
        尾款未结清：构建、签发链接与客户下载均被阻断（退款后会立即失效）。请先在项目中完成收款。
      </p>
      <p v-if="projectClosed" class="alert warning" role="status">项目已关闭。历史交付记录保留，不能重新构建或签发下载链接。</p>
      <p v-else-if="detail.status === 'blocked'" class="alert warn">
        交付因付款状态回退被阻断，结清后请重新构建。
      </p>
      <p v-if="buildError" class="form-error" role="alert">{{ buildError }}</p>
      <p v-if="detail.last_error" class="alert error">最近一次构建错误：{{ detail.last_error }}</p>

      <div class="stat-strip four">
        <div class="stat">
          <span class="stat-value">{{ detail.item_count }}</span>
          <span class="stat-label">照片</span>
        </div>
        <div class="stat">
          <span class="stat-value small-value">{{ detail.total_bytes ? bytes(detail.total_bytes) : '—' }}</span>
          <span class="stat-label">交付总大小</span>
        </div>
        <div class="stat">
          <span class="stat-value accent">{{ detail.web_ready }}</span>
          <span class="stat-label">网页版就绪 / {{ detail.item_count }}</span>
        </div>
        <div class="stat">
          <span class="stat-value">{{ detail.web_skipped }}</span>
          <span class="stat-label">无网页版（仅原图）</span>
        </div>
      </div>

      <div class="panel">
        <h2 class="panel-title">交付包</h2>
        <div class="package-grid">
          <article v-for="pkg in currentPackages" :key="pkg.id" class="package-card">
            <header>
              <strong>{{ PACKAGE_SPEC_LABELS[pkg.spec] }}</strong>
              <span class="pill" :class="packageTone(pkg.status)">{{ pkg.status }}</span>
            </header>
            <dl>
              <div><dt>版本</dt><dd>v{{ pkg.version }}</dd></div>
              <div><dt>大小</dt><dd>{{ pkg.size_bytes ? bytes(pkg.size_bytes) : '—' }}</dd></div>
              <div><dt>文件数</dt><dd>{{ pkg.entry_count || '—' }}</dd></div>
              <div><dt>文件名</dt><dd class="mono small">{{ pkg.file_name || '—' }}</dd></div>
              <div class="wide"><dt>SHA-256</dt><dd class="mono small" :title="pkg.sha256">{{ shortHash(pkg.sha256) }}</dd></div>
              <div><dt>完成时间</dt><dd>{{ formatDateTime(pkg.built_at) }}</dd></div>
            </dl>
            <p v-if="pkg.last_error" class="form-error">{{ pkg.last_error }}</p>
          </article>
          <p v-if="currentPackages.length === 0" class="muted small">尚未构建，提交构建后会生成网页版与原图两个 ZIP。</p>
        </div>
        <p class="muted small">
          网页版为长边 2048 的无水印 JPEG；原图包含审片通过的最新修图版本，没有修图版本的素材使用原始拍摄文件。HEIC/RAW 等无法解码的格式只进入原图包，并记录警告。
        </p>
      </div>

      <div v-if="detail.warnings.length > 0" class="panel">
        <h2 class="panel-title">网页版警告</h2>
        <ul class="warning-list">
          <li v-for="warning in detail.warnings" :key="warning.code">
            <span class="pill warn">{{ warning.code }}</span>
            <span>{{ warning.message }}</span>
            <span class="muted small">{{ warning.count }} 个文件</span>
          </li>
        </ul>
      </div>

      <DownloadTokensTab
        :delivery-id="detail.id"
        :tokens="detail.tokens"
        :can-manage="canManage"
        :ready="isReady"
        :payment-ok="paymentOk"
        @refresh="load(false)"
      />

      <div class="panel">
        <h2 class="panel-title">通知客户</h2>
        <p class="muted small">
          签发一个新链接并通过邮件发送给客户；明文链接只存在于邮件正文，发送成功后会被清空。旧链接仍然有效，可在上方单独撤销。
        </p>
        <form class="inline-form" @submit.prevent="notify">
          <label class="field">
            <span>收件邮箱（留空使用客户邮箱）</span>
            <input v-model="notifyRecipient" type="email" maxlength="200" placeholder="client@example.com" />
          </label>
          <button
            class="btn primary"
            type="submit"
            :disabled="notifyBusy || !isReady || !paymentOk || !canManage || activeTokens.length >= 50"
          >
            {{ notifyBusy ? '发送中…' : '发送交付邮件' }}
          </button>
        </form>
      </div>

      <div class="panel">
        <div class="panel-head">
          <h2 class="panel-title">文件清单</h2>
          <label class="inline-field">
            <span>网页版</span>
            <select v-model="webFilter">
              <option value="">全部</option>
              <option value="ready">已就绪</option>
              <option value="skipped">仅原图</option>
              <option value="pending">处理中</option>
            </select>
          </label>
        </div>
        <table class="data compact">
          <thead>
            <tr>
              <th>#</th>
              <th>原始文件</th>
              <th>来源</th>
              <th>网页版</th>
              <th class="num">原图大小</th>
              <th>警告</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in items" :key="item.id">
              <td>{{ item.position }}</td>
              <td>
                {{ item.original_name }}
                <div class="muted small">{{ item.original_mime || '未知类型' }}</div>
              </td>
              <td>
                <span v-if="item.revision_no > 0" class="pill ok">修图 v{{ item.revision_no }}</span>
                <span v-else class="muted small">原始文件</span>
              </td>
              <td>
                <span v-if="item.web_status === 'ready'" class="pill ok">{{ item.web_width }}×{{ item.web_height }}</span>
                <span v-else-if="item.web_status === 'skipped'" class="pill warn">跳过</span>
                <span v-else class="pill muted">处理中</span>
              </td>
              <td class="num">{{ bytes(item.original_size) }}</td>
              <td class="muted small">{{ item.web_warning || '—' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-if="itemsLoading" class="empty muted">加载中…</p>
        <p v-else-if="items.length === 0" class="empty muted">没有匹配的文件。</p>
        <PagerBar
          :total="itemsTotal"
          :limit="itemsLimit"
          :offset="itemsOffset"
          :busy="itemsLoading"
          @change="(next) => { itemsOffset = next; loadItems() }"
        />
      </div>

      <ConfirmDialog
        :open="buildAsk"
        title="构建交付包"
        :message="
          detail.packages.length > 0
            ? '将重新构建网页版与原图两个 ZIP，构建版本加一，旧版本对象会保留一版用于排查。构建期间客户仍可下载上一版。'
            : '将生成网页版（长边 2048 JPEG）与原图两个 ZIP。构建在后台进行，完成后才能签发下载链接。'
        "
        confirm-text="提交构建"
        :busy="building"
        @cancel="buildAsk = false"
        @confirm="confirmBuild"
      />
    </template>
  </section>
</template>
