<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import type { ArchiveDetail, ArchiveItem } from '../api/archive'
import {
  ARCHIVE_ITEM_STATUS_LABELS,
  archiveItemKindLabel,
  archiveItemStatusLabel,
  archiveItemStatusTone,
  archiveStatusLabel,
  archiveStatusTone,
  getArchive,
  listArchiveItems,
  requestArchiveDeletion,
  retryArchiveVerification,
} from '../api/archive'
import PagerBar from '../components/PagerBar.vue'
import { auth } from '../state/session'
import { toast } from '../state/toast'
import { bytes, deadlineLabel, formatDateTime } from '../utils/format'

const props = defineProps<{ id: string }>()

const detail = ref<ArchiveDetail | null>(null)
const loading = ref(true)
const notFound = ref(false)
const error = ref('')

const items = ref<ArchiveItem[]>([])
const itemsTotal = ref(0)
const itemsOffset = ref(0)
const itemsLimit = 50
const statusFilter = ref('')
const itemsLoading = ref(false)

const retrying = ref(false)
const deletionOpen = ref(false)
const deletionBusy = ref(false)
const deletionError = ref('')
const confirmWord = ref('')
const deletionReason = ref('')

let poll: number | null = null

const canManage = computed(() => ['owner', 'staff', 'assistant'].includes(auth.role))
const isOwner = computed(() => auth.role === 'owner')
const isActive = computed(() => ['pending', 'verifying', 'deleting'].includes(detail.value?.status ?? ''))
const canRetry = computed(() => canManage.value && detail.value?.status === 'failed')
const canDelete = computed(
  () =>
    isOwner.value &&
    detail.value?.project_state === 'archived' &&
    (detail.value.status === 'ready' ||
      (detail.value.status === 'failed' && Boolean(detail.value.deletion_requested_at))),
)
const confirmMatches = computed(() => confirmWord.value.trim() === 'DELETE')
const canSubmitDeletion = computed(
  () => confirmMatches.value && deletionReason.value.trim().length > 0 && !deletionBusy.value,
)

function schedulePoll(): void {
  if (poll !== null) {
    window.clearInterval(poll)
    poll = null
  }
  if (!isActive.value) return
  poll = window.setInterval(() => {
    void load(false)
  }, 4000)
}

async function load(showSpinner = true): Promise<void> {
  if (showSpinner) loading.value = true
  error.value = ''
  try {
    detail.value = await getArchive(props.id)
    schedulePoll()
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) notFound.value = true
    else error.value = err instanceof ApiError ? err.detail || err.message : '加载归档失败'
  } finally {
    loading.value = false
  }
}

async function loadItems(): Promise<void> {
  if (!detail.value) return
  itemsLoading.value = true
  try {
    const page = await listArchiveItems(props.id, {
      status: statusFilter.value || undefined,
      limit: itemsLimit,
      offset: itemsOffset.value,
    })
    items.value = page.items
    itemsTotal.value = page.total
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '加载核验明细失败', 'error')
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

watch(statusFilter, () => {
  itemsOffset.value = 0
  void loadItems()
})

async function retry(): Promise<void> {
  if (retrying.value) return
  retrying.value = true
  try {
    await retryArchiveVerification(props.id)
    toast('已重新提交核验，仅会重新校验缺失或不一致的对象', 'success')
    await load(false)
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '重试核验失败', 'error')
  } finally {
    retrying.value = false
  }
}

function openDeletion(): void {
  confirmWord.value = ''
  deletionReason.value = ''
  deletionError.value = ''
  deletionOpen.value = true
}

async function submitDeletion(): Promise<void> {
  if (!canSubmitDeletion.value) return
  deletionBusy.value = true
  deletionError.value = ''
  try {
    await requestArchiveDeletion(props.id, deletionReason.value.trim())
    deletionOpen.value = false
    toast('删除任务已提交，对象将逐个删除；归档记录与审计日志保留', 'success')
    await load(false)
  } catch (err) {
    deletionError.value = err instanceof ApiError ? err.detail || err.message : '提交删除失败'
  } finally {
    deletionBusy.value = false
  }
}

