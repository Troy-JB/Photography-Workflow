<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import { clientGroupKey, isOnDate, listWorkflow, WORKFLOW_STAGES, type WorkflowItem, type WorkflowStage } from '../api/workflow'

const props = defineProps<{ embedded?: boolean; items?: WorkflowItem[] }>()
const router = useRouter()
const loadedItems = ref<WorkflowItem[]>([])
const loading = ref(false)
const hasLoaded = ref(false)
const error = ref('')
const stage = ref<WorkflowStage | ''>('')
const period = ref('all')
const dateBasis = ref<'shoot' | 'order'>('shoot')
const customFrom = ref('')
const customTo = ref('')
const sortBy = ref<'shoot' | 'order'>('shoot')
const ascending = ref(false)

const sourceItems = computed(() => props.items ?? loadedItems.value)

async function load(): Promise<void> {
  if (props.items) return
  if (loading.value) return
  loading.value = !hasLoaded.value
  error.value = ''
  try {
    loadedItems.value = (await listWorkflow()).items ?? []
    hasLoaded.value = true
  } catch (reason) {
    error.value = reason instanceof ApiError ? reason.detail || reason.message : '客户流程加载失败'
  } finally {
    loading.value = false
  }
}

const refreshListener = () => { if (!props.items) void load() }
onMounted(() => {
  void load()
  window.addEventListener('workflow-refresh', refreshListener)
})
onUnmounted(() => window.removeEventListener('workflow-refresh', refreshListener))
watch(() => props.items, (items) => { if (!items) void load() })

function inSelectedPeriod(item: WorkflowItem): boolean {
  const value = dateBasis.value === 'shoot' ? item.shoot_at : item.order_at
  if (period.value === 'all') return true
  if (!value) return false
  const now = new Date()
  const date = new Date(value)
  if (period.value === 'today') return isOnDate(value, now)
  if (period.value === 'tomorrow') {
    const tomorrow = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1)
    return isOnDate(value, tomorrow)
  }
  if (period.value === 'week') {
    const start = new Date(now.getFullYear(), now.getMonth(), now.getDate() - ((now.getDay() + 6) % 7))
    const end = new Date(start.getFullYear(), start.getMonth(), start.getDate() + 7)
    return date >= start && date < end
  }
  if (period.value === 'month') return date.getFullYear() === now.getFullYear() && date.getMonth() === now.getMonth()
  const day = `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`
  return (!customFrom.value || day >= customFrom.value) && (!customTo.value || day <= customTo.value)
}

const filtered = computed(() => sourceItems.value.filter((item) =>
  (!stage.value || item.workflow_stage === stage.value) && inSelectedPeriod(item),
))

const rows = computed(() => {
  const groups = new Map<string, WorkflowItem[]>()
  for (const item of filtered.value) {
    const key = clientGroupKey(item)
    groups.set(key, [...(groups.get(key) ?? []), item])
  }
  const dateOf = (item: WorkflowItem): number => Date.parse(sortBy.value === 'shoot' ? item.shoot_at ?? '' : item.order_at) || 0
  return [...groups.values()].map((group) => {
    group.sort((a, b) => dateOf(a) - dateOf(b))
    return { ...group[group.length - 1], project_count: group.length }
  }).sort((a, b) => (dateOf(a) - dateOf(b)) * (ascending.value ? 1 : -1))
})

function toggleSort(field: 'shoot' | 'order'): void {
  if (sortBy.value === field) ascending.value = !ascending.value
  else {
    sortBy.value = field
    ascending.value = false
  }
}

function dateLabel(value?: string): string {
  if (!value) return '—'
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short', hour12: false }).format(new Date(value))
}

function amount(item: WorkflowItem): string {
  return new Intl.NumberFormat('zh-CN', { style: 'currency', currency: item.currency || 'CNY' }).format(item.amount_cents / 100)
}

function openClient(item: WorkflowItem): void {
  void router.push({ name: 'client-workflow', params: { id: item.id } })
}
</script>

<template>
  <section class="client-list-section">
    <header v-if="!embedded" class="page-head">
      <div><h1>预约客户明细</h1><p class="muted">客户套餐、预约、报价与收款流程</p></div>
      <div class="row-actions"><RouterLink class="btn ghost" to="/schedule">预约与套餐</RouterLink><RouterLink class="btn primary" to="/commerce">报价与收款</RouterLink></div>
    </header>

    <div class="workflow-filters">
      <label><span>日期范围</span><select v-model="period">
        <option value="all">全部日期</option><option value="today">今天</option><option value="tomorrow">明天</option>
        <option value="week">本周</option><option value="month">本月</option><option value="custom">自定义</option>
      </select></label>
      <label><span>日期依据</span><select v-model="dateBasis"><option value="shoot">拍摄时间</option><option value="order">下单时间</option></select></label>
      <label><span>流程状态</span><select v-model="stage"><option value="">全部状态</option><option v-for="item in WORKFLOW_STAGES" :key="item.id" :value="item.id">{{ item.label }}</option><option value="cancelled">已取消</option></select></label>
      <label v-if="period === 'custom'"><span>开始</span><input v-model="customFrom" type="date" /></label>
      <label v-if="period === 'custom'"><span>结束</span><input v-model="customTo" type="date" /></label>
      <button class="btn ghost sm" type="button" :disabled="loading || Boolean(props.items)" @click="load">刷新</button>
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <div class="client-table-wrap">
      <table class="data client-table">
        <thead><tr>
          <th>客户</th><th>流程状态</th><th class="num">交易金额</th>
          <th><button class="table-sort" type="button" @click="toggleSort('shoot')">拍摄时间 <span>{{ sortBy === 'shoot' ? ascending ? '↑' : '↓' : '↕' }}</span></button></th>
          <th><button class="table-sort" type="button" @click="toggleSort('order')">下单时间 <span>{{ sortBy === 'order' ? ascending ? '↑' : '↓' : '↕' }}</span></button></th>
        </tr></thead>
        <tbody>
          <tr v-if="loading"><td colspan="5" class="empty-cell">正在加载客户…</td></tr>
          <tr v-else-if="rows.length === 0"><td colspan="5" class="empty-cell">暂无匹配客户</td></tr>
          <tr v-for="item in rows" :key="clientGroupKey(item)" class="client-row" tabindex="0" @click="openClient(item)" @keydown.enter="openClient(item)">
            <td><div class="client-person"><img :src="item.cover_url || '/favicon.svg'" alt="" /><span><strong>{{ item.client_name || item.title }}</strong><small>{{ item.project_count > 1 ? `${item.project_count} 个项目 · ` : '' }}{{ item.code }}</small></span></div></td>
            <td><span :class="['workflow-badge', `stage-${item.workflow_stage}`]">{{ item.workflow_stage_label }}</span></td>
            <td class="num">{{ amount(item) }}</td><td>{{ dateLabel(item.shoot_at) }}</td><td>{{ dateLabel(item.order_at) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>
