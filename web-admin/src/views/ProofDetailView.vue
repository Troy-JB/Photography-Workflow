<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import { createDelivery } from '../api/delivery'
import type { ProofRoundDetail, ProofRoundItem, ProofToken } from '../api/proof'
import {
  extendProofRound,
  getProofRound,
  issueProofToken,
  notifyProofCustomer,
  resubmitProofRound,
  revokeProofToken,
  startRetouch,
  uploadProofRevision,
} from '../api/proof'
import type { GalleryDetail, Project } from '../api/types'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import ProofLightbox from '../components/ProofLightbox.vue'
import { api, auth } from '../state/session'
import { toast } from '../state/toast'
import { copyText } from '../utils/clipboard'
import {
  deadlineLabel,
  formatDateTime,
  fromLocalInput,
  proofStatusLabel,
  proofStatusTone,
  toLocalInput,
} from '../utils/format'
import { parseNumberInput } from '../utils/number'
import { imageSize, sha256Hex, stripExtension } from '../utils/upload'

const props = defineProps<{ id: string }>()
const router = useRouter()

type TabKey = 'overview' | 'assets' | 'retouch'

interface UploadEntry {
  key: number
  name: string
  status: 'pending' | 'ok' | 'duplicate' | 'error'
  message?: string
}

const round = ref<ProofRoundDetail | null>(null)
const project = ref<Project | null>(null)
const galleryTitle = ref('')
const loading = ref(true)
const notFound = ref(false)
const error = ref('')
const activeTab = ref<TabKey>('overview')

const dueDraft = ref('')
const extending = ref(false)
const extendError = ref('')

const tokenLabel = ref('')
const tokenMaxVisits = ref('')
const tokenExpires = ref('')
const tokenBusy = ref(false)
const tokenError = ref('')
const revoking = ref<ProofToken | null>(null)
const revokeBusy = ref(false)

const notifyBusy = ref(false)
const oneTime = ref<{ title: string; note: string; url: string; token?: string } | null>(null)

const selectedItem = ref<ProofRoundItem | null>(null)
const selectedItemLive = computed(() => {
  if (!selectedItem.value || !round.value) return selectedItem.value
  return round.value.items.find((item) => item.asset_id === selectedItem.value?.asset_id) ?? selectedItem.value
})

const retouchBusy = ref(false)
const resubmitAsk = ref(false)
const resubmitBusy = ref(false)
const deliveryBusy = ref(false)
const uploadEntries = ref<UploadEntry[]>([])
const uploading = ref(false)
const uploadTarget = ref('')
const bulkInput = ref<HTMLInputElement | null>(null)
const singleInput = ref<HTMLInputElement | null>(null)
let uploadSeq = 0

const canManage = computed(() => ['owner', 'staff', 'assistant'].includes(auth.role))
const projectClosed = computed(() => project.value?.state === 'cancelled' || project.value?.state === 'archived')
const canOperate = computed(() => canManage.value && !projectClosed.value)
const isApproved = computed(() => round.value?.status === 'approved')
const activeTokens = computed(() => (round.value?.tokens ?? []).filter((t) => !t.revoked_at))

const PROVIDERS: Record<string, string> = { manual: '手工上传', folder_watch: '目录监控' }

function tokenState(token: ProofToken): { label: string; tone: string } {
  if (token.revoked_at) return { label: '已撤销', tone: 'warn' }
  if (token.expires_at && Date.parse(token.expires_at) <= Date.now()) return { label: '已过期', tone: 'muted' }
  return { label: '有效', tone: 'ok' }
}

const timeline = computed(() => {
  const r = round.value
  if (!r) return []
  const feedback = r.status === 'approved' ? '客户确认通过' : '客户提出修改'
  const steps = [
    { key: 'created', label: '创建审片', at: r.created_at },
    { key: 'viewed', label: '客户查看', at: r.viewed_at },
    { key: 'feedback', label: feedback, at: r.submitted_at },
    { key: 'retouch', label: '开始修图', at: r.retouch?.started_at },
    { key: 'delivered', label: '修图回传', at: r.retouch?.delivered_at },
    { key: 'approved', label: '客户通过', at: r.approved_at },
  ]
  const current: Record<string, string> = {
    open: 'viewed',
    in_review: 'feedback',
    changes_requested: 'retouch',
    retouching: 'delivered',
    resubmitted: 'approved',
    approved: 'approved',
  }
  const currentKey = current[r.status] ?? ''
  return steps.map((step) => ({
    ...step,
    state: step.at ? 'done' : step.key === currentKey ? 'current' : 'pending',
  }))
})

