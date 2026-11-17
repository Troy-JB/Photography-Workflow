<script setup lang="ts">
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { api, onUpdateEvent } from '../api'
import type { DestinationInfo, Health, SelftestReport, Settings, UpdateState } from '../types'
import { formatBytes } from '../format'

defineProps<{ health: Health | null }>()
const emit = defineEmits<{ saved: [] }>()

const form = reactive<Settings>({
  dest_a: '',
  dest_b: '',
  require_backup: true,
  workers: 2,
  project_code: '',
  exiftool_path: '',
  ffmpeg_path: '',
})
const checks = reactive<{ a: DestinationInfo | null; b: DestinationInfo | null }>({ a: null, b: null })
const selftestReport = ref<SelftestReport | null>(null)
const saving = ref(false)
const testing = ref(false)
const saved = ref('')
const error = ref('')

const update = ref<UpdateState | null>(null)
const updateBusy = ref(false)
const updateFeedback = ref('')
const updateError = ref('')
let offUpdate: (() => void) | undefined

onMounted(async () => {
  try {
    const settings = await api.getSettings()
    Object.assign(form, settings)
  } catch (err) {
    error.value = (err as Error).message
  }
  try {
    update.value = await api.updateStatus()
  } catch (err) {
    update.value = null
    updateError.value = (err as Error).message
  }
  offUpdate = onUpdateEvent(() => {
    void refreshUpdate()
  })
})

onUnmounted(() => offUpdate?.())

async function refreshUpdate() {
  try {
    update.value = await api.updateStatus()
  } catch {
    // Snapshot polling still covers the failure; do not spam the page.
  }
}

async function checkUpdate() {
  updateBusy.value = true
  updateFeedback.value = ''
  updateError.value = ''
  try {
    update.value = await api.checkUpdate()
    if (update.value.message) updateFeedback.value = update.value.message
  } catch (err) {
    updateError.value = (err as Error).message
  } finally {
    updateBusy.value = false
  }
}

async function downloadUpdate() {
  updateBusy.value = true
  updateFeedback.value = ''
  updateError.value = ''
  try {
    update.value = await api.downloadUpdate()
  } catch (err) {
    updateError.value = (err as Error).message
  } finally {
    updateBusy.value = false
  }
}

async function installUpdate() {
  const version = update.value?.latest || '新版本'
  if (!window.confirm(`现在安装 ${version}？安装程序启动后应用会自动退出，导入任务请先完成或暂停。`)) {
    return
  }
  updateBusy.value = true
  updateFeedback.value = ''
  updateError.value = ''
  try {
    update.value = await api.installUpdate()
    if (update.value.message) updateFeedback.value = update.value.message
  } catch (err) {
    updateError.value = (err as Error).message
  } finally {
    updateBusy.value = false
  }
}

function channelLabel(channel?: string) {
  return channel === 'stable' ? 'stable（正式）' : 'dev（测试）'
}

