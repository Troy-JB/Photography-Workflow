<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { api } from '../api'
import type { CardInfo, Project, Settings, Snapshot, StartResult } from '../types'
import AlertList from '../components/AlertList.vue'
import StatusPill from '../components/StatusPill.vue'
import { formatBytes, formatTime, pauseLabel, statusLabel, today } from '../format'

const props = defineProps<{
  snapshot: Snapshot | null
  settings: Settings | null
}>()

const emit = defineEmits<{ refresh: [] }>()

const cards = ref<CardInfo[]>([])
const cardsError = ref('')
const busy = ref(false)
const actionError = ref('')
const notice = ref('')
const existing = ref<StartResult | null>(null)
const backupAck = ref(false)
const projects = ref<Project[]>([])
const projectsError = ref('')
const attachProjectID = ref('')
let seeded = false

const form = reactive({
  source: '',
  project_code: '',
  project_id: '',
  shoot_date: today(),
  dest_a: '',
  dest_b: '',
  require_backup: true,
})

watch(
  () => props.settings,
  (s) => {
    if (!s || seeded) return
    seeded = true
    form.project_code = s.project_code
    form.dest_a = s.dest_a
    form.dest_b = s.dest_b
    form.require_backup = s.require_backup
  },
  { immediate: true },
)

const current = computed(() => props.snapshot?.import)
const running = computed(() => Boolean(props.snapshot?.running))
const counts = computed(
  () =>
    props.snapshot?.counts ?? {
      total: 0,
      local_verified: 0,
      uploaded: 0,
      remote_verified: 0,
      failed: 0,
    },
)
const alerts = computed(() => props.snapshot?.alerts ?? [])
const percent = computed(() =>
  counts.value.total > 0 ? Math.round((counts.value.local_verified / counts.value.total) * 100) : 0,
)
const upload = computed(() => props.snapshot?.upload)
const uploading = computed(() => Boolean(upload.value?.running))
const uploadPercent = computed(() =>
  counts.value.total > 0 ? Math.round((counts.value.remote_verified / counts.value.total) * 100) : 0,
)
const loggedIn = computed(() => Boolean(props.snapshot?.server?.info.logged_in))
const canUpload = computed(() => {
  const im = current.value
  if (!im || !im.project_id || uploading.value || running.value) return false
  return im.status === 'local_complete' || im.status === 'uploading'
})
const paused = computed(() => Boolean(current.value?.pause_reason))
const canPause = computed(() => running.value)
const canResume = computed(() => {
  const im = current.value
  if (!im || running.value) return false
  if (im.status === 'local_complete' || im.status === 'complete') return false
  return im.status === 'open'
})
const existingDone = computed(() => {
  const status = existing.value?.import.status
  return status === 'local_complete' || status === 'complete'
})

async function refreshCards() {
  cardsError.value = ''
  try {
    cards.value = (await api.cards()) ?? []
  } catch (err) {
    cardsError.value = (err as Error).message
  }
}

async function refreshProjects() {
  projectsError.value = ''
  if (!loggedIn.value) {
    projects.value = []
    return
  }
  try {
    projects.value = (await api.projects(100)) ?? []
  } catch (err) {
    projectsError.value = (err as Error).message
  }
}

watch(loggedIn, (value) => {
  if (value) refreshProjects()
  else projects.value = []
})

function chooseProject(id: string) {
  form.project_id = id
  const p = projects.value.find((item) => item.id === id)
  if (p && !form.project_code.trim()) form.project_code = p.code
}