const retouchItems = computed(() => round.value?.items ?? [])
const missingItems = computed(() => retouchItems.value.filter((item) => !readyThisCycle(item)))
const readyCount = computed(() => retouchItems.value.length - missingItems.value.length)

function readyThisCycle(item: ProofRoundItem): boolean {
  if (item.revision_no <= 0 || item.revision_state !== 'derivative_ready') return false
  const started = round.value?.retouch?.started_at
  if (!started) return false
  if (!item.revision_created_at) return true
  return Date.parse(item.revision_created_at) >= Date.parse(started)
}

function revisionLabel(item: ProofRoundItem): { text: string; tone: string } | null {
  if (item.revision_no <= 0) return null
  if (item.revision_state === 'derivative_ready') return { text: `修图版 v${item.revision_no}`, tone: 'ok' }
  if (item.revision_state === 'failed') return { text: `v${item.revision_no} 派生失败`, tone: 'danger' }
  return { text: `v${item.revision_no} 派生中`, tone: 'warn' }
}

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const detail = await getProofRound(props.id)
    round.value = detail
    dueDraft.value = toLocalInput(detail.due_at)
    if (!project.value || project.value.id !== detail.project_id) {
      api<Project>(`/api/v1/projects/${detail.project_id}`)
        .then((value) => {
          project.value = value
        })
        .catch(() => {
          // The header falls back to the project id.
        })
    }
    api<GalleryDetail>(`/api/v1/galleries/${detail.gallery_id}`)
      .then((value) => {
        galleryTitle.value = value.title
      })
      .catch(() => {
        galleryTitle.value = ''
      })
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) notFound.value = true
    else error.value = err instanceof ApiError ? err.detail || err.message : '加载审片失败'
  } finally {
    loading.value = false
  }
}

watch(
  () => props.id,
  () => {
    round.value = null
    project.value = null
    galleryTitle.value = ''
    notFound.value = false
    activeTab.value = 'overview'
    selectedItem.value = null
    uploadEntries.value = []
    void load()
  },
  { immediate: true },
)

async function extend(): Promise<void> {
  if (!round.value || extending.value) return
  const due = fromLocalInput(dueDraft.value)
  if (!due) {
    extendError.value = '请选择有效的截止时间。'
    return
  }
  extending.value = true
  extendError.value = ''
  try {
    await extendProofRound(round.value.id, due, round.value.version)
    toast('审片截止时间已更新', 'success')
    await load()
  } catch (err) {
    if (err instanceof ApiError && err.code === 'version_conflict') {
      toast('轮次已在别处被修改，已刷新到最新状态', 'error')
      await load()
    } else {
      extendError.value = err instanceof ApiError ? err.detail || err.message : '延期失败'
    }
  } finally {
    extending.value = false
  }
}

async function createToken(): Promise<void> {
  if (!round.value || tokenBusy.value) return
  const parsed = parseNumberInput(tokenMaxVisits.value)
  const visits = parsed.empty ? 0 : parsed.value
  if (!Number.isInteger(visits) || visits < 0) {
    tokenError.value = '访问次数上限必须是非负整数，0 表示不限'
    return
  }
  tokenBusy.value = true
  tokenError.value = ''
  try {
    const issued = await issueProofToken(round.value.id, {
      label: tokenLabel.value.trim(),
      max_visits: visits,
      expires_at: fromLocalInput(tokenExpires.value),
    })
    oneTime.value = {
      title: '审片链接已生成',
      note: '明文链接只显示一次，数据库仅保存哈希；撤销令牌后旧链接立即失效。',
      url: issued.proof_url,
      token: issued.token,
    }
    tokenLabel.value = ''
    tokenMaxVisits.value = ''
    tokenExpires.value = ''
    await load()
  } catch (err) {
    tokenError.value = err instanceof ApiError ? err.detail || err.message : '创建链接失败'
  } finally {
    tokenBusy.value = false
  }
}

