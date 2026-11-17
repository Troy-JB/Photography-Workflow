<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import type { Gallery, GallerySummary, ListResponse, Project } from '../api/types'
import CreateGalleryDialog from '../components/CreateGalleryDialog.vue'
import PagerBar from '../components/PagerBar.vue'
import { api } from '../state/session'
import { deadlineLabel, formatDateTime, statusLabel } from '../utils/format'

const router = useRouter()
const route = useRoute()

const projects = ref<Project[]>([])
const projectCatalog = ref<Project[]>([])
const galleries = ref<GallerySummary[]>([])
const total = ref(0)
const offset = ref(0)
const limit = 20
const projectFilter = ref(typeof route.query.project === 'string' ? route.query.project : '')
const statusFilter = ref('')
const loading = ref(false)
const error = ref('')
const showCreate = ref(false)

const projectNames = computed(() => {
  const map = new Map<string, string>()
  for (const project of projectCatalog.value) map.set(project.id, `${project.code} · ${project.title}${project.state === 'cancelled' ? ' · 已取消' : project.state === 'archived' ? ' · 已归档' : ''}`)
  return map
})

function projectName(id: string): string {
  return projectNames.value.get(id) ?? id.slice(0, 8)
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
    // The gallery list still works without the filter source.
  }
}

async function loadGalleries(): Promise<void> {
  loading.value = true
  error.value = ''
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset.value) })
  if (projectFilter.value) params.set('project_id', projectFilter.value)
  if (statusFilter.value) params.set('status', statusFilter.value)
  try {
    const page = await api<ListResponse<GallerySummary>>(`/api/v1/galleries?${params.toString()}`)
    galleries.value = page.items
    total.value = page.total
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '加载失败'
  } finally {
    loading.value = false
  }
}

watch([projectFilter, statusFilter], () => {
  offset.value = 0
  void loadGalleries()
})

function openGallery(gallery: Gallery): void {
  void router.push({ name: 'gallery', params: { id: gallery.id } })
}

function onCreated(gallery: Gallery): void {
  showCreate.value = false
  void router.push({ name: 'gallery', params: { id: gallery.id } })
}

onMounted(async () => {
  await Promise.all([loadProjects(), loadGalleries()])
})
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1>原片相册</h1>
        <p class="muted">创建相册、补齐预览图、发布链接并查看客户选片结果。</p>
      </div>
      <button class="btn primary" type="button" :disabled="projects.length === 0" @click="showCreate = true">
        新建相册
      </button>
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
          <option value="draft">草稿</option>
          <option value="published">已发布</option>
          <option value="expired">已过期</option>
          <option value="revoked">已撤销</option>
        </select>
      </label>
      <button class="btn ghost" type="button" :disabled="loading" @click="loadGalleries">刷新</button>
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <div class="panel">
      <table class="data gallery-table">
        <thead>
          <tr>
            <th>相册</th>
            <th>项目</th>
            <th>状态</th>
            <th class="num">资产</th>
            <th>截止</th>
            <th>提交时间</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="gallery in galleries" :key="gallery.id" class="clickable" @click="openGallery(gallery)">
            <td>
              <strong>{{ gallery.title }}</strong>
              <div class="muted small">第 {{ gallery.round_no }} 轮 · 更新于 {{ formatDateTime(gallery.updated_at) }}</div>
            </td>
            <td class="muted">{{ projectName(gallery.project_id) }}</td>
            <td><span class="pill" :class="gallery.status">{{ statusLabel(gallery.status) }}</span></td>
            <td class="num">{{ gallery.asset_count }}</td>
            <td>{{ deadlineLabel(gallery.expires_at) }}</td>
            <td>{{ gallery.submitted_at ? formatDateTime(gallery.submitted_at) : '—' }}</td>
            <td class="row-action">
              <button class="btn ghost sm" type="button" @click.stop="openGallery(gallery)">管理</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="loading" class="empty muted">加载中…</p>
      <p v-else-if="galleries.length === 0" class="empty muted">没有符合条件的相册。</p>
    </div>

    <PagerBar
      :total="total"
      :limit="limit"
      :offset="offset"
      :busy="loading"
      @change="(next) => { offset = next; loadGalleries() }"
    />

    <CreateGalleryDialog
      :open="showCreate"
      :projects="projects"
      :default-project="projectFilter"
      @close="showCreate = false"
      @created="onCreated"
    />
  </section>
</template>
