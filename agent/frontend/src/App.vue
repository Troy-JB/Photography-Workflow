<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { api, isDesktop, onImportEvent } from './api'
import type { Health, Settings, Snapshot } from './types'
import DashboardView from './views/DashboardView.vue'
import HistoryView from './views/HistoryView.vue'
import SettingsView from './views/SettingsView.vue'
import ServerView from './views/ServerView.vue'

type ViewKey = 'dashboard' | 'history' | 'settings' | 'server'

const views: { key: ViewKey; label: string; hint: string }[] = [
  { key: 'dashboard', label: '导入卡片', hint: '读卡 · 双写 · 校验' },
  { key: 'history', label: '历史记录', hint: '导入与错误' },
  { key: 'settings', label: '设置', hint: '目录与策略' },
  { key: 'server', label: '服务器', hint: '连接与同步' },
]

const view = ref<ViewKey>('dashboard')
const health = ref<Health | null>(null)
const settings = ref<Settings | null>(null)
const snapshot = ref<Snapshot | null>(null)
const bridgeError = ref('')

let timer: number | undefined
let offEvents: (() => void) | undefined

async function refreshHealth() {
  try {
    health.value = await api.health()
  } catch (err) {
    bridgeError.value = (err as Error).message
  }
}

async function loadSettings() {
  try {
    settings.value = await api.getSettings()
  } catch (err) {
    bridgeError.value = (err as Error).message
  }
}

async function refreshSnapshot() {
  if (!isDesktop()) return
  try {
    snapshot.value = await api.snapshot()
  } catch (err) {
    bridgeError.value = (err as Error).message
  }
}

async function onSettingsSaved() {
  await loadSettings()
  await refreshSnapshot()
}

onMounted(async () => {
  if (!isDesktop()) {
    bridgeError.value = '当前页面未运行在 PWF Agent 桌面容器内，所有导入功能不可用。'
    return
  }
  await refreshHealth()
  await loadSettings()
  await refreshSnapshot()
  timer = window.setInterval(refreshSnapshot, 1000)
  offEvents = onImportEvent(refreshSnapshot)
})

onUnmounted(() => {
  if (timer) window.clearInterval(timer)
  offEvents?.()
})
</script>

<template>
  <div class="shell">
    <aside class="sidebar">
      <div class="brand">
        <div class="brand-mark">PWF</div>
        <div>
          <div class="brand-name">PWF Agent</div>
          <div class="brand-sub">采集工作站</div>
        </div>
      </div>
      <nav class="nav">
        <button
          v-for="item in views"
          :key="item.key"
          class="nav-item"
          :class="{ active: view === item.key }"
          @click="view = item.key"
        >
          <span class="nav-label">{{ item.label }}</span>
          <span class="nav-hint">{{ item.hint }}</span>
        </button>
      </nav>
      <div class="sidebar-foot">
        <div class="small muted">版本 {{ health?.version || '—' }}</div>
        <div class="tiny mono muted" :title="health?.db_path || ''">{{ health?.db_path || '' }}</div>
      </div>
    </aside>

    <main class="main">
      <header class="topbar">
        <div class="topbar-left">
          <span v-if="snapshot?.running" class="live"><span class="pulse" />导入进行中</span>
          <span v-else-if="snapshot?.upload.running" class="live"><span class="pulse" />上传进行中</span>
          <span v-else-if="snapshot?.server.info.logged_in" class="muted small">
            已连接 {{ snapshot.server.info.tenant_name || snapshot.server.info.server_url }}
          </span>
          <span v-else class="muted small">空闲</span>
        </div>
        <div class="topbar-right">
          <button
            v-if="snapshot?.update.available"
            class="pill"
            :class="snapshot.update.mandatory ? 'pill-warn' : 'pill-info'"
            @click="view = 'settings'"
          >
            {{ snapshot.update.mandatory ? '必须更新' : '可更新' }} {{ snapshot.update.latest }}
          </button>
          <span v-if="snapshot?.backup_disabled" class="pill pill-warn">
            <span class="dot" />备份已关闭
          </span>
          <button
            class="alerts-badge"
            :class="{ active: (snapshot?.unacked_total ?? 0) > 0 }"
            @click="view = 'history'"
          >
            提醒 {{ snapshot?.unacked_total ?? 0 }}
          </button>
        </div>
      </header>

      <div v-if="bridgeError" class="notice error in-main">{{ bridgeError }}</div>
      <div v-else-if="health && !health.ok" class="notice error in-main">
        本地状态数据库不可用：{{ health.error }}
      </div>

      <DashboardView
        v-if="view === 'dashboard'"
        :snapshot="snapshot"
        :settings="settings"
        @refresh="refreshSnapshot"
      />
      <HistoryView v-else-if="view === 'history'" />
      <SettingsView v-else-if="view === 'settings'" :health="health" @saved="onSettingsSaved" />
      <ServerView v-else :snapshot="snapshot" @refresh="refreshSnapshot" />
    </main>
  </div>
</template>
