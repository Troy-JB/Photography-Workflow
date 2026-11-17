<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { CalendarDays, RefreshCw, TrendingUp } from '@lucide/vue'
import type { RouteLocationRaw } from 'vue-router'
import { ApiError } from '../api/client'
import { getWorkbench, isOnDate, listWorkflowNotifications, orderFrequency, type WorkflowItem, type WorkflowNotification } from '../api/workflow'
import { auth } from '../state/session'
import ClientListView from './ClientListView.vue'

const items = ref<WorkflowItem[]>([])
const loading = ref(true)
const error = ref('')
const notifications = ref<WorkflowNotification[]>([])
const frequencyDays = ref<7 | 30 | 90>(7)
const today = new Date()
const tomorrow = new Date(today.getFullYear(), today.getMonth(), today.getDate() + 1)

const todayShoots = computed(() => items.value.filter((item) => item.workflow_stage !== 'cancelled' && isOnDate(item.shoot_at, today)))
const tomorrowShoots = computed(() => items.value.filter((item) => item.workflow_stage !== 'cancelled' && isOnDate(item.shoot_at, tomorrow)))
const dueToday = computed(() => items.value.flatMap((item) => {
  const result: { item: WorkflowItem; label: string }[] = []
  if (item.workflow_stage !== 'cancelled' && item.selection_due_at && isOnDate(item.selection_due_at, today)) result.push({ item, label: '选片截止' })
  if (item.workflow_stage !== 'cancelled' && item.proof_due_at && isOnDate(item.proof_due_at, today)) result.push({ item, label: '审片截止' })
  return result
}))
const notificationTasks = computed(() => {
  const actionableTypes = new Set([
    'shoot_reminder', 'hold_expiry', 'auth_incomplete', 'gallery_deadline', 'gallery_submitted',
    'proof_changes_requested', 'proof_deadline', 'retouch_delivered', 'delivery_ready',
  ])
  return notifications.value.filter((notice) => {
    if (!isOnDate(notice.due_at, today)) return false
    const projectID = typeof notice.payload.project_id === 'string'
      ? notice.payload.project_id
      : notice.subject_type === 'project' ? notice.subject_id : ''
    const project = items.value.find((item) => item.id === projectID)
    if (project && (project.workflow_stage === 'completed' || project.workflow_stage === 'cancelled')) return false
    if (notice.type === 'workflow_event') return notice.payload.event === 'booking_created'
    return actionableTypes.has(notice.type)
  })
})
const frequency = computed(() => orderFrequency(items.value, frequencyDays.value, new Date()))
const maxFrequency = computed(() => Math.max(1, ...frequency.value.map((item) => item.count)))
const frequencyTotal = computed(() => frequency.value.reduce((total, item) => total + item.count, 0))
const frequencyAverage = computed(() => (frequencyTotal.value / frequencyDays.value).toFixed(1))

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const [workbench, notices] = await Promise.all([getWorkbench(), listWorkflowNotifications()])
    items.value = workbench.items ?? []
    notifications.value = notices.items ?? []
  } catch (reason) {
    error.value = reason instanceof ApiError ? reason.detail || reason.message : '工作台加载失败'
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
  window.addEventListener('workflow-refresh', load)
})
onUnmounted(() => window.removeEventListener('workflow-refresh', load))

function dateLabel(value?: string): string {
  if (!value) return '时间待定'
  return new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit' }).format(new Date(value))
}

function notificationTarget(notice: WorkflowNotification): RouteLocationRaw {
  if (notice.subject_type !== 'project') return '/clients'
  const stage = notice.payload.workflow_stage
  return { name: 'client-workflow', params: { id: notice.subject_id }, query: typeof stage === 'string' ? { stage } : {} }
}
</script>

