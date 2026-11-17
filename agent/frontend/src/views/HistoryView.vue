<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'
import type { AlertEvent, FileRow, ImportDetail, ImportRow, PreviewResult } from '../types'
import AlertList from '../components/AlertList.vue'
import StatusPill from '../components/StatusPill.vue'
import { fileStateLabel, formatBytes, formatTime, statusLabel } from '../format'

const rows = ref<ImportRow[]>([])
const selectedId = ref('')
const detail = ref<ImportDetail | null>(null)
const events = ref<AlertEvent[]>([])
const showEvents = ref(false)
const loading = ref(false)
const busy = ref(false)
const error = ref('')

const previewRow = ref<FileRow | null>(null)
const preview = ref<PreviewResult | null>(null)
const previewLoading = ref(false)
const previewError = ref('')

const selected = computed(() => rows.value.find((r) => r.import.id === selectedId.value) ?? null)

async function load() {
  loading.value = true
  error.value = ''
  try {
    rows.value = (await api.imports(200)) ?? []
    if (selectedId.value && !rows.value.some((r) => r.import.id === selectedId.value)) {
      selectedId.value = ''
      detail.value = null
    }
    if (!selectedId.value && rows.value.length > 0) {
      await select(rows.value[0].import.id)
    } else if (selectedId.value) {
      await select(selectedId.value)
    }
    if (showEvents.value) events.value = (await api.recentEvents(100)) ?? []
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    loading.value = false
  }
}

