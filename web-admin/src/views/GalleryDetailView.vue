<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import type { DeriveResult, Gallery, GalleryDetail, Project } from '../api/types'
import AssetsTab from '../components/AssetsTab.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import ProofCreateDialog from '../components/ProofCreateDialog.vue'
import ResultsTab from '../components/ResultsTab.vue'
import SettingsTab from '../components/SettingsTab.vue'
import TokensTab from '../components/TokensTab.vue'
import { api, auth } from '../state/session'
import { toast } from '../state/toast'
import { deadlineLabel, formatDateTime, statusLabel } from '../utils/format'

const props = defineProps<{ id: string }>()
const router = useRouter()

type TabKey = 'assets' | 'results' | 'tokens' | 'settings'

const gallery = ref<GalleryDetail | null>(null)
const project = ref<Project | null>(null)
const loading = ref(true)
const notFound = ref(false)
const error = ref('')
const activeTab = ref<TabKey>('assets')
const publishing = ref(false)
const publishConfirm = ref(false)
const publishIssue = ref('')
const backfilling = ref(false)
const showProof = ref(false)

const selectedSelections = computed(() =>
  (gallery.value?.selections ?? [])
    .filter((item) => item.decision === 'selected')
    .map((item) => ({ asset_id: item.asset_id, filename: item.filename })),
)
const canStartProof = computed(
  () =>
    Boolean(gallery.value) &&
    gallery.value!.status === 'published' &&
    selectedSelections.value.length > 0 &&
    ['owner', 'staff', 'assistant'].includes(auth.role),
)
const proofHint = computed(() => {
  if (!gallery.value) return ''
  if (gallery.value.status !== 'published') return '发布相册后才能发起审片'
  if (!gallery.value.submitted_at) return '客户提交选片后才能发起审片'
  if (selectedSelections.value.length === 0) return '客户还没有选中的照片'
  return '从客户已选照片发起一轮审片'
})

const tabs = computed(() => {
  if (!gallery.value) return []
  const count = gallery.value.counts
  return [
    { key: 'assets' as TabKey, label: '资产' },
    { key: 'results' as TabKey, label: `选片结果 · ${count.selected}/${count.assets}` },
    { key: 'tokens' as TabKey, label: `分享链接 · ${gallery.value.tokens.filter((t) => !t.revoked_at).length}` },
    { key: 'settings' as TabKey, label: '设置' },
  ]
})

const isDraft = computed(() => gallery.value?.status === 'draft')
const isPublished = computed(() => gallery.value?.status === 'published')
const projectClosed = computed(() => project.value?.state === 'cancelled' || project.value?.state === 'archived')

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const detail = await api<GalleryDetail>(`/api/v1/galleries/${props.id}`)
    gallery.value = detail
    if (!project.value || project.value.id !== detail.project_id) {
      api<Project>(`/api/v1/projects/${detail.project_id}`)
        .then((value) => {
          project.value = value
        })
        .catch(() => {
          // The gallery header falls back to the project id.
        })
    }
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      notFound.value = true
    } else {
      error.value = err instanceof ApiError ? err.detail || err.message : '加载相册失败'
    }
  } finally {
    loading.value = false
  }
}

watch(
  () => props.id,
  () => {
    gallery.value = null
    project.value = null
    publishIssue.value = ''
    notFound.value = false
    activeTab.value = 'assets'
    showProof.value = false
    void load()
  },
  { immediate: true },
)

function refresh(): void {
  void load()
}

async function publish(): Promise<void> {
  if (publishing.value) return
  publishing.value = true
  publishIssue.value = ''
  try {
    await api<Gallery>(`/api/v1/galleries/${props.id}/publish`, { method: 'POST', body: {} })
    publishConfirm.value = false
    toast('相册已发布，可以创建分享链接了', 'success')
    await load()
    activeTab.value = 'tokens'
  } catch (err) {
    publishConfirm.value = false
    if (err instanceof ApiError && err.code === 'derivatives_incomplete') {
      publishIssue.value = err.detail
      activeTab.value = 'results'
      toast('预览图尚未就绪，无法发布', 'error')
      await load()
    } else if (err instanceof ApiError && err.code === 'version_conflict') {
      toast('相册已在别处被修改，已刷新到最新状态', 'error')
      await load()
    } else {
      toast(err instanceof ApiError ? err.detail || err.message : '发布失败', 'error')
    }
  } finally {
    publishing.value = false
  }
}

async function backfill(): Promise<void> {
  if (!gallery.value || backfilling.value) return
  backfilling.value = true
  try {
    const result = await api<DeriveResult>(
      `/api/v1/projects/${gallery.value.project_id}/derivatives:derive`,
      { method: 'POST', body: { asset_ids: [], force: false } },
    )
    toast(`已排队生成 ${result.candidates} 张预览图，worker 完成后请刷新并发布`, 'success')
    publishIssue.value = ''
    await load()
    activeTab.value = 'results'
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '补齐预览失败', 'error')
  } finally {
    backfilling.value = false
  }
}
</script>

