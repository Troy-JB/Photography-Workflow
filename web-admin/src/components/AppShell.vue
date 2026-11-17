<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useRouter } from 'vue-router'
import { Archive, Bell, BriefcaseBusiness, Camera, Images, LayoutDashboard, LogOut, Menu, Settings2, Sparkles, UserRound, X } from '@lucide/vue'
import { api as authedAPI, apiStream, auth, signOut } from '../state/session'

interface LiveNotification {
  id: string
  type: string
  subject_type: string
  subject_id: string
  title: string
  body: string
  created_at: string
  payload: Record<string, unknown>
}

const router = useRouter()
const route = useRoute()
const liveNotification = ref<LiveNotification | null>(null)
const mobileNavOpen = ref(false)
const ordersOpen = ref(false)
const albumsOpen = ref(false)
const ordersActive = computed(() => ['clients', 'client-workflow', 'schedule', 'commerce', 'archives', 'archive'].includes(String(route.name)))
const albumsActive = computed(() => ['galleries', 'gallery', 'proofs', 'proof', 'deliveries', 'delivery'].includes(String(route.name)))
let streamController: AbortController | null = null
const seen = new Set<string>()
const streamStartedAt = Date.now()

watch(ordersActive, (active) => { if (active) ordersOpen.value = true }, { immediate: true })
watch(albumsActive, (active) => { if (active) albumsOpen.value = true }, { immediate: true })
watch(() => route.fullPath, () => { mobileNavOpen.value = false })

function onGlobalKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') mobileNavOpen.value = false
}

async function logout(): Promise<void> {
  await signOut()
  await router.replace({ name: 'login' })
}

function receiveNotification(raw: string): void {
  let notification: LiveNotification
  try { notification = JSON.parse(raw) as LiveNotification } catch { return }
  if (!notification?.id || seen.has(notification.id)) return
  seen.add(notification.id)
  if (notification.type === 'workflow_event') {
    window.dispatchEvent(new CustomEvent('workflow-refresh'))
  }
  liveNotification.value = notification
}

async function pollNotifications(): Promise<void> {
  try {
    const result = await authedAPI<{ items: LiveNotification[] }>('/api/v1/notifications?unread=true&limit=50')
    const fresh = (result.items ?? []).filter((item) => Date.parse(item.created_at) >= streamStartedAt && !seen.has(item.id))
    for (const item of fresh) receiveNotification(JSON.stringify(item))
  } catch { /* SSE reconnect or the next refresh will retry */ }
}

async function wait(ms: number): Promise<void> {
  await new Promise((resolve) => window.setTimeout(resolve, ms))
}

async function readStream(signal: AbortSignal): Promise<void> {
  let lastEventID = ''
  while (!signal.aborted) {
    try {
      const response = await apiStream('/api/v1/notifications/stream', signal, lastEventID ? { 'Last-Event-ID': lastEventID } : {})
      if (!response.body) throw new Error('notification stream has no body')
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      while (!signal.aborted) {
        const { value, done } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        const blocks = buffer.split('\n\n')
        buffer = blocks.pop() ?? ''
        for (const block of blocks) {
          const lines = block.split('\n')
          const id = lines.find((line) => line.startsWith('id: '))?.slice(4)
          const event = lines.find((line) => line.startsWith('event: '))?.slice(7)
          const data = lines.filter((line) => line.startsWith('data: ')).map((line) => line.slice(6)).join('\n')
          if (id) lastEventID = id
          if (event !== 'refresh' && data) receiveNotification(data)
        }
      }
    } catch {
      if (signal.aborted) return
      await pollNotifications()
      await wait(2500)
    }
  }
}

function notificationProject(): string | undefined {
  const value = liveNotification.value
  if (!value) return undefined
  const projectID = value.payload?.project_id
  if (typeof projectID === 'string') return projectID
  return value.subject_type === 'project' ? value.subject_id : undefined
}

function notificationStage(): string | undefined {
  const stage = liveNotification.value?.payload?.workflow_stage
  return typeof stage === 'string' ? stage : undefined
}

async function openNotification(): Promise<void> {
  const current = liveNotification.value
  if (!current) return
  const projectID = notificationProject()
  try { await authedAPI<void>(`/api/v1/notifications/${current.id}/read`, { method: 'POST', body: {} }) } catch { /* navigation remains available */ }
  liveNotification.value = null
  if (projectID) await router.push({ name: 'client-workflow', params: { id: projectID }, query: notificationStage() ? { stage: notificationStage() } : {} })
  else await router.push({ name: 'clients' })
}

async function checkNotifications(): Promise<void> {
  if (liveNotification.value) await openNotification()
  else await pollNotifications()
}

