<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import type { ServerState, Settings, Snapshot } from '../types'

const props = defineProps<{ snapshot: Snapshot | null }>()
const emit = defineEmits<{ refresh: [] }>()

const state = ref<ServerState | null>(null)
const form = reactive({ server_url: '', tenant_slug: '', bandwidth_kbps: 0 })
const credentials = reactive({ email: '', password: '' })
const busy = ref(false)
const error = ref('')
const notice = ref('')

function seed(s: Settings) {
  form.server_url = s.server_url || ''
  form.tenant_slug = s.tenant_slug || ''
  form.bandwidth_kbps = s.bandwidth_kbps || 0
}

async function load() {
  try {
    const [settings, server] = await Promise.all([api.getSettings(), api.server()])
    seed(settings)
    state.value = server
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function saveConnection() {
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    state.value = await api.saveServerConfig(
      form.server_url.trim(),
      form.tenant_slug.trim(),
      Number(form.bandwidth_kbps) || 0,
    )
    notice.value = '服务器地址已保存'
    emit('refresh')
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function login() {
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    state.value = await api.login(credentials.email.trim(), credentials.password)
    credentials.password = ''
    notice.value = '已登录并完成设备注册'
    emit('refresh')
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function logout() {
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    await api.logout()
    state.value = await api.server()
    notice.value = '已退出登录'
    emit('refresh')
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <section class="view">
    <header class="view-head">
      <div>
        <h1>服务器</h1>
        <p class="muted">登录后，本地校验完成的导入会登记资产并分片上传；断网或退出登录只会暂停，不会丢进度。</p>
      </div>
      <button class="btn ghost" :disabled="busy" @click="load">刷新</button>
    </header>

    <div v-if="error" class="notice error">{{ error }}</div>
    <div v-if="notice" class="notice ok">{{ notice }}</div>
    <div v-if="props.snapshot?.server.error" class="notice warn">
      服务器连接异常：{{ props.snapshot.server.error }}
    </div>

    <div class="grid two">
      <section class="panel">
        <h2>连接</h2>
        <div class="field">
          <label>服务器地址</label>
          <input v-model="form.server_url" class="input mono" placeholder="https://photo.example.com" />
          <p class="small muted">必须使用 https://；只有本机调试允许 http://localhost。</p>
        </div>
        <div class="field">
          <label>租户标识（可留空）</label>
          <input v-model="form.tenant_slug" class="input mono" placeholder="studio" />
          <p class="small muted">同一个邮箱属于多个租户时才需要填写。</p>
        </div>
        <div class="field">
          <label>上传带宽上限（kbit/s，0 表示不限）</label>
          <input v-model.number="form.bandwidth_kbps" type="number" min="0" class="input" />
        </div>
        <div class="field-row">
          <button class="btn primary" :disabled="busy" @click="saveConnection">保存连接</button>
        </div>
      </section>

      <section class="panel">
        <h2>账号</h2>
        <template v-if="state?.info.logged_in">
          <dl class="meta-grid single">
            <div><dt>账号</dt><dd>{{ state.info.email }}</dd></div>
            <div><dt>姓名</dt><dd>{{ state.info.user_name || '—' }}</dd></div>
            <div><dt>租户</dt><dd>{{ state.info.tenant_name || '—' }}（{{ state.info.role }}）</dd></div>
            <div><dt>服务器</dt><dd class="mono small">{{ state.info.server_url }}</dd></div>
          </dl>
          <div v-if="state.device_error" class="notice warn">设备注册失败：{{ state.device_error }}</div>
          <div class="field-row">
            <button class="btn" :disabled="busy" @click="logout">退出登录</button>
          </div>
        </template>
        <template v-else-if="state?.configured">
          <p class="muted small">尚未登录。令牌只保存在本机（Windows 凭据保护），密码不会落盘。</p>
          <div class="field">
            <label>邮箱</label>
            <input v-model="credentials.email" class="input" autocomplete="username" />
          </div>
          <div class="field">
            <label>密码</label>
            <input
              v-model="credentials.password"
              type="password"
              class="input"
              autocomplete="current-password"
              @keyup.enter="login"
            />
          </div>
          <div class="field-row">
            <button class="btn primary" :disabled="busy" @click="login">
              {{ busy ? '登录中…' : '登录' }}
            </button>
          </div>
        </template>
        <p v-else class="muted small">请先在左侧填写并保存服务器地址。</p>
      </section>
    </div>

    <section v-if="state?.info.device_id" class="panel">
      <h2>本机设备</h2>
      <dl class="meta-grid">
        <div><dt>名称</dt><dd>{{ state.info.device_name }}</dd></div>
        <div><dt>设备 ID</dt><dd class="mono small">{{ state.info.device_id }}</dd></div>
        <div><dt>指纹</dt><dd class="mono small">{{ state.info.fingerprint }}</dd></div>
        <div><dt>心跳</dt><dd>每 60 秒，用于服务端显示在线状态</dd></div>
      </dl>
    </section>
  </section>
</template>