<template>
  <section class="page workbench-page">
    <header class="page-head workbench-head">
      <div>
        <p class="eyebrow">工作室 / 今日</p>
        <h1>欢迎回来，{{ auth.user?.name || '摄影师' }}</h1>
        <p class="muted">{{ new Intl.DateTimeFormat('zh-CN', { dateStyle: 'full' }).format(today) }}</p>
      </div>
      <button class="btn ghost refresh-action" type="button" :disabled="loading" @click="load"><RefreshCw :size="16" />{{ loading ? '更新中' : '刷新' }}</button>
    </header>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <div class="workbench-layout">
      <section class="plan-column today-plan">
        <header class="plan-heading"><div class="plan-heading-icon"><CalendarDays :size="17" /></div><div><h2>今日计划</h2><p>拍摄与客户节点</p></div><span class="plan-count">{{ todayShoots.length + dueToday.length + notificationTasks.length }}</span></header>
        <h3>拍摄安排</h3>
        <RouterLink v-for="item in todayShoots" :key="item.id" class="plan-row" :to="{ name: 'client-workflow', params: { id: item.id } }"><span><strong>{{ item.client_name || item.title }}</strong><small>{{ item.title }}</small></span><time>{{ dateLabel(item.shoot_at) }}</time></RouterLink>
        <p v-if="!todayShoots.length" class="plan-empty">今天没有拍摄安排</p>
        <h3>今天到期</h3>
        <RouterLink v-for="task in dueToday" :key="`${task.item.id}-${task.label}`" class="plan-row" :to="{ name: 'client-workflow', params: { id: task.item.id }, query: { stage: task.item.workflow_stage } }"><span><strong>{{ task.item.client_name || task.item.title }}</strong><small>{{ task.label }}</small></span><span class="task-state">{{ task.item.workflow_stage_label }}</span></RouterLink>
        <p v-if="!dueToday.length" class="plan-empty">没有今天到期的事项</p>
        <RouterLink v-for="notice in notificationTasks" :key="notice.id" class="plan-row" :to="notificationTarget(notice)"><span><strong>{{ notice.title }}</strong><small>{{ notice.body }}</small></span><span class="task-state">提醒</span></RouterLink>
      </section>
      <div class="workbench-side">
        <section class="plan-column tomorrow-plan">
          <header class="plan-heading"><div class="plan-heading-icon tomorrow-icon"><CalendarDays :size="17" /></div><div><h2>明日计划</h2><p>提前安排拍摄时段</p></div><span class="plan-count">{{ tomorrowShoots.length }}</span></header>
          <h3>拍摄安排</h3>
          <RouterLink v-for="item in tomorrowShoots" :key="item.id" class="plan-row" :to="{ name: 'client-workflow', params: { id: item.id } }"><span><strong>{{ item.client_name || item.title }}</strong><small>{{ item.title }}</small></span><time>{{ dateLabel(item.shoot_at) }}</time></RouterLink>
          <p v-if="!tomorrowShoots.length" class="plan-empty">明天没有拍摄安排</p>
        </section>

        <section class="frequency-section">
      <header class="section-toolbar">
        <div class="frequency-title"><span class="trend-icon"><TrendingUp :size="17" /></span><div><h2>接单趋势</h2><p class="muted">按客户预约创建时间统计</p></div></div>
        <label class="range-select"><span class="sr-only">统计范围</span><select v-model.number="frequencyDays"><option :value="7">近 7 天</option><option :value="30">近 30 天</option><option :value="90">近 90 天</option></select></label>
      </header>
      <div class="frequency-summary"><strong>{{ frequencyTotal }}</strong><span>笔新预约</span><span class="summary-divider" /><span>日均 {{ frequencyAverage }} 笔</span></div>
      <div class="frequency-chart" role="img" :aria-label="`近 ${frequencyDays} 天共 ${frequencyTotal} 笔预约`">
        <div v-for="bucket in frequency" :key="bucket.date.toISOString()" class="frequency-bar" :title="`${bucket.date.toLocaleDateString('zh-CN')} · ${bucket.count} 笔`">
          <span class="bar-count">{{ bucket.count || '' }}</span><i :style="{ height: `${Math.max(3, bucket.count / maxFrequency * 100)}%` }" /><small>{{ bucket.date.getDate() }}</small>
        </div>
      </div>
      <div class="chart-foot"><span>{{ frequency[0]?.date.toLocaleDateString('zh-CN', { month: 'short', day: 'numeric' }) }}</span><span>{{ frequency[frequency.length - 1]?.date.toLocaleDateString('zh-CN', { month: 'short', day: 'numeric' }) }}</span></div>
        </section>
      </div>
    </div>
    <ClientListView embedded :items="items" />
  </section>
</template>