onMounted(() => {
  streamController = new AbortController()
  window.addEventListener('keydown', onGlobalKeydown)
  void readStream(streamController.signal)
})
onBeforeUnmount(() => { streamController?.abort(); window.removeEventListener('keydown', onGlobalKeydown) })
</script>

<template>
  <div class="shell">
    <aside class="side-nav" :class="{ 'mobile-open': mobileNavOpen }">
      <RouterLink to="/" class="brand" aria-label="返回官网首页">
        <span class="brand-mark">P</span>
        <span class="brand-text">
          <strong>摄影工作流</strong>
          <small>管理端 · 选片与审片</small>
        </span>
      </RouterLink>
      <nav class="side-menu" aria-label="主导航">
        <RouterLink to="/workbench" class="side-link"><LayoutDashboard :size="18" /><span>工作台</span></RouterLink>
        <section class="side-group">
          <button class="side-link side-group-trigger" :class="{ 'group-active': ordersActive }" type="button" :aria-expanded="ordersOpen" @click="ordersOpen = !ordersOpen">
            <BriefcaseBusiness :size="18" /><span>订单中心</span><span class="side-chevron" aria-hidden="true">{{ ordersOpen ? '-' : '+' }}</span>
          </button>
          <div v-if="ordersOpen" class="side-submenu">
            <RouterLink to="/clients" class="side-sublink" :class="{ selected: ['clients', 'client-workflow', 'schedule', 'commerce'].includes(String(route.name)) }"><UserRound :size="16" /><span>预约客户明细</span></RouterLink>
            <RouterLink to="/archives" class="side-sublink"><Archive :size="16" /><span>归档</span></RouterLink>
          </div>
        </section>
        <section class="side-group">
          <button class="side-link side-group-trigger" :class="{ 'group-active': albumsActive }" type="button" :aria-expanded="albumsOpen" @click="albumsOpen = !albumsOpen">
            <Images :size="18" /><span>相册中心</span><span class="side-chevron" aria-hidden="true">{{ albumsOpen ? '-' : '+' }}</span>
          </button>
          <div v-if="albumsOpen" class="side-submenu">
            <RouterLink to="/galleries" class="side-sublink"><Camera :size="16" /><span>原片相册</span></RouterLink>
            <RouterLink to="/proofs" class="side-sublink" :class="{ selected: ['proofs', 'proof', 'deliveries', 'delivery'].includes(String(route.name)) }"><Sparkles :size="16" /><span>精修相册</span></RouterLink>
          </div>
        </section>
        <RouterLink to="/leads" class="side-link"><UserRound :size="18" /><span>线索</span></RouterLink>
      </nav>
      <button class="mobile-nav-dismiss" type="button" aria-label="关闭导航" @click="mobileNavOpen = false"><X :size="18" /></button>
    </aside>
    <button v-if="mobileNavOpen" class="mobile-nav-scrim" type="button" aria-label="关闭导航" @click="mobileNavOpen = false" />
    <div class="app-main">
      <header class="topbar">
        <button class="toolbar-icon mobile-nav-toggle" type="button" :aria-expanded="mobileNavOpen" aria-label="打开主导航" @click="mobileNavOpen = !mobileNavOpen"><Menu :size="18" /></button>
        <div class="topbar-spacer" />
        <div class="topbar-actions">
        <RouterLink v-if="auth.role === 'owner'" class="toolbar-icon" to="/settings/retention" aria-label="保留策略" title="保留策略"><Settings2 :size="17" /></RouterLink>
        <button class="toolbar-icon notification-action" type="button" :disabled="!auth.user" :aria-label="liveNotification ? '打开新提醒' : '检查通知'" :title="liveNotification ? '打开新提醒' : '检查通知'" @click="checkNotifications"><Bell :size="17" /><i v-if="liveNotification" /></button>
        <div class="account">
          <RouterLink v-if="!auth.user" class="account-login" to="/login">登录</RouterLink>
          <template v-else>
            <span class="account-avatar" aria-hidden="true">{{ (auth.user.name || auth.user.email || 'P').slice(0, 1).toUpperCase() }}</span>
            <span class="account-meta"><strong>{{ auth.user.name || auth.user.email }}</strong><small>{{ auth.role }}</small></span>
            <button class="toolbar-icon logout-action" type="button" aria-label="退出登录" title="退出登录" @click="logout"><LogOut :size="16" /></button>
          </template>
        </div>
        </div>
      </header>
      <aside v-if="liveNotification" class="live-notification" role="status">
        <div><strong>{{ liveNotification.title }}</strong><p>{{ liveNotification.body }}</p></div>
        <button class="btn primary sm" type="button" @click="openNotification">打开流程</button>
        <button class="btn ghost sm" type="button" aria-label="关闭提醒" @click="liveNotification = null">关闭</button>
      </aside>
      <main class="content"><slot /></main>
    </div>
  </div>
</template>