async function attachProject() {
  const im = current.value
  if (!im || !attachProjectID.value) {
    actionError.value = '请选择要关联的服务器项目'
    return
  }
  busy.value = true
  actionError.value = ''
  try {
    await api.attachProject(im.id, attachProjectID.value)
    notice.value = '已关联服务器项目，可以开始上传'
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function startUpload() {
  const im = current.value
  if (!im) return
  busy.value = true
  actionError.value = ''
  notice.value = ''
  try {
    await api.startUpload(im.id)
    notice.value = '已开始上传；上传在后台继续，可随时暂停。'
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function pauseUpload() {
  actionError.value = ''
  try {
    await api.pauseUpload()
    notice.value = '已请求暂停上传，已完成的分片会保留。'
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  }
}

function selectCard(card: CardInfo) {
  form.source = card.root
  actionError.value = ''
}

async function browseSource() {
  try {
    const picked = await api.pickDirectory('选择存储卡或要导入的目录', form.source)
    if (picked) form.source = picked
  } catch (err) {
    actionError.value = (err as Error).message
  }
}

async function browseDest(which: 'a' | 'b') {
  const start = which === 'a' ? form.dest_a : form.dest_b
  try {
    const picked = await api.pickDirectory(which === 'a' ? '选择目的地 A' : '选择备份目的地 B', start)
    if (picked) {
      if (which === 'a') form.dest_a = picked
      else form.dest_b = picked
    }
  } catch (err) {
    actionError.value = (err as Error).message
  }
}

async function start(allowDuplicate: boolean) {
  actionError.value = ''
  notice.value = ''
  if (!form.source.trim()) {
    actionError.value = '请选择存储卡或填写来源目录'
    return
  }
  if (!form.project_code.trim()) {
    actionError.value = '请填写项目编号'
    return
  }
  if (!form.require_backup && !backupAck.value) {
    actionError.value = '备份已关闭：请勾选确认本次导入只有一份本地副本'
    return
  }
  busy.value = true
  try {
    const res = await api.startImport({
      source: form.source.trim(),
      project_code: form.project_code.trim(),
      project_id: form.project_id,
      shoot_date: form.shoot_date,
      dest_a: form.dest_a.trim(),
      dest_b: form.require_backup ? form.dest_b.trim() : '',
      require_backup: form.require_backup,
      allow_duplicate: allowDuplicate,
    })
    if (res.existing) {
      existing.value = res
    } else {
      existing.value = null
      notice.value = `导入已创建：新增 ${res.added} 个文件，正在后台拷贝。`
    }
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function continueExisting() {
  const im = existing.value?.import
  if (!im) return
  busy.value = true
  actionError.value = ''
  try {
    await api.runImport(im.id)
    existing.value = null
    notice.value = '已继续该导入，正在从断点恢复。'
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function pause() {
  actionError.value = ''
  try {
    await api.pauseImport()
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  }
}

async function resume() {
  const im = current.value
  if (!im) return
  busy.value = true
  actionError.value = ''
  try {
    await api.runImport(im.id)
    notice.value = '已继续导入。'
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function ack(id: number) {
  try {
    await api.ackAlert(id)
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  }
}

async function ackAll() {
  try {
    await api.ackAllAlerts()
    emit('refresh')
  } catch (err) {
    actionError.value = (err as Error).message
  }
}

onMounted(() => {
  refreshCards()
  refreshProjects()
})
</script>

<template>
  <section class="view">
    <header class="view-head">
      <div>
        <h1>导入存储卡</h1>
        <p class="muted">读取一次卡片，同时写入 A / B 两份本地副本，逐文件校验 SHA-256 后才算本地完成。</p>
      </div>
      <button class="btn ghost" :disabled="busy" @click="refreshCards(); emit('refresh')">刷新</button>
    </header>

    <div v-if="actionError" class="notice error">{{ actionError }}</div>
    <div v-if="notice" class="notice ok">{{ notice }}</div>
    <div v-if="cardsError" class="notice warn">存储卡检测不可用：{{ cardsError }}</div>

    <div class="grid two">
      <section class="panel">
        <h2>1 · 选择来源</h2>
        <div v-if="cards.length" class="card-list">
          <button
            v-for="card in cards"
            :key="card.fingerprint"
            class="card-item"
            :class="{ active: form.source === card.root }"
            @click="selectCard(card)"
          >
            <span class="card-label">{{ card.label || '未命名存储卡' }}</span>
            <span class="card-meta">{{ card.root }} · {{ formatBytes(card.capacity) }}</span>
            <span class="card-fp">{{ card.fingerprint }}</span>
          </button>
        </div>
        <p v-else class="muted small">未检测到可移动存储卡（读卡器为空或卡未挂载）。也可以直接选择任意目录导入。</p>
        <div class="field-row">
          <input v-model="form.source" class="input grow" placeholder="例如 E:\ 或 D:\DCIM\20261030" />
          <button class="btn" @click="browseSource">浏览…</button>
        </div>
      </section>

      <section class="panel">
        <h2>2 · 导入参数</h2>
        <div class="field">
          <label>项目编号</label>
          <input v-model="form.project_code" class="input" placeholder="例如 WK-2026-1024" />
        </div>
        <div class="field">
          <label>服务器项目（可选）</label>
          <select
            v-if="loggedIn"
            v-model="form.project_id"
            class="input"
            @change="chooseProject(form.project_id)"
          >
            <option value="">不关联（仅本地拷贝）</option>
            <option v-for="p in projects" :key="p.id" :value="p.id">{{ p.code }} · {{ p.title }}</option>
          </select>
          <p v-else class="small muted">登录服务器后可关联项目；关联后本地校验完成会自动上传并等待云端复核。</p>
          <p v-if="projectsError" class="small danger-text">{{ projectsError }}</p>
        </div>
        <div class="field">
          <label>拍摄日期</label>
          <input v-model="form.shoot_date" type="date" class="input" />
        </div>
        <div class="field">
          <label>目的地 A（主副本）</label>
          <div class="field-row">
            <input v-model="form.dest_a" class="input grow" placeholder="D:\PhotoWorkflow" />
            <button class="btn" @click="browseDest('a')">浏览…</button>
          </div>
        </div>
        <div class="field">
          <label class="checkline">
            <input v-model="form.require_backup" type="checkbox" />
            <span>启用目的地 B（备份副本）</span>
          </label>
          <div v-if="form.require_backup" class="field-row">
            <input v-model="form.dest_b" class="input grow" placeholder="\\NAS\Backup 或 E:\Backup" />
            <button class="btn" @click="browseDest('b')">浏览…</button>
          </div>
        </div>
        <label v-if="!form.require_backup" class="checkline danger-text">
          <input v-model="backupAck" type="checkbox" />
          <span>我确认本次导入只有一份本地副本，界面与事件流会持续提示“未备份”</span>
        </label>
      </section>
    </div>

    <section class="panel action-bar">
      <button class="btn primary" :disabled="busy || running" @click="start(false)">开始导入</button>
      <span v-if="running" class="muted small">正在导入：完成或暂停后才能开始新的导入。</span>
      <span v-else class="muted small">拷贝在后台运行，可随时暂停；拔出卡片会自动暂停并保留断点。</span>
    </section>

    <section v-if="existing" class="panel notice-panel">
      <h2>{{ existingDone ? '这张卡的本地导入已完成' : '这张卡已有未完成的导入' }}</h2>
      <p class="muted">
        项目 {{ existing.import.project_code }} · {{ existing.import.shoot_date }} ·
        状态 {{ statusLabel(existing.import.status) }} · 创建于 {{ formatTime(existing.import.created_at) }}
      </p>
      <p class="small muted">
        {{
          existingDone
            ? '两份本地副本已经校验一致，继续该导入没有可执行的工作。只有需要重新校验或换项目目录重新导入时才新建。'
            : '为避免重复资产，默认继续现有导入。只有确认要重新导入时才新建。'
        }}
      </p>
      <div class="field-row">
        <button v-if="!existingDone" class="btn primary" :disabled="busy" @click="continueExisting">
          继续该导入
        </button>
        <button class="btn" :class="{ danger: !existingDone }" :disabled="busy" @click="start(true)">
          仍然新建导入
        </button>
        <button class="btn ghost" :disabled="busy" @click="existing = null">取消</button>
      </div>
    </section>

    <section v-if="current" class="panel">
      <div class="panel-head">
        <h2>当前导入</h2>
        <StatusPill :status="current.status" :pause-reason="current.pause_reason" :running="running" />
      </div>
      <dl class="meta-grid">
        <div><dt>项目</dt><dd>{{ current.project_code }}</dd></div>
        <div><dt>拍摄日期</dt><dd>{{ current.shoot_date }}</dd></div>
        <div><dt>目的地 A</dt><dd class="mono">{{ current.dest_a }}</dd></div>
        <div>
          <dt>目的地 B</dt>
          <dd class="mono">{{ current.require_backup ? current.dest_b : '已关闭（单副本）' }}</dd>
        </div>
      </dl>

      <div class="progress">
        <div class="progress-bar" :class="{ paused }" :style="{ width: percent + '%' }" />
      </div>
      <div class="progress-line">
        <span>{{ percent }}% · {{ counts.local_verified }} / {{ counts.total }} 个文件本地校验通过</span>
        <span v-if="paused" class="warn-text">已暂停：{{ pauseLabel(current.pause_reason) }}</span>
      </div>

      <div class="stat-row">
        <div class="stat"><span class="stat-value">{{ counts.total }}</span><span class="stat-label">文件总数</span></div>
        <div class="stat"><span class="stat-value ok-text">{{ counts.local_verified }}</span><span class="stat-label">本地校验通过</span></div>
        <div class="stat"><span class="stat-value" :class="{ 'err-text': counts.failed > 0 }">{{ counts.failed }}</span><span class="stat-label">失败</span></div>
        <div class="stat"><span class="stat-value">{{ counts.remote_verified }}</span><span class="stat-label">云端确认</span></div>
      </div>

      <p v-if="snapshot?.last_result?.message" class="muted small">{{ snapshot.last_result.message }}</p>

      <div class="field-row">
        <button v-if="canPause" class="btn" @click="pause">暂停导入</button>
        <button v-if="canResume" class="btn primary" :disabled="busy" @click="resume">继续导入</button>
      </div>

      <template v-if="['local_complete', 'uploading', 'complete'].includes(current.status)">
        <div class="panel-head sub">
          <h3>上传到服务器</h3>
          <span v-if="uploading" class="muted small">上传进行中</span>
          <span v-else-if="current.status === 'complete'" class="ok-text small">云端复核完成</span>
        </div>

        <template v-if="current.project_id">
          <div class="progress">
            <div class="progress-bar" :style="{ width: uploadPercent + '%' }" />
          </div>
          <div class="progress-line">
            <span>
              {{ uploadPercent }}% · {{ counts.remote_verified }} / {{ counts.total }} 个文件云端复核通过
            </span>
            <span v-if="upload?.result?.paused" class="warn-text">
              已暂停：{{ pauseLabel(upload.result.reason) }}
            </span>
          </div>
          <div class="stat-row">
            <div class="stat"><span class="stat-value">{{ counts.uploaded }}</span><span class="stat-label">已上传</span></div>
            <div class="stat"><span class="stat-value ok-text">{{ counts.remote_verified }}</span><span class="stat-label">云端复核通过</span></div>
            <div class="stat"><span class="stat-value">{{ upload?.result?.pending ?? 0 }}</span><span class="stat-label">待复核</span></div>
            <div class="stat"><span class="stat-value" :class="{ 'err-text': counts.failed > 0 }">{{ counts.failed }}</span><span class="stat-label">失败</span></div>
          </div>
          <p v-if="upload?.result?.message" class="muted small">{{ upload.result.message }}</p>
          <div v-if="upload?.error" class="notice error">{{ upload.error }}</div>
          <div class="field-row">
            <button v-if="canUpload" class="btn primary" :disabled="busy" @click="startUpload">
              {{ current.status === 'uploading' ? '继续上传' : '开始上传' }}
            </button>
            <button v-if="uploading" class="btn" @click="pauseUpload">暂停上传</button>
            <span v-if="!loggedIn" class="muted small">未登录服务器：请先到“服务器”页面登录。</span>
          </div>
        </template>

        <template v-else>
          <p class="muted small">该导入尚未关联服务器项目。关联后即可上传，服务端会复核每个文件的 SHA-256。</p>
          <div class="field-row">
            <select v-model="attachProjectID" class="input grow">
              <option value="">选择项目…</option>
              <option v-for="p in projects" :key="p.id" :value="p.id">{{ p.code }} · {{ p.title }}</option>
            </select>
            <button class="btn" :disabled="busy || !attachProjectID" @click="attachProject">关联项目</button>
          </div>
          <p v-if="!loggedIn" class="small muted">请先到“服务器”页面登录并刷新项目列表。</p>
        </template>
      </template>

      <template v-if="alerts.length">
        <div class="panel-head sub">
          <h3>需要处理的提醒</h3>
          <button class="btn tiny ghost" @click="ackAll">全部确认</button>
        </div>
        <AlertList :alerts="alerts" can-ack @ack="ack" />
      </template>
    </section>
  </section>
</template>