function shortHash(hash?: string): string {
  if (!hash) return '—'
  return `${hash.slice(0, 10)}…${hash.slice(-6)}`
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
    <RouterLink to="/archives" class="back-link">← 归档列表</RouterLink>

    <p v-if="notFound" class="alert error">归档不存在，或不属于当前租户。</p>
    <p v-else-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-else-if="loading && !detail" class="empty muted">加载中…</p>

    <template v-if="detail">
      <header class="detail-head">
        <div class="detail-title">
          <div class="title-row">
            <h1>{{ detail.project_code }} · 归档</h1>
            <span class="pill" :class="archiveStatusTone(detail.status)">{{ archiveStatusLabel(detail.status) }}</span>
            <span v-if="detail.warned_at" class="pill warn">已发到期提醒</span>
          </div>
          <p class="muted">
            {{ detail.project_title }} · 项目状态 {{ detail.project_state }}
            <template v-if="detail.verified_at"> · 核验于 {{ formatDateTime(detail.verified_at) }}</template>
          </p>
        </div>
        <div class="detail-actions">
          <button
            v-if="canRetry"
            class="btn primary"
            type="button"
            :disabled="retrying"
            @click="retry"
          >
            {{ retrying ? '提交中…' : '重新核验' }}
          </button>
          <button v-if="canDelete" class="btn danger" type="button" @click="openDeletion">删除归档对象</button>
          <button class="btn ghost" type="button" :disabled="loading" @click="load()">刷新</button>
        </div>
      </header>

      <p v-if="detail.status === 'failed'" class="alert error" role="alert">
        核验失败：{{ detail.last_error || '存在缺失或哈希不一致的对象' }}。请检查对象存储后重新核验。
      </p>
      <p v-else-if="detail.status === 'deleting'" class="alert warn">
        删除进行中：对象逐个删除，全部完成后归档记录转为「已删除」，业务记录与审计日志始终保留。
      </p>
      <p v-else-if="detail.status === 'deleted'" class="alert warn">
        归档对象已于 {{ formatDateTime(detail.deleted_at) }} 删除。删除原因：{{ detail.reason || '未记录' }}。
      </p>
      <p v-else-if="detail.status === 'ready'" class="muted small">
        归档已校验通过。到期后只会发送提醒，不会自动删除对象。
      </p>

      <div class="stat-strip four">
        <div class="stat">
          <span class="stat-value">{{ detail.verified_count }}</span>
          <span class="stat-label">已校验对象</span>
        </div>
        <div class="stat">
          <span class="stat-value" :class="detail.failed_count > 0 ? 'accent' : ''">{{ detail.failed_count }}</span>
          <span class="stat-label">失败对象</span>
        </div>
        <div class="stat">
          <span class="stat-value small-value">{{ detail.total_bytes ? bytes(detail.total_bytes) : '—' }}</span>
          <span class="stat-label">归档总大小</span>
        </div>
        <div class="stat">
          <span class="stat-value small-value">
            {{ detail.retention_until ? deadlineLabel(detail.retention_until) : '—' }}
          </span>
          <span class="stat-label">保留到期</span>
        </div>
      </div>

      <div class="panel">
        <h2 class="panel-title">保留策略</h2>
        <dl class="package-grid">
          <article class="package-card">
            <header><strong>保留期</strong></header>
            <p class="stat-value small-value">{{ detail.keep_days }} 天</p>
            <p class="muted small">创建归档时快照，之后修改租户策略不会改变本期到期日。</p>
          </article>
          <article class="package-card">
            <header><strong>提前提醒</strong></header>
            <p class="stat-value small-value">{{ detail.warn_days }} 天</p>
            <p class="muted small">
              {{ detail.warned_at ? `已于 ${formatDateTime(detail.warned_at)} 发送一次到期提醒。` : '到期前发送一次站内通知与告警。' }}
            </p>
          </article>
          <article class="package-card">
            <header><strong>到期日</strong></header>
            <p class="stat-value small-value">{{ formatDateTime(detail.retention_until) }}</p>
            <p class="muted small">到期不会自动删除；删除需要 owner 输入 DELETE 并填写原因。</p>
          </article>
        </dl>
      </div>

      <div v-if="detail.status === 'deleted' || detail.deletion_requested_at" class="panel">
        <h2 class="panel-title">删除记录</h2>
        <dl class="package-grid">
          <article class="package-card">
            <header><strong>申请时间</strong></header>
            <p>{{ formatDateTime(detail.deletion_requested_at) }}</p>
          </article>
          <article class="package-card">
            <header><strong>原因</strong></header>
            <p>{{ detail.reason || '—' }}</p>
          </article>
          <article class="package-card">
            <header><strong>删除完成</strong></header>
            <p>{{ formatDateTime(detail.deleted_at) }}</p>
          </article>
        </dl>
      </div>

      <div class="panel">
        <div class="panel-head">
          <h2 class="panel-title">核验明细</h2>
          <label class="inline-field">
            <span>状态</span>
            <select v-model="statusFilter">
              <option value="">全部</option>
              <option v-for="(label, value) in ARCHIVE_ITEM_STATUS_LABELS" :key="value" :value="value">
                {{ label }}
              </option>
            </select>
          </label>
        </div>
        <table class="data compact">
          <thead>
            <tr>
              <th>类型</th>
              <th>对象键</th>
              <th>状态</th>
              <th class="num">大小</th>
              <th>SHA-256</th>
              <th>备注</th>
              <th>校验时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in items" :key="item.id">
              <td>
                {{ archiveItemKindLabel(item.kind) }}
                <div v-if="item.asset_id" class="muted small mono">{{ item.asset_id.slice(0, 8) }}</div>
              </td>
              <td class="mono small">{{ item.object_key }}</td>
              <td>
                <span class="pill" :class="archiveItemStatusTone(item.status)">{{ archiveItemStatusLabel(item.status) }}</span>
              </td>
              <td class="num">{{ item.actual_size || item.expected_size ? bytes(item.actual_size || item.expected_size) : '—' }}</td>
              <td class="mono small" :title="item.actual_sha256 || item.expected_sha256">
                {{ shortHash(item.actual_sha256 || item.expected_sha256) }}
              </td>
              <td class="muted small">{{ item.note || '—' }}</td>
              <td>{{ formatDateTime(item.checked_at) }}</td>
            </tr>
          </tbody>
        </table>
        <p v-if="itemsLoading" class="empty muted">加载中…</p>
        <p v-else-if="items.length === 0" class="empty muted">没有匹配的核验对象。</p>
        <PagerBar
          :total="itemsTotal"
          :limit="itemsLimit"
          :offset="itemsOffset"
          :busy="itemsLoading"
          @change="(next) => { itemsOffset = next; loadItems() }"
        />
      </div>

      <Teleport to="body">
        <div v-if="deletionOpen" class="overlay" @click.self="deletionOpen = false">
          <div class="dialog dialog-wide" role="dialog" aria-modal="true" aria-label="删除归档对象">
            <h2 class="dialog-title">删除归档对象</h2>
            <p class="alert error">
              这将永久删除本项目归档中的全部对象文件，<strong>不可恢复</strong>。合同、付款、发票等业务记录与审计日志会保留。
            </p>
            <label class="field">
              <span>删除原因（将写入审计日志）</span>
              <textarea v-model="deletionReason" class="reason-input" rows="3" maxlength="500"></textarea>
            </label>
            <label class="field">
              <span>请输入 DELETE 确认</span>
              <input v-model="confirmWord" type="text" autocomplete="off" placeholder="DELETE" />
            </label>
            <p v-if="deletionError" class="form-error" role="alert">{{ deletionError }}</p>
            <p v-if="confirmWord !== '' && !confirmMatches" class="muted small">必须输入大写的 DELETE 才能提交。</p>
            <div class="dialog-actions">
              <button class="btn ghost" type="button" :disabled="deletionBusy" @click="deletionOpen = false">取消</button>
              <button class="btn danger" type="button" :disabled="!canSubmitDeletion" @click="submitDeletion">
                {{ deletionBusy ? '提交中…' : '永久删除' }}
              </button>
            </div>
          </div>
        </div>
      </Teleport>
    </template>
  </section>
</template>

<style scoped>
.reason-input {
  width: 100%;
  padding: 0.55rem 0.7rem;
  border-radius: 8px;
  border: 1px solid var(--border-strong);
  background: var(--panel-strong);
  color: var(--text);
  font: inherit;
  resize: vertical;
}
</style>
