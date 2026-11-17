<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import type { DeliverySummary } from '../api/delivery'
import {
  DELIVERY_STATUS_LABELS,
  deliveryStatusLabel,
  deliveryStatusTone,
  listDeliveries,
} from '../api/delivery'
import type { ListResponse, Project } from '../api/types'
import PagerBar from '../components/PagerBar.vue'
import { api } from '../state/session'
import { bytes, formatDateTime } from '../utils/format'

const router = useRouter()
const route = useRoute()

const projects = ref<Project[]>([])
const projectCatalog = ref<Project[]>([])
const deliveries = ref<DeliverySummary[]>([])
const total = ref(0)
const offset = ref(0)
const limit = 20
const projectFilter = ref(typeof route.query.project === 'string' ? route.query.project : '')
const statusFilter = ref('')
const loading = ref(false)
const error = ref('')

const projectNames = computed(() => {
  const map = new Map<string, string>()
  for (const project of projectCatalog.value) map.set(project.id, `${project.code} · ${project.title}${project.state === 'cancelled' ? ' · 已取消' : project.state === 'archived' ? ' · 已归档' : ''}`)
  return map
})

function projectName(row: DeliverySummary): string {
  const title = projectNames.value.get(row.project_id) ?? `${row.project_code} · ${row.project_title}`
  return row.project_state === 'cancelled' ? `${title} · 已取消` : row.project_state === 'archived' ? `${title} · 已归档` : title
}

async function loadProjects(): Promise<void> {
  try {
    const [all, selectable] = await Promise.all([
      api<ListResponse<Project>>('/api/v1/projects?limit=200'),
      api<ListResponse<Project>>('/api/v1/projects?limit=200&exclude_closed=true'),
    ])
    projectCatalog.value = all.items
    projects.value = selectable.items
  } catch {
    // The list still renders project code and title from the delivery row.
  }
}

async function loadDeliveries(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const page = await listDeliveries({
      project_id: projectFilter.value || undefined,
      status: statusFilter.value || undefined,
      limit,
      offset: offset.value,
    })
    deliveries.value = page.items
    total.value = page.total
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '加载交付列表失败'
  } finally {
    loading.value = false
  }
}

watch([projectFilter, statusFilter], () => {
  offset.value = 0
  void loadDeliveries()
})

function openDelivery(row: DeliverySummary): void {
  void router.push({ name: 'delivery', params: { id: row.id } })
}

onMounted(async () => {
  await Promise.all([loadProjects(), loadDeliveries()])
})
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1>交付</h1>
        <p class="muted">为已通过的审片生成网页版与原图下载包，签发一次性可见的客户下载链接。</p>
      </div>
      <RouterLink to="/proofs" class="btn ghost">从审片创建交付</RouterLink>
    </header>

    <div class="toolbar">
      <label class="inline-field">
        <span>项目</span>
        <select v-model="projectFilter">
          <option value="">全部项目</option>
          <option v-for="project in projects" :key="project.id" :value="project.id">
            {{ project.code }} · {{ project.title }}
          </option>
        </select>
      </label>
      <label class="inline-field">
        <span>状态</span>
        <select v-model="statusFilter">
          <option value="">全部状态</option>
          <option v-for="(label, value) in DELIVERY_STATUS_LABELS" :key="value" :value="value">
            {{ label }}
          </option>
        </select>
      </label>
      <button class="btn ghost" type="button" :disabled="loading" @click="loadDeliveries">刷新</button>
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <div class="panel">
      <table class="data">
        <thead>
          <tr>
            <th>项目</th>
            <th>轮次</th>
            <th>状态</th>
            <th>尾款</th>
            <th class="num">照片</th>
            <th class="num">总大小</th>
            <th>版本</th>
            <th>就绪时间</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="row in deliveries" :key="row.id" class="clickable" @click="openDelivery(row)">
            <td>
              <strong>{{ row.project_code }}</strong>
              <div class="muted small">{{ projectName(row) }}</div>
            </td>
            <td class="muted">第 {{ row.round_no }} 轮</td>
            <td>
              <span class="pill" :class="deliveryStatusTone(row.status)">{{ deliveryStatusLabel(row.status) }}</span>
              <span v-if="row.status === 'blocked'" class="pill danger" title="退款或未结清导致交付失效">尾款</span>
            </td>
            <td>
              <span class="pill" :class="row.payment_status === 'paid' ? 'ok' : 'warn'">
                {{ row.payment_status === 'paid' ? '已结清' : '待结清' }}
              </span>
            </td>
            <td class="num">{{ row.item_count }}</td>
            <td class="num">{{ row.total_bytes ? bytes(row.total_bytes) : '—' }}</td>
            <td>v{{ row.build_version }}</td>
            <td>{{ formatDateTime(row.ready_at) }}</td>
            <td class="row-action">
              <button class="btn ghost sm" type="button" @click.stop="openDelivery(row)">查看</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="loading" class="empty muted">加载中…</p>
      <p v-else-if="deliveries.length === 0" class="empty muted">
        还没有交付。打开一个已通过的审片轮次，点击「创建交付」。
      </p>
    </div>

    <PagerBar
      :total="total"
      :limit="limit"
      :offset="offset"
      :busy="loading"
      @change="(next) => { offset = next; loadDeliveries() }"
    />
  </section>
</template>
