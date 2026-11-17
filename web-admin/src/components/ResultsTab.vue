<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import type { Decision, DeriveResult, GalleryAsset, GalleryAssetsPage, GalleryDetail } from '../api/types'
import PagerBar from './PagerBar.vue'
import ConfirmDialog from './ConfirmDialog.vue'
import { api } from '../state/session'
import { toast } from '../state/toast'
import { decisionLabel } from '../utils/format'

const props = defineProps<{ gallery: GalleryDetail }>()
const emit = defineEmits<{ changed: [] }>()

type Filter = 'all' | 'selected' | 'rejected' | 'undecided'

const filter = ref<Filter>('all')
const search = ref('')
const items = ref<GalleryAsset[]>([])
const total = ref(0)
const offset = ref(0)
const limit = 60
const derivMissing = ref(0)
const derivFailed = ref(0)
const loading = ref(false)
const deriving = ref(false)
const error = ref('')
const removing = ref<GalleryAsset | null>(null)
const removeBusy = ref(false)

const editable = computed(() => props.gallery.status === 'draft')
const tabs = computed(() => [
  { key: 'all' as Filter, label: '全部', count: props.gallery.counts.assets },
  { key: 'selected' as Filter, label: '已选', count: props.gallery.counts.selected },
  { key: 'rejected' as Filter, label: '已弃', count: props.gallery.counts.rejected },
  { key: 'undecided' as Filter, label: '未决定', count: props.gallery.counts.undecided },
])

let debounce = 0

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  const params = new URLSearchParams({
    limit: String(limit),
    offset: String(offset.value),
    filter: filter.value,
  })
  if (search.value.trim()) params.set('q', search.value.trim())
  try {
    const page = await api<GalleryAssetsPage>(`/api/v1/galleries/${props.gallery.id}/assets?${params.toString()}`)
    items.value = page.items
    total.value = page.total
    derivMissing.value = page.derivatives.missing
    derivFailed.value = page.derivatives.failed
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '加载选片结果失败'
  } finally {
    loading.value = false
  }
}

watch(filter, () => {
  offset.value = 0
  void load()
})

watch(search, () => {
  window.clearTimeout(debounce)
  debounce = window.setTimeout(() => {
    offset.value = 0
    void load()
  }, 280)
})

function badgeClass(decision: Decision): string {
  return decision === 'selected' ? 'ok' : decision === 'rejected' ? 'warn' : 'muted'
}

async function backfill(): Promise<void> {
  if (deriving.value) return
  deriving.value = true
  try {
    const result = await api<DeriveResult>(
      `/api/v1/projects/${props.gallery.project_id}/derivatives:derive`,
      { method: 'POST', body: { asset_ids: [], force: false } },
    )
    toast(`已排队生成 ${result.candidates} 张预览图，worker 完成后刷新即可发布`, 'success')
    emit('changed')
    await load()
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '补齐预览失败', 'error')
  } finally {
    deriving.value = false
  }
}

async function confirmRemove(): Promise<void> {
  const target = removing.value
  if (!target) return
  removeBusy.value = true
  try {
    await api<void>(`/api/v1/galleries/${props.gallery.id}/assets/${target.asset_id}`, { method: 'DELETE' })
    toast('已从相册移出', 'success')
    removing.value = null
    await load()
    emit('changed')
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '移出失败', 'error')
  } finally {
    removeBusy.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="tab-panel">
    <div class="derivative-bar" :class="{ warn: derivMissing > 0, error: derivFailed > 0 }">
      <div class="derivative-copy">
        <strong v-if="derivMissing === 0 && derivFailed === 0">全部 {{ gallery.counts.assets }} 张预览图已就绪</strong>
        <strong v-else-if="derivMissing > 0">{{ derivMissing }} 张缺少可发布的预览图</strong>
        <strong v-else>预览图生成失败 {{ derivFailed }} 张</strong>
        <span class="muted small">
          <template v-if="derivMissing > 0">缺少预览图时无法发布，可一键补齐；生成由后台 worker 完成。</template>
          <template v-else-if="derivFailed > 0">失败原因见各条目提示，修复后重新补齐并发布。</template>
          <template v-else>缩略图用于列表，带水印大图用于灯箱。</template>
        </span>
      </div>
      <div class="derivative-actions">
        <button class="btn ghost sm" type="button" :disabled="loading" @click="load">刷新状态</button>
        <button v-if="derivMissing > 0 || derivFailed > 0" class="btn sm" type="button" :disabled="deriving" @click="backfill">
          {{ deriving ? '排队中…' : '补齐预览图' }}
        </button>
      </div>
    </div>

    <div class="filter-tabs">
      <button
        v-for="tab in tabs"
        :key="tab.key"
        type="button"
        class="filter-tab"
        :class="{ active: filter === tab.key }"
        @click="filter = tab.key"
      >
        {{ tab.label }} <span class="tab-count">{{ tab.count }}</span>
      </button>
      <div class="spacer"></div>
      <input v-model="search" class="search-input" type="search" placeholder="按文件名筛选" />
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <div class="panel">
      <table class="data compact selection-table">
        <thead>
          <tr>
            <th>照片</th>
            <th>状态</th>
            <th class="num">顺序</th>
            <th>预览</th>
            <th v-if="editable"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="item in items" :key="item.asset_id">
            <td>
              <div class="row-media">
                <span class="row-thumb" :style="item.color && !item.thumb_url ? { background: item.color } : undefined">
                  <img v-if="item.thumb_url" :src="item.thumb_url" :alt="item.filename" loading="lazy" />
                  <span v-else-if="item.lqip" class="lqip" :style="{ backgroundImage: `url(${item.lqip})` }"></span>
                </span>
                <div>
                  <strong>{{ item.filename }}</strong>
                  <div class="muted small">{{ item.width }}×{{ item.height }}<template v-if="item.is_video"> · 视频</template></div>
                </div>
              </div>
            </td>
            <td><span class="pill" :class="badgeClass(item.decision)">{{ decisionLabel(item.decision) }}</span></td>
            <td class="num">{{ item.position }}</td>
            <td>
              <span v-if="item.preview_ready" class="muted small">就绪</span>
              <span v-else class="pill warn" :title="item.derive_error || ''">缺失</span>
            </td>
            <td v-if="editable" class="row-action">
              <button class="btn ghost sm" type="button" @click="removing = item">移出</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="loading" class="empty muted">加载中…</p>
      <p v-else-if="items.length === 0" class="empty muted">
        {{ filter === 'all' ? '相册还没有资产。' : '该分组暂无照片。' }}
      </p>
    </div>

    <PagerBar
      :total="total"
      :limit="limit"
      :offset="offset"
      :busy="loading"
      @change="(next) => { offset = next; load() }"
    />

    <ConfirmDialog
      :open="removing !== null"
      title="移出相册"
      :message="`确定将 ${removing?.filename ?? ''} 从相册中移出？客户的选片记录会一并删除。`"
      confirm-text="移出"
      tone="danger"
      :busy="removeBusy"
      @cancel="removing = null"
      @confirm="confirmRemove"
    />
  </section>
</template>