async function confirmRevoke(): Promise<void> {
  const token = revoking.value
  if (!token || !round.value) return
  revokeBusy.value = true
  try {
    await revokeProofToken(round.value.id, token.id)
    toast('审片链接已撤销，客户下一次请求即失效', 'success')
    revoking.value = null
    await load()
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '撤销失败', 'error')
  } finally {
    revokeBusy.value = false
  }
}

async function notifyEmail(): Promise<void> {
  if (!round.value || notifyBusy.value) return
  notifyBusy.value = true
  try {
    const result = await notifyProofCustomer(round.value.id, 'email')
    oneTime.value = {
      title: '邀请邮件已加入发送队列',
      note: '邮件由后台发送，发送成功后正文会被清空；下方链接为本次新签发的链接。',
      url: result.proof_url,
      token: result.token,
    }
    await load()
  } catch (err) {
    const detail = err instanceof ApiError ? err.detail || err.message : '发送失败'
    toast(detail, 'error')
  } finally {
    notifyBusy.value = false
  }
}

async function copy(value: string, what = '链接'): Promise<void> {
  const ok = await copyText(value)
  toast(ok ? `${what}已复制` : '复制失败，请手动选择文本复制', ok ? 'success' : 'error')
}

async function openDelivery(): Promise<void> {
  if (!round.value || deliveryBusy.value) return
  deliveryBusy.value = true
  try {
    const summary = await createDelivery(round.value.id)
    void router.push({ name: 'delivery', params: { id: summary.id } })
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '创建交付失败', 'error')
  } finally {
    deliveryBusy.value = false
  }
}

async function beginRetouch(): Promise<void> {
  if (!round.value || retouchBusy.value) return
  retouchBusy.value = true
  try {
    const task = await startRetouch(round.value.id)
    toast(
      task.provider === 'folder_watch'
        ? '修图任务已创建，源文件将导出到监控目录'
        : '修图任务已创建，请上传修图后的版本',
      'success',
    )
    await load()
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '接单失败', 'error')
  } finally {
    retouchBusy.value = false
  }
}

function openBulkPicker(): void {
  bulkInput.value?.click()
}

function openSinglePicker(item: ProofRoundItem): void {
  uploadTarget.value = item.asset_id
  singleInput.value?.click()
}

function onBulkPicked(event: Event): void {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = ''
  void uploadFiles(files)
}

function onSinglePicked(event: Event): void {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  const assetID = uploadTarget.value
  input.value = ''
  uploadTarget.value = ''
  if (files.length > 0) void uploadFiles([files[0]], assetID)
}

async function uploadFiles(files: File[], forcedAssetID = ''): Promise<void> {
  const current = round.value
  if (!current || current.status !== 'retouching' || uploading.value || files.length === 0) return
  uploading.value = true
  let imported = 0
  let duplicates = 0
  let failures = 0
  try {
    for (const file of files) {
      const entry: UploadEntry = { key: ++uploadSeq, name: file.name, status: 'pending' }
      uploadEntries.value = [entry, ...uploadEntries.value].slice(0, 30)

      let item: ProofRoundItem | undefined
      if (forcedAssetID) {
        item = current.items.find((candidate) => candidate.asset_id === forcedAssetID)
      } else {
        const stem = stripExtension(file.name).toLowerCase()
        item = current.items.find((candidate) => stripExtension(candidate.filename).toLowerCase() === stem)
      }
      if (!item) {
        entry.status = 'error'
        entry.message = '未找到同名素材，请使用该素材行的「上传」按钮'
        failures += 1
        continue
      }
      try {
        const [digest, size] = await Promise.all([sha256Hex(file), imageSize(file)])
        const form = new FormData()
        form.set('asset_id', item.asset_id)
        form.set('sha256', digest)
        form.set('size', String(file.size))
        form.set('width', String(size.width))
        form.set('height', String(size.height))
        form.set('file', file, file.name)
        const result = await uploadProofRevision(current.id, form)
        if (result.duplicate) {
          const started = current.retouch?.started_at
          const thisCycle = started ? Date.parse(result.created_at) >= Date.parse(started) : false
          entry.status = 'duplicate'
          entry.message = thisCycle
            ? `校验和一致，沿用本轮 ${item.filename} 的 v${result.revision_no}`
            : `与历史 v${result.revision_no} 相同，本轮需要新的改动`
          duplicates += 1
        } else {
          entry.status = 'ok'
          entry.message = `${item.filename} → v${result.revision_no}，后台生成预览中`
          imported += 1
        }
      } catch (err) {
        entry.status = 'error'
        entry.message = err instanceof ApiError ? err.detail || err.message : '上传失败'
        failures += 1
      }
    }
  } finally {
    uploading.value = false
  }
  const parts = [`导入 ${imported} 个`]
  if (duplicates > 0) parts.push(`重复 ${duplicates} 个`)
  if (failures > 0) parts.push(`失败 ${failures} 个`)
  toast(parts.join('，'), failures > 0 ? 'error' : 'success')
  await load()
}

