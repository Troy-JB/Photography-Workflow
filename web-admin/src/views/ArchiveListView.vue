<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import type { ArchiveSummary } from '../api/archive'
import {
  ARCHIVE_STATUS_LABELS,
  archiveStatusLabel,
  archiveStatusTone,
  listArchives,
} from '../api/archive'
import PagerBar from '../components/PagerBar.vue'
import { bytes, formatDateTime } from '../utils/format'

const router = useRouter()

const archives = ref<ArchiveSummary[]>([])
const total = ref(0)
const offset = ref(0)
const limit = 20
const statusFilter = ref('')
const loading = ref(false)
const error = ref('')

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const page = await listArchives({
      status: statusFilter.value || undefined,
      limit,
      offset: offset.value,
    })
    archives.value = page.items
    total.value = page.total
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '加载归档列表失败'
  } finally {
    loading.value = false
  }
}

watch(statusFilter, () => {
  offset.value = 0
  void load()
})

function openArchive(row: ArchiveSummary): void {
  void router.push({ name: 'archive', params: { id: row.id } })
}

function retentionLabel(row: ArchiveSummary): string {
  if (row.status === 'deleted') return `已于 ${formatDateTime(row.deleted_at)} 删除`
  return row.retention_until ? formatDateTime(row.retention_until) : '—'
}

onMounted(load)
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1>归档</h1>
        <p class="muted">
          项目进入归档状态后逐对象校验存在性与 SHA-256，按租户保留策略计算到期日；到期只告警，不会自动删除。
        </p>
      </div>
      <RouterLink to="/settings/retention" class="btn ghost">保留策略</RouterLink>
    </header>

    <div class="toolbar">
      <label class="inline-field">
        <span>状态</span>
        <select v-model="statusFilter">
          <option value="">全部状态</option>
          <option v-for="(label, value) in ARCHIVE_STATUS_LABELS" :key="value" :value="value">
            {{ label }}
          </option>
        </select>
      </label>
      <button class="btn ghost" type="button" :disabled="loading" @click="load">刷新</button>
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <div class="panel">
      <table class="data">
        <thead>
          <tr>
            <th>项目</th>
            <th>状态</th>
            <th class="num">对象</th>
            <th class="num">已校验</th>
            <th class="num">失败</th>
            <th class="num">总大小</th>
            <th>保留到期</th>
            <th>核验时间</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="row in archives" :key="row.id" class="clickable" @click="openArchive(row)">
            <td>
              <strong>{{ row.project_code }}</strong>
              <div class="muted small">{{ row.project_title }}</div>
            </td>
            <td><span class="pill" :class="archiveStatusTone(row.status)">{{ archiveStatusLabel(row.status) }}</span></td>
            <td class="num">{{ row.object_count || row.verified_count + row.failed_count || '—' }}</td>
            <td class="num">{{ row.verified_count }}</td>
            <td class="num">
              <span v-if="row.failed_count > 0" class="pill danger">{{ row.failed_count }}</span>
              <span v-else class="muted">0</span>
            </td>
            <td class="num">{{ row.total_bytes ? bytes(row.total_bytes) : '—' }}</td>
            <td>{{ retentionLabel(row) }}</td>
            <td>{{ formatDateTime(row.verified_at) }}</td>
            <td class="row-action">
              <button class="btn ghost sm" type="button" @click.stop="openArchive(row)">查看</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="loading" class="empty muted">加载中…</p>
      <p v-else-if="archives.length === 0" class="empty muted">
        还没有归档。项目完成并进入归档状态后会自动创建归档并开始核验。
      </p>
    </div>

    <PagerBar
      :total="total"
      :limit="limit"
      :offset="offset"
      :busy="loading"
      @change="(next) => { offset = next; load() }"
    />
  </section>
</template>
