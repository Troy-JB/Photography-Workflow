<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ApiError } from '../api/client'
import type { IntegrationStatus } from '../api/proof'
import { getIntegrationStatus, syncFolderWatch } from '../api/proof'
import { auth } from '../state/session'
import { toast } from '../state/toast'
import { formatDateTime } from '../utils/format'

const status = ref<IntegrationStatus | null>(null)
const loading = ref(false)
const syncing = ref(false)
const error = ref('')

const isOwner = computed(() => auth.role === 'owner')
const isFolderWatch = computed(() => status.value?.provider === 'folder_watch')

const PROVIDERS: Record<string, string> = {
  manual: '手工上传',
  folder_watch: '目录监控（FolderWatch）',
}

const counts = computed(() => {
  if (!status.value) return []
  const rows: { label: string; value: number }[] = []
  for (const [state, n] of Object.entries(status.value.exports)) {
    rows.push({ label: `导出 · ${state}`, value: n })
  }
  for (const [state, n] of Object.entries(status.value.imports)) {
    rows.push({ label: `回传 · ${state}`, value: n })
  }
  return rows
})

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    status.value = await getIntegrationStatus()
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '加载对接状态失败'
  } finally {
    loading.value = false
  }
}

async function sync(): Promise<void> {
  if (syncing.value) return
  syncing.value = true
  try {
    const result = await syncFolderWatch()
    toast(`已扫描回传目录，导入 ${result.imported} 个版本`, 'success')
    await load()
  } catch (err) {
    toast(err instanceof ApiError ? err.detail || err.message : '扫描失败', 'error')
  } finally {
    syncing.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="panel">
    <div class="panel-head">
      <h2 class="panel-title">修图对接</h2>
      <div class="gate-actions">
        <button v-if="isFolderWatch && isOwner" class="btn ghost sm" type="button" :disabled="syncing" @click="sync">
          {{ syncing ? '扫描中…' : '立即扫描回传目录' }}
        </button>
        <button class="btn ghost sm" type="button" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-else-if="loading && !status" class="muted small">加载中…</p>

    <template v-if="status">
      <div class="meta-list integration-meta">
        <div>
          <dt>提供方</dt>
          <dd>{{ PROVIDERS[status.provider] ?? status.provider }}</dd>
        </div>
        <div v-if="status.root">
          <dt>监控目录</dt>
          <dd class="mono">{{ status.root }}</dd>
        </div>
        <div>
          <dt>进行中任务</dt>
          <dd>{{ status.active_tasks }}</dd>
        </div>
        <div>
          <dt>超时任务</dt>
          <dd>{{ status.overdue_tasks }}</dd>
        </div>
        <div v-for="row in counts" :key="row.label">
          <dt>{{ row.label }}</dt>
          <dd>{{ row.value }}</dd>
        </div>
      </div>

      <p v-if="status.provider === 'manual'" class="muted small">
        手工上传模式：在审片详情「修图」页逐个上传修图后的 JPEG/PNG/WebP。
      </p>
      <p v-else-if="isFolderWatch" class="muted small">
        导出目录 <span class="mono">outgoing/&lt;taskID&gt;</span>，回传目录
        <span class="mono">incoming/&lt;taskID&gt;/&lt;assetID&gt;.jpg</span>。说明文件随任务写入。
      </p>

      <template v-if="status.recent_errors.length > 0">
        <h3 class="sub-title">最近失败</h3>
        <table class="data compact">
          <thead>
            <tr>
              <th>任务</th>
              <th>方向</th>
              <th class="num">尝试</th>
              <th>错误</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="job in status.recent_errors" :key="`${job.retouch_task_id}-${job.direction}`">
              <td class="mono">{{ job.retouch_task_id.slice(0, 8) }}</td>
              <td>{{ job.direction === 'export' ? '导出' : '回传' }}</td>
              <td class="num">{{ job.attempts }}</td>
              <td class="muted small">{{ job.last_error }}</td>
              <td>{{ formatDateTime(job.updated_at) }}</td>
            </tr>
          </tbody>
        </table>
      </template>
    </template>
  </div>
</template>