async function select(id: string) {
  selectedId.value = id
  try {
    detail.value = await api.importDetail(id, 200)
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function resume() {
  if (!selectedId.value) return
  busy.value = true
  error.value = ''
  try {
    await api.runImport(selectedId.value)
    await load()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function ack(id: number) {
  try {
    await api.ackAlert(id)
    await select(selectedId.value)
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function toggleEvents() {
  showEvents.value = !showEvents.value
  if (showEvents.value) {
    try {
      events.value = (await api.recentEvents(100)) ?? []
    } catch (err) {
      error.value = (err as Error).message
    }
  }
}

function copyLabel(state: string): string {
  switch (state) {
    case 'pending':
      return '待拷贝'
    case 'copying':
      return '拷贝中'
    case 'copied':
      return '已写入'
    case 'verified':
      return '校验通过'
    case 'failed':
      return '失败'
    default:
      return state
  }
}

function copyState(row: ImportDetail['files'][number], dest: 'A' | 'B'): string {
  const copy = row.copies.find((c) => c.dest === dest)
  return copy ? copyLabel(copy.state) : '—'
}

function canPreview(row: FileRow): boolean {
  return row.file.kind === 'photo' || row.file.kind === 'video'
}

function mediaMeta(row: FileRow): string {
  const f = row.file
  const dims = f.width && f.height ? `${f.width}×${f.height}` : ''
  return [f.camera, f.lens, f.captured_at ? formatTime(f.captured_at) : '', dims]
    .filter(Boolean)
    .join(' · ')
}

async function openPreview(row: FileRow) {
  if (!canPreview(row)) return
  previewRow.value = row
  preview.value = null
  previewError.value = ''
  previewLoading.value = true
  try {
    preview.value = await api.previewFile(row.file.id)
  } catch (err) {
    previewError.value = (err as Error).message
  } finally {
    previewLoading.value = false
  }
}

function closePreview() {
  previewRow.value = null
  preview.value = null
  previewError.value = ''
}

onMounted(load)
</script>

<template>
  <section class="view">
    <header class="view-head">
      <div>
        <h1>历史与错误</h1>
        <p class="muted">每次导入、每个副本的运行状态都保存在本地 SQLite，可随时查看并继续未完成的导入。</p>
      </div>
      <div class="field-row">
        <button class="btn ghost" :disabled="loading" @click="load">刷新</button>
        <button class="btn ghost" @click="toggleEvents">{{ showEvents ? '隐藏事件日志' : '事件日志' }}</button>
      </div>
    </header>

    <div v-if="error" class="notice error">{{ error }}</div>

    <section v-if="showEvents" class="panel">
      <h2>最近事件</h2>
      <table class="table">
        <thead>
          <tr><th>时间</th><th>级别</th><th>代码</th><th>消息</th></tr>
        </thead>
        <tbody>
          <tr v-for="event in events" :key="event.id">
            <td class="mono nowrap">{{ formatTime(event.created_at) }}</td>
            <td><span class="level" :class="`level-${event.level}`">{{ event.level }}</span></td>
            <td class="mono small">{{ event.code }}</td>
            <td>{{ event.message }}</td>
          </tr>
          <tr v-if="events.length === 0"><td colspan="4" class="muted">暂无事件</td></tr>
        </tbody>
      </table>
    </section>

    <div class="grid history">
      <section class="panel">
        <h2>导入记录</h2>
        <table class="table clickable">
          <thead>
            <tr><th>创建时间</th><th>项目</th><th>日期</th><th>状态</th><th>本地校验</th><th>失败</th></tr>
          </thead>
          <tbody>
            <tr
              v-for="row in rows"
              :key="row.import.id"
              :class="{ selected: row.import.id === selectedId }"
              @click="select(row.import.id)"
            >
              <td class="mono nowrap">{{ formatTime(row.import.created_at) }}</td>
              <td>
                <div>{{ row.import.project_code }}</div>
                <div class="small muted">{{ row.card_label || '手动目录' }}</div>
              </td>
              <td class="mono">{{ row.import.shoot_date }}</td>
              <td>{{ statusLabel(row.import.status) }}</td>
              <td class="mono">{{ row.counts.local_verified }}/{{ row.counts.total }}</td>
              <td class="mono" :class="{ 'err-text': row.counts.failed > 0 }">{{ row.counts.failed }}</td>
            </tr>
            <tr v-if="rows.length === 0"><td colspan="6" class="muted">还没有导入记录</td></tr>
          </tbody>
        </table>
      </section>

      <section v-if="detail" class="panel detail">
        <div class="panel-head">
          <h2>导入详情</h2>
          <StatusPill
            :status="detail.status.import.status"
            :pause-reason="detail.status.import.pause_reason"
          />
        </div>
        <dl class="meta-grid">
          <div><dt>导入 ID</dt><dd class="mono small">{{ detail.status.import.id }}</dd></div>
          <div><dt>项目</dt><dd>{{ detail.status.import.project_code }}</dd></div>
          <div><dt>来源</dt><dd class="mono small">{{ detail.status.import.source_root }}</dd></div>
          <div><dt>目的地 A</dt><dd class="mono small">{{ detail.status.import.dest_a }}</dd></div>
          <div>
            <dt>目的地 B</dt>
            <dd class="mono small">
              {{ detail.status.import.require_backup ? detail.status.import.dest_b : '已关闭（单副本）' }}
            </dd>
          </div>
          <div><dt>文件</dt><dd>{{ detail.status.counts.total }} 个</dd></div>
        </dl>

        <div class="field-row">
          <button
            v-if="detail.status.import.status === 'open'"
            class="btn primary"
            :disabled="busy"
            @click="resume"
          >
            继续导入
          </button>
        </div>

        <template v-if="detail.status.unacknowledged_events?.length">
          <div class="panel-head sub"><h3>未确认提醒</h3></div>
          <AlertList :alerts="detail.status.unacknowledged_events" can-ack @ack="ack" />
        </template>

        <div class="panel-head sub">
          <h3>文件（显示前 {{ detail.files.length }} / {{ detail.total_files }} 个）</h3>
        </div>
        <table class="table files">
          <thead>
            <tr><th>相对路径</th><th>大小</th><th>状态</th><th>A</th><th>B</th><th>预览</th></tr>
          </thead>
          <tbody>
            <tr v-for="row in detail.files" :key="row.file.id">
              <td class="mono small" :title="row.file.rel_path">
                {{ row.file.rel_path }}
                <div v-if="mediaMeta(row)" class="file-meta">{{ mediaMeta(row) }}</div>
                <div v-if="row.file.error" class="err-text small">{{ row.file.error }}</div>
              </td>
              <td class="mono nowrap">{{ formatBytes(row.file.size) }}</td>
              <td class="nowrap">{{ fileStateLabel(row.file.state) }}</td>
              <td class="nowrap">{{ copyState(row, 'A') }}</td>
              <td class="nowrap">
                {{ detail.status.import.require_backup ? copyState(row, 'B') : '未启用' }}
              </td>
              <td class="nowrap">
                <button v-if="canPreview(row)" class="btn tiny" @click="openPreview(row)">查看</button>
                <span v-else class="muted">—</span>
              </td>
            </tr>
          </tbody>
        </table>
      </section>
    </div>

    <div v-if="previewRow" class="modal-backdrop" @click.self="closePreview">
      <section class="modal">
        <header class="panel-head">
          <h2 class="modal-title" :title="previewRow.file.rel_path">{{ previewRow.file.rel_path }}</h2>
          <button class="btn ghost" @click="closePreview">关闭</button>
        </header>
        <p v-if="mediaMeta(previewRow)" class="muted small">{{ mediaMeta(previewRow) }}</p>
        <div v-if="previewLoading" class="muted small">正在生成预览…</div>
        <div v-else-if="previewError" class="notice error">{{ previewError }}</div>
        <img v-else-if="preview" :src="preview.data_url" class="preview-img" alt="预览" />
        <p v-if="preview" class="muted tiny">预览缓存 {{ formatBytes(preview.size) }} · 512px 本地生成，不上传</p>
      </section>
    </div>

    <p v-if="selected && !detail" class="muted">正在加载详情…</p>
  </section>
</template>