async function confirmResubmit(): Promise<void> {
  if (!round.value || resubmitBusy.value) return
  resubmitBusy.value = true
  try {
    await resubmitProofRound(round.value.id)
    resubmitAsk.value = false
    toast('修图版本已回传，客户将收到新的确认邮件', 'success')
    await load()
    activeTab.value = 'assets'
  } catch (err) {
    resubmitAsk.value = false
    if (err instanceof ApiError && err.code === 'revisions_incomplete') {
      toast(err.detail || '仍有素材缺少本轮修图版本', 'error')
      await load()
    } else {
      toast(err instanceof ApiError ? err.detail || err.message : '回传失败', 'error')
    }
  } finally {
    resubmitBusy.value = false
  }
}
</script>

<template>
  <section class="page detail">
    <RouterLink to="/proofs" class="back-link">← 审片列表</RouterLink>

    <p v-if="notFound" class="alert error">审片轮次不存在，或已被删除。</p>
    <p v-else-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-else-if="loading && !round" class="empty muted">加载中…</p>

    <template v-if="round">
      <header class="detail-head">
        <div class="detail-title">
          <div class="title-row">
            <h1>第 {{ round.round_no }} 轮审片</h1>
            <span class="pill" :class="proofStatusTone(round.status)">{{ proofStatusLabel(round.status) }}</span>
            <span v-if="round.override_reason" class="pill danger">owner 放行</span>
          </div>
          <p class="muted">
            {{ project ? `${project.code} · ${project.title}` : '项目 ' + round.project_id.slice(0, 8) }}
            <template v-if="galleryTitle"> · {{ galleryTitle }}</template>
            · 截止 {{ deadlineLabel(round.due_at) }}
          </p>
          <p class="muted small">
            客户邮箱：{{ round.client_email || '未登记' }}
            <template v-if="round.change_order_id"> · 变更单 {{ round.change_order_id.slice(0, 8) }}</template>
            <template v-if="round.override_reason"> · 放行原因：{{ round.override_reason }}</template>
          </p>
        </div>
        <div class="detail-actions">
          <button
            v-if="isApproved && canOperate"
            class="btn"
            type="button"
            :disabled="deliveryBusy"
            @click="openDelivery"
          >
            {{ deliveryBusy ? '打开中…' : '交付下载' }}
          </button>
          <button
            v-if="!isApproved && canOperate"
            class="btn primary"
            type="button"
            :disabled="notifyBusy"
            @click="notifyEmail"
          >
            {{ notifyBusy ? '发送中…' : '发送邀请邮件' }}
          </button>
          <button class="btn ghost" type="button" :disabled="loading" @click="load">刷新</button>
        </div>
      </header>

      <p v-if="projectClosed" class="alert warning" role="status">项目已关闭。审片记录保留供查看，不能继续通知客户、修图或修改轮次。</p>

      <div class="stat-strip four">
        <div class="stat">
          <span class="stat-value">{{ round.asset_count }}</span>
          <span class="stat-label">照片</span>
        </div>
        <div class="stat">
          <span class="stat-value accent">{{ round.open_comments }}</span>
          <span class="stat-label">未解决批注 / {{ round.comment_count }}</span>
        </div>
        <div class="stat">
          <span class="stat-value">{{ activeTokens.length }}</span>
          <span class="stat-label">有效链接</span>
        </div>
        <div class="stat wide">
          <span class="stat-value small-value">
            {{ round.retouch ? `${PROVIDERS[round.retouch.provider] ?? round.retouch.provider} · ${round.retouch.status}` : '尚未接单' }}
          </span>
          <span class="stat-label">修图任务</span>
        </div>
      </div>

      <nav class="tabs">
        <button type="button" class="tab" :class="{ active: activeTab === 'overview' }" @click="activeTab = 'overview'">
          概览与链接
        </button>
        <button type="button" class="tab" :class="{ active: activeTab === 'assets' }" @click="activeTab = 'assets'">
          素材与批注 · {{ round.asset_count }}
        </button>
        <button type="button" class="tab" :class="{ active: activeTab === 'retouch' }" @click="activeTab = 'retouch'">
          修图
        </button>
      </nav>

      <div v-if="activeTab === 'overview'" class="tab-panel">
        <div class="panel">
          <h2 class="panel-title">状态时间线</h2>
          <ol class="proof-timeline">
            <li v-for="step in timeline" :key="step.key" :class="step.state">
              <span class="timeline-dot"></span>
              <div>
                <strong>{{ step.label }}</strong>
                <div class="muted small">{{ step.at ? formatDateTime(step.at) : step.state === 'current' ? '进行中' : '—' }}</div>
              </div>
            </li>
          </ol>
        </div>

        <div class="panel">
          <h2 class="panel-title">截止时间</h2>
          <form class="inline-form" @submit.prevent="extend">
            <label class="field">
              <span>审片截止</span>
              <input v-model="dueDraft" type="datetime-local" :disabled="isApproved || !canOperate" />
            </label>
            <button class="btn" type="submit" :disabled="extending || isApproved || !canOperate">
              {{ extending ? '保存中…' : '更新截止' }}
            </button>
          </form>
          <p v-if="extendError" class="form-error" role="alert">{{ extendError }}</p>
          <p v-if="isApproved" class="muted small">轮次已通过，不能再修改截止时间。</p>
        </div>

        <div class="panel">
          <h2 class="panel-title">审片链接</h2>
          <form class="token-form" @submit.prevent="createToken">
            <label class="field">
              <span>备注</span>
              <input v-model="tokenLabel" type="text" maxlength="80" placeholder="例如：客户手机" :disabled="isApproved || !canOperate" />
            </label>
            <label class="field">
              <span>访问次数上限（0 为不限）</span>
              <input v-model="tokenMaxVisits" type="number" min="0" step="1" placeholder="不限" :disabled="isApproved || !canOperate" />
            </label>
            <label class="field">
              <span>过期时间（默认为截止后 7 天）</span>
              <input v-model="tokenExpires" type="datetime-local" :disabled="isApproved || !canOperate" />
            </label>
            <button class="btn primary" type="submit" :disabled="tokenBusy || isApproved || !canOperate">
              {{ tokenBusy ? '生成中…' : '生成链接' }}
            </button>
          </form>
          <p v-if="tokenError" class="form-error" role="alert">{{ tokenError }}</p>
          <p class="muted small token-hint">
            明文链接仅在生成后显示一次，数据库只保存哈希；审片链接独立于选片截止，可单独撤销。
          </p>

          <table class="data compact">
            <thead>
              <tr>
                <th>备注</th>
                <th>状态</th>
                <th class="num">访问次数</th>
                <th>过期时间</th>
                <th>最近使用</th>
                <th>创建时间</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="token in round.tokens" :key="token.id">
                <td>{{ token.label || '未命名' }}</td>
                <td><span class="pill" :class="tokenState(token).tone">{{ tokenState(token).label }}</span></td>
                <td class="num">{{ token.visits }}<span class="muted"> / {{ token.max_visits || '∞' }}</span></td>
                <td>{{ formatDateTime(token.expires_at) }}</td>
                <td>{{ formatDateTime(token.last_used_at) }}</td>
                <td>{{ formatDateTime(token.created_at) }}</td>
                <td class="row-action">
                  <button
                    v-if="!token.revoked_at"
                    class="btn ghost sm"
                    type="button"
                    @click="revoking = token"
                  >
                    撤销
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
          <p v-if="round.tokens.length === 0" class="muted small">还没有签发链接。</p>
        </div>
      </div>

      <div v-else-if="activeTab === 'assets'" class="tab-panel">
        <p v-if="round.asset_count === 0" class="empty muted">这一轮没有照片。</p>
        <div v-else class="proof-grid">
          <button
            v-for="item in round.items"
            :key="item.asset_id"
            class="proof-card"
            type="button"
            @click="selectedItem = item"
          >
            <span class="proof-thumb">
              <img v-if="item.thumb_url" :src="item.thumb_url" :alt="item.filename" loading="lazy" />
              <span v-else class="thumb-empty muted">无预览</span>
              <span v-if="item.is_video" class="video-badge">视频</span>
              <span v-if="item.open_comments > 0" class="comment-badge">{{ item.open_comments }}</span>
            </span>
            <span class="proof-card-meta">
              <span class="proof-card-name" :title="item.filename">{{ item.filename }}</span>
              <span class="proof-card-badges">
                <span v-if="revisionLabel(item)" class="pill" :class="revisionLabel(item)!.tone">
                  {{ revisionLabel(item)!.text }}
                </span>
                <span v-if="item.comments > 0" class="muted small">
                  批注 {{ item.comments }}
                </span>
              </span>
            </span>
          </button>
        </div>
        <p class="muted small">点击任意照片查看带水印大图、批注叠加与评论线程；员工可将批注标记为已解决。</p>
      </div>

      <div v-else class="tab-panel">
        <div class="panel">
          <h2 class="panel-title">修图任务</h2>
          <p v-if="round.status === 'open' || round.status === 'in_review'" class="notice">
            客户尚未提出修改。客户提交「提出修改」后才能接单开始修图。
          </p>
          <template v-else-if="round.status === 'changes_requested'">
            <p class="notice">
              客户已提交修改意见。接单后客户不能新增批注；回传后可继续复核和提出修改。
            </p>
            <button class="btn primary" type="button" :disabled="retouchBusy || !canOperate" @click="beginRetouch">
              {{ retouchBusy ? '接单中…' : '接单开始修图' }}
            </button>
          </template>
          <template v-else-if="round.status === 'retouching'">
            <div class="derivative-bar" :class="{ warn: missingItems.length > 0 }">
              <div class="derivative-copy">
                <strong>已导入本轮版本 {{ readyCount }} / {{ retouchItems.length }} 张</strong>
                <span class="muted small">
                  {{ PROVIDERS[round.retouch?.provider ?? 'manual'] ?? round.retouch?.provider }}
                  <template v-if="round.retouch?.started_at"> · 开始于 {{ formatDateTime(round.retouch.started_at) }}</template>
                  <template v-if="round.retouch?.provider === 'folder_watch'">
                    · 源文件已导出到监控目录，修图方回传 JPEG 后自动导入
                  </template>
                </span>
              </div>
              <div class="derivative-actions">
                <input
                  ref="bulkInput"
                  class="hidden-input"
                  type="file"
                  multiple
                  accept="image/jpeg,image/png,image/webp"
                  @change="onBulkPicked"
                />
                <input
                  ref="singleInput"
                  class="hidden-input"
                  type="file"
                  accept="image/jpeg,image/png,image/webp"
                  @change="onSinglePicked"
                />
                <button class="btn sm" type="button" :disabled="uploading || !canOperate" @click="openBulkPicker">
                  {{ uploading ? '上传中…' : '批量上传修图版本' }}
                </button>
                <button
                  class="btn primary sm"
                  type="button"
                  :disabled="uploading || resubmitBusy || missingItems.length > 0 || !canOperate"
                  :title="missingItems.length > 0 ? '仍有素材缺少本轮修图版本' : '回传后客户只能通过'"
                  @click="resubmitAsk = true"
                >
                  回传客户确认
                </button>
              </div>
            </div>

            <p v-if="missingItems.length > 0" class="alert warn">
              还有 {{ missingItems.length }} 张缺少本轮可用的修图版本，回传会被拒绝。可批量上传同名文件，或在下方逐张上传。
            </p>

            <table class="data compact">
              <thead>
                <tr>
                  <th>照片</th>
                  <th>本轮版本</th>
                  <th>批注</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="item in round.items" :key="item.asset_id" :class="{ missing: !readyThisCycle(item) }">
                  <td>
                    <div class="row-media">
                      <span class="row-thumb">
                        <img v-if="item.thumb_url" :src="item.thumb_url" :alt="item.filename" loading="lazy" />
                      </span>
                      <div>
                        <strong>{{ item.filename }}</strong>
                        <div class="muted small">#{{ item.position }} · {{ item.width }}×{{ item.height }}</div>
                      </div>
                    </div>
                  </td>
                  <td>
                    <span v-if="readyThisCycle(item)" class="pill ok">已就绪 v{{ item.revision_no }}</span>
                    <span v-else-if="item.revision_state === 'uploaded'" class="pill warn">派生中 v{{ item.revision_no }}</span>
                    <span v-else-if="item.revision_state === 'failed'" class="pill danger">派生失败 v{{ item.revision_no }}</span>
                    <span v-else class="pill warn">缺失</span>
                  </td>
                  <td class="muted small">{{ item.open_comments }} / {{ item.comments }}</td>
                  <td class="row-action">
                    <button class="btn ghost sm" type="button" :disabled="uploading || !canOperate" @click="openSinglePicker(item)">
                      上传
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>

            <div v-if="uploadEntries.length > 0" class="upload-log">
              <h3 class="sub-title">本次上传</h3>
              <ul>
                <li v-for="entry in uploadEntries" :key="entry.key" :class="entry.status">
                  <span>{{ entry.name }}</span>
                  <span class="muted small">{{ entry.message || '等待中…' }}</span>
                </li>
              </ul>
            </div>
            <p class="muted small">
              支持 JPEG / PNG / WebP（≤50MB），需为修图后的导出图；RAW/TIFF/PSD 不在本轮范围。重复文件按 SHA-256 幂等处理。
            </p>
          </template>
          <template v-else-if="round.status === 'resubmitted'">
            <p class="notice">修图版本已回传，等待客户复核。客户可通过，或继续提交修改意见。</p>
            <p v-if="round.retouch?.delivered_at" class="muted small">回传于 {{ formatDateTime(round.retouch.delivered_at) }}</p>
          </template>
          <template v-else>
            <p class="notice">客户已通过本轮审片，修图流程结束。</p>
          </template>
        </div>
      </div>

      <ProofLightbox
        v-if="selectedItemLive"
        :round-id="round.id"
        :item="selectedItemLive"
        :can-resolve="canOperate && !isApproved"
        @close="selectedItem = null"
        @changed="load"
      />

      <Teleport to="body">
        <div v-if="oneTime" class="overlay" @click.self="oneTime = null">
          <div class="dialog dialog-wide" role="dialog" aria-modal="true" :aria-label="oneTime.title">
            <h2 class="dialog-title">{{ oneTime.title }}</h2>
            <p class="muted">{{ oneTime.note }}</p>
            <label class="field">
              <span>客户审片链接</span>
              <div class="copy-row">
                <input :value="oneTime.url" readonly @focus="($event.target as HTMLInputElement).select()" />
                <button class="btn sm" type="button" @click="copy(oneTime!.url)">复制</button>
              </div>
            </label>
            <label v-if="oneTime.token" class="field">
              <span>令牌（排查问题用，请勿外传）</span>
              <div class="copy-row">
                <input :value="oneTime.token" readonly @focus="($event.target as HTMLInputElement).select()" />
                <button class="btn sm" type="button" @click="copy(oneTime!.token!, '令牌')">复制</button>
              </div>
            </label>
            <div class="dialog-actions">
              <button class="btn primary" type="button" @click="oneTime = null">我已保存</button>
            </div>
          </div>
        </div>
      </Teleport>

      <ConfirmDialog
        :open="revoking !== null"
        title="撤销审片链接"
        message="撤销后该链接立即失效，客户已打开的页面会在下一次请求时被拒绝。"
        confirm-text="撤销链接"
        tone="danger"
        :busy="revokeBusy"
        @cancel="revoking = null"
        @confirm="confirmRevoke"
      />

      <ConfirmDialog
        :open="resubmitAsk"
        title="回传客户确认"
        :message="`将 ${retouchItems.length} 张修图版本回传给客户。回传后客户只能选择通过；再提修改需新建下一轮（可能触发额外轮次费用）。`"
        confirm-text="确认回传"
        :busy="resubmitBusy"
        @cancel="resubmitAsk = false"
        @confirm="confirmResubmit"
      />
    </template>
  </section>
</template>