function formatChecked(iso?: string) {
  if (!iso) return '尚未检查'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

async function save() {
  saving.value = true
  saved.value = ''
  error.value = ''
  try {
    await api.saveSettings({ ...form })
    saved.value = '设置已保存'
    emit('saved')
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    saving.value = false
  }
}

async function browse(which: 'a' | 'b') {
  try {
    const picked = await api.pickDirectory(
      which === 'a' ? '选择目的地 A' : '选择备份目的地 B',
      which === 'a' ? form.dest_a : form.dest_b,
    )
    if (!picked) return
    if (which === 'a') form.dest_a = picked
    else form.dest_b = picked
    checks[which] = null
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function check(which: 'a' | 'b') {
  error.value = ''
  try {
    checks[which] = await api.checkDestination(which === 'a' ? form.dest_a : form.dest_b)
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function runSelftest() {
  testing.value = true
  error.value = ''
  try {
    selftestReport.value = await api.selftest()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    testing.value = false
  }
}
</script>

<template>
  <section class="view">
    <header class="view-head">
      <div>
        <h1>设置</h1>
        <p class="muted">目的地目录、备份策略与运行参数保存在本机；开启备份时每个文件都会写入两份并分别校验。</p>
      </div>
      <button class="btn ghost" :disabled="testing" @click="runSelftest">
        {{ testing ? '自检中…' : '运行自检' }}
      </button>
    </header>

    <div v-if="error" class="notice error">{{ error }}</div>
    <div v-if="saved" class="notice ok">{{ saved }}</div>

    <div class="grid two">
      <section class="panel">        <h2>存储与备份</h2>
        <div class="field">
          <label>目的地 A（主副本）</label>
          <div class="field-row">
            <input v-model="form.dest_a" class="input grow" placeholder="D:\PhotoWorkflow" />
            <button class="btn" @click="browse('a')">浏览…</button>
            <button class="btn ghost" @click="check('a')">检查</button>
          </div>
          <div v-if="checks.a" class="check-result" :class="{ bad: !checks.a.writable }">
            <template v-if="checks.a.writable">
              可写 · 剩余 {{ formatBytes(checks.a.free_bytes) }}
            </template>
            <template v-else>{{ checks.a.error || '不可写' }}</template>
          </div>
        </div>

        <div class="field">
          <label class="checkline">
            <input v-model="form.require_backup" type="checkbox" />
            <span>启用目的地 B（备份副本）</span>
          </label>
          <div v-if="form.require_backup" class="field-row">
            <input v-model="form.dest_b" class="input grow" placeholder="\\NAS\Backup" />
            <button class="btn" @click="browse('b')">浏览…</button>
            <button class="btn ghost" @click="check('b')">检查</button>
          </div>
          <div v-if="form.require_backup && checks.b" class="check-result" :class="{ bad: !checks.b.writable }">
            <template v-if="checks.b.writable">
              可写 · 剩余 {{ formatBytes(checks.b.free_bytes) }}
            </template>
            <template v-else>{{ checks.b.error || '不可写' }}</template>
          </div>
          <p v-if="!form.require_backup" class="small danger-text">
            关闭后每个文件只有一份本地副本，导入界面与事件流会持续显示“未备份”提醒。
          </p>
        </div>

        <div class="field">
          <label>默认项目编号（可留空）</label>
          <input v-model="form.project_code" class="input" placeholder="例如 WK-2026-1024" />
        </div>
        <div class="field">
          <label>拷贝并发（每个目的地）</label>
          <select v-model.number="form.workers" class="input">
            <option v-for="n in 8" :key="n" :value="n">{{ n }}</option>
          </select>
        </div>
      </section>

      <section class="panel">
        <h2>运行环境</h2>
        <dl class="meta-grid single">
          <div><dt>版本</dt><dd>{{ health?.version || '—' }}</dd></div>
          <div><dt>状态数据库</dt><dd class="mono small">{{ health?.db_path || '—' }}</dd></div>
          <div><dt>设置文件</dt><dd class="mono small">{{ health?.settings_path || '—' }}</dd></div>
          <div>
            <dt>状态</dt>
            <dd>
              <span v-if="health?.ok" class="ok-text">正常</span>
              <span v-else class="err-text">{{ health?.error || '未连接' }}</span>
            </dd>
          </div>
        </dl>
        <p class="muted small">
          自检会在临时目录执行迁移、双写三方哈希、拔卡暂停恢复与重启恢复；不会触碰真实项目数据。
        </p>

        <template v-if="selftestReport">
          <div class="panel-head sub">
            <h3>自检结果</h3>
            <span :class="selftestReport.ok ? 'ok-text' : 'err-text'">
              {{ selftestReport.ok ? '全部通过' : '存在失败项' }}
            </span>
          </div>
          <table class="table">
            <thead><tr><th>步骤</th><th>结果</th><th>耗时</th><th>说明</th></tr></thead>
            <tbody>
              <tr v-for="step in selftestReport.steps" :key="step.name">
                <td>{{ step.name }}</td>
                <td :class="step.ok ? 'ok-text' : 'err-text'">{{ step.ok ? '通过' : '失败' }}</td>
                <td class="mono nowrap">{{ step.duration_ms }} ms</td>
                <td class="small muted">{{ step.detail || '' }}</td>
              </tr>
            </tbody>
          </table>
        </template>
      </section>
    </div>

    <section class="panel">
      <h2>外部工具（EXIF 与预览）</h2>
      <p class="muted small">
        留空时依次查找安装目录下的 tools\exiftool.exe / tools\ffmpeg.exe 与系统 PATH。
        导入完成后的 EXIF 读取与 RAW、视频预览依赖这两个工具；缺失只产生提醒，不影响拷贝与校验。
      </p>
      <div class="tool-grid">
        <div class="field">
          <label>exiftool 路径</label>
          <input v-model="form.exiftool_path" class="input mono" placeholder="留空自动查找，例如 C:\Tools\exiftool.exe" />
        </div>
        <div class="field">
          <label>ffmpeg 路径</label>
          <input v-model="form.ffmpeg_path" class="input mono" placeholder="留空自动查找，例如 C:\Tools\ffmpeg.exe" />
        </div>
      </div>
      <div class="field-row">
        <button class="btn primary" :disabled="saving" @click="save">{{ saving ? '保存中…' : '保存设置' }}</button>
      </div>
    </section>

    <section class="panel">
      <div class="panel-head">
        <div>
          <h2>更新</h2>
          <p class="muted small">
            启动时与每 6 小时自动检查一次；清单使用内置公钥验签，安装包下载后校验 SHA-256，
            stable 通道还要求代码签名与固定发布者一致。
          </p>
        </div>
        <button
          class="btn ghost"
          :disabled="updateBusy || !update?.configured || update?.checking"
          @click="checkUpdate"
        >
          {{ update?.checking ? '检查中…' : '检查更新' }}
        </button>
      </div>

      <p v-if="!update" class="muted small">正在读取更新状态…</p>
      <template v-else>
        <div v-if="!update.configured" class="notice warn">
          此构建未配置更新源（通常为开发构建），无法在线检查或安装更新。
        </div>
        <template v-else>
          <dl class="meta-grid">
            <div><dt>当前版本</dt><dd>{{ update.current }}</dd></div>
            <div><dt>通道</dt><dd>{{ channelLabel(update.channel) }}</dd></div>
            <div><dt>最近检查</dt><dd>{{ formatChecked(update.checked_at) }}</dd></div>
            <div><dt>最新版本</dt><dd>{{ update.latest || '—' }}</dd></div>
          </dl>

          <div v-if="update.unsigned" class="notice warn">
            dev 通道的安装包未做代码签名，仅用于内部测试；正式发布请使用 stable 通道。
          </div>
          <div v-if="update.mandatory && update.available" class="notice warn">
            这是强制更新：服务器已停止支持当前版本 {{ update.current }}，请尽快安装 {{ update.latest }}。
          </div>
          <div v-if="updateFeedback" class="notice ok">{{ updateFeedback }}</div>
          <div v-if="updateError" class="notice error">{{ updateError }}</div>
          <div v-if="update.error" class="notice error">{{ update.error }}</div>

          <template v-if="update.available">
            <div class="panel-head sub">
              <h3>{{ update.latest }} 可用</h3>
              <span class="pill pill-info">新版本</span>
            </div>
            <p v-if="update.notes" class="small update-notes">{{ update.notes }}</p>

            <template v-if="update.downloading">
              <div class="progress">
                <div class="progress-bar" :style="{ width: update.progress + '%' }" />
              </div>
              <div class="progress-line">
                <span>下载中 {{ update.progress }}%</span>
                <span class="mono">{{ formatBytes(update.downloaded_size ?? 0) }}</span>
              </div>
            </template>
            <template v-else-if="update.downloaded">
              <div class="notice ok">
                安装包已下载，SHA-256 校验通过{{ update.unsigned ? '（未签名）' : '，代码签名校验通过' }}；
                大小 {{ formatBytes(update.downloaded_size ?? 0) }}。
              </div>
              <div class="field-row">
                <button
                  class="btn primary"
                  :disabled="updateBusy || update.installing || !update.supported"
                  @click="installUpdate"
                >
                  {{ update.installing ? '正在启动安装程序…' : '安装并重启' }}
                </button>
              </div>
              <p v-if="!update.supported" class="small danger-text">
                当前为开发构建（未按版本目录安装），只能下载，不能安装；请在正式安装的机器上使用。
              </p>
            </template>
            <div v-else class="field-row">
              <button class="btn primary" :disabled="updateBusy" @click="downloadUpdate">
                {{ update.downloading ? '下载中…' : '下载更新' }}
              </button>
            </div>
          </template>
          <p v-else-if="!update.checking" class="muted small">当前已是最新版本。</p>
        </template>
      </template>
    </section>
  </section>
</template>