<template>
  <section class="page detail">
    <RouterLink to="/galleries" class="back-link">← 相册列表</RouterLink>

    <p v-if="notFound" class="alert error">相册不存在，或已被删除。</p>
    <p v-else-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-else-if="loading && !gallery" class="empty muted">加载中…</p>

    <template v-if="gallery">
      <header class="detail-head">
        <div class="detail-title">
          <div class="title-row">
            <h1>{{ gallery.title }}</h1>
            <span class="pill" :class="gallery.status">{{ statusLabel(gallery.status) }}</span>
            <span v-if="gallery.submitted_at" class="pill ok">客户已提交</span>
          </div>
          <p class="muted">
            {{ project ? `${project.code} · ${project.title}` : '项目 ' + gallery.project_id.slice(0, 8) }}
            · 第 {{ gallery.round_no }} 轮 · 截止 {{ deadlineLabel(gallery.expires_at) }}
            <template v-if="gallery.submitted_at"> · 提交于 {{ formatDateTime(gallery.submitted_at) }}</template>
          </p>
        </div>
        <div class="detail-actions">
          <button
            v-if="isDraft && !projectClosed"
            class="btn primary"
            type="button"
            :disabled="gallery.counts.assets === 0 || publishing"
            :title="gallery.counts.assets === 0 ? '请先把项目资产加入相册' : '发布后资产集合冻结'"
            @click="publishConfirm = true"
          >
            发布相册
          </button>
          <button v-if="isPublished && !projectClosed" class="btn" type="button" @click="activeTab = 'tokens'">创建分享链接</button>
          <button v-if="isPublished" class="btn ghost" type="button" @click="activeTab = 'results'">查看选片结果</button>
          <button
            v-if="isPublished && !projectClosed"
            class="btn primary"
            type="button"
            :disabled="!canStartProof"
            :title="proofHint"
            @click="showProof = true"
          >
            发起审片
          </button>
          <button class="btn ghost" type="button" :disabled="loading" @click="refresh">刷新</button>
        </div>
      </header>

      <p v-if="projectClosed" class="alert warning" role="status">项目已关闭。相册记录保留供查看，不能继续修改资产、发布或签发新的分享链接。</p>

      <div class="stat-strip">
        <div class="stat">
          <span class="stat-value">{{ gallery.counts.assets }}</span>
          <span class="stat-label">资产</span>
        </div>
        <div class="stat">
          <span class="stat-value accent">{{ gallery.counts.selected }}</span>
          <span class="stat-label">已选</span>
        </div>
        <div class="stat">
          <span class="stat-value">{{ gallery.counts.rejected }}</span>
          <span class="stat-label">已弃</span>
        </div>
        <div class="stat">
          <span class="stat-value">{{ gallery.counts.undecided }}</span>
          <span class="stat-label">未决定</span>
        </div>
        <div class="stat wide">
          <span class="stat-value small-value">{{ gallery.selection_limit === null ? '不限' : `${gallery.selection_limit} 张` }}</span>
          <span class="stat-label">选片上限</span>
        </div>
      </div>

      <div v-if="publishIssue" class="alert warn publish-issue">
        <div>
          <strong>无法发布：预览图尚未全部就绪。</strong>
          <p class="small">{{ publishIssue }}</p>
        </div>
        <div class="issue-actions">
          <button class="btn sm" type="button" :disabled="backfilling" @click="backfill">
            {{ backfilling ? '排队中…' : '补齐预览图' }}
          </button>
          <button class="btn ghost sm" type="button" @click="publishIssue = ''">关闭</button>
        </div>
      </div>

      <p v-if="isDraft && gallery.counts.assets === 0" class="notice">
        相册还没有资产。请在「资产」中添加项目照片后再发布。
      </p>

      <nav class="tabs">
        <button
          v-for="tab in tabs"
          :key="tab.key"
          type="button"
          class="tab"
          :class="{ active: activeTab === tab.key }"
          @click="activeTab = tab.key"
        >
          {{ tab.label }}
        </button>
      </nav>

      <div class="tab-body">
        <p v-if="projectClosed" class="empty muted">此相册已进入只读历史状态。</p>
        <AssetsTab v-else-if="activeTab === 'assets'" :gallery="gallery" @changed="refresh" />
        <ResultsTab
          v-else-if="!projectClosed && activeTab === 'results'"
          :key="`results-${gallery.version}-${gallery.selection_version}`"
          :gallery="gallery"
          @changed="refresh"
        />
        <TokensTab v-else-if="!projectClosed && activeTab === 'tokens'" :gallery="gallery" @changed="refresh" />
        <SettingsTab v-else-if="!projectClosed" :gallery="gallery" @changed="refresh" />
      </div>

      <ConfirmDialog
        :open="publishConfirm && !projectClosed"
        title="发布相册"
        :message="`发布后相册的资产集合将冻结（共 ${gallery.counts.assets} 张），客户可凭分享链接开始选片。如需调整资产，需新建下一轮相册。`"
        confirm-text="发布"
        :busy="publishing"
        @cancel="publishConfirm = false"
        @confirm="publish"
      />

      <ProofCreateDialog
        :open="showProof && !projectClosed"
        :project-id="gallery.project_id"
        :gallery-id="gallery.id"
        :gallery-title="gallery.title"
        :selections="selectedSelections"
        @close="showProof = false"
        @created="(id) => router.push({ name: 'proof', params: { id } })"
      />
    </template>
  </section>
</template>
