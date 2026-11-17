<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch, type Component } from 'vue'
import { CalendarDays, Camera, Check, CircleCheck, CircleX, Image, Images, WandSparkles } from '@lucide/vue'
import { useRoute } from 'vue-router'
import { ApiError } from '../api/client'
import { listWorkflow, WORKFLOW_STAGES, type WorkflowItem, type WorkflowStage } from '../api/workflow'

const route = useRoute()
const item = ref<WorkflowItem | null>(null)
const loading = ref(true)
const error = ref('')
const viewing = ref<WorkflowStage>('not_shot')
const loaded = ref(false)
const viewingChanged = ref(false)
const currentIndex = computed(() => WORKFLOW_STAGES.findIndex((step) => step.id === item.value?.workflow_stage))
const viewingStep = computed(() => WORKFLOW_STAGES.find((step) => step.id === viewing.value))
const progressPercent = computed(() => currentIndex.value < 0 ? 0 : currentIndex.value / (WORKFLOW_STAGES.length - 1) * 100)
const stageIcons: Record<WorkflowStage, Component> = {
  not_shot: CalendarDays,
  shooting: Camera,
  originals_delivery: Image,
  selecting: Images,
  retouch_delivery: WandSparkles,
  completed: CircleCheck,
  cancelled: CircleX,
}

async function load(): Promise<void> {
  if (!loaded.value) loading.value = true
  error.value = ''
  try {
    const result = await listWorkflow()
    item.value = result.items.find((row) => row.id === String(route.params.id)) ?? null
    const queryStage = String(route.query.stage ?? '') as WorkflowStage
    if (WORKFLOW_STAGES.some((step) => step.id === queryStage)) {
      viewing.value = queryStage
      viewingChanged.value = true
    } else if (!viewingChanged.value) {
      viewing.value = item.value?.workflow_stage === 'cancelled' ? 'not_shot' : item.value?.workflow_stage ?? 'not_shot'
    }
  } catch (reason) {
    error.value = reason instanceof ApiError ? reason.detail || reason.message : '客户流程加载失败'
  } finally {
    loaded.value = true
    loading.value = false
  }
}

onMounted(() => {
  void load()
  window.addEventListener('workflow-refresh', load)
})
onUnmounted(() => window.removeEventListener('workflow-refresh', load))
watch(() => route.params.id, () => {
  loaded.value = false
  viewingChanged.value = false
  void load()
})
watch(() => route.query.stage, (value) => {
  const stage = String(value ?? '') as WorkflowStage
  if (WORKFLOW_STAGES.some((step) => step.id === stage)) {
    viewing.value = stage
    viewingChanged.value = true
  }
})

const context = computed(() => {
  const current = item.value
  if (!current) return ''
  switch (viewing.value) {
    case 'not_shot': return current.payment_status === 'unpaid' ? '等待客户确认报价并支付定金。' : '预约与报价信息。'
    case 'shooting': return `收款状态：${current.payment_status}。${current.shoot_at ? `拍摄时间：${new Date(current.shoot_at).toLocaleString('zh-CN')}` : '拍摄时间待安排。'}`
    case 'originals_delivery': return current.gallery_id ? '原片相册已发布，等待客户开始选片。' : '原片相册尚未发布。'
    case 'selecting': return current.gallery_submitted ? '客户已提交选片，等待进入精修。' : '客户正在选片。'
    case 'retouch_delivery': return `精修与交付处理中${current.delivery_status ? `；交付状态：${current.delivery_status}` : ''}。`
    case 'completed': return current.workflow_stage === 'completed' ? '尾款已付且交付已完成。' : '尾款支付并完成交付后，订单进入已完成。'
  }
})

const links: Record<WorkflowStage, { to: string; label: string }> = {
  not_shot: { to: '/commerce', label: '查看报价与收款' },
  shooting: { to: '/schedule', label: '查看预约' },
  originals_delivery: { to: '/galleries', label: '查看原片相册' },
  selecting: { to: '/galleries', label: '查看选片相册' },
  retouch_delivery: { to: '/proofs', label: '查看审片与精修' },
  completed: { to: '/deliveries', label: '查看交付' },
  cancelled: { to: '/clients', label: '返回客户列表' },
}
</script>

<template>
  <section class="page client-workflow-page">
    <RouterLink to="/clients" class="back-link">← 客户列表</RouterLink>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-else-if="loading" class="muted">正在加载客户流程…</p>
    <p v-else-if="!item" class="muted">未找到该客户流程</p>
    <template v-else>
      <header class="workflow-detail-head"><div><p class="eyebrow">{{ item.code }} · {{ item.title }}</p><h1>{{ item.client_name || item.title }}</h1></div><span :class="['workflow-badge', `stage-${item.workflow_stage}`]">{{ item.workflow_stage_label }}</span></header>
      <div v-if="item.workflow_stage === 'cancelled'" class="cancelled-state">已取消 · 此订单已结束</div>
      <section class="workflow-progress">
        <header class="progress-heading"><div><p class="eyebrow">订单进度</p><h2>服务流程</h2></div><div class="progress-count"><strong>{{ currentIndex < 0 ? '—' : String(currentIndex + 1).padStart(2, '0') }}</strong><span>/ 06</span></div></header>
        <nav class="progress-track" aria-label="客户服务进度">
          <div class="progress-rail" aria-hidden="true"><i :style="{ width: `${progressPercent}%` }" /></div>
          <button v-for="(step, index) in WORKFLOW_STAGES" :key="step.id" type="button" :class="['progress-step', { reached: currentIndex >= index && item.workflow_stage !== 'cancelled', current: currentIndex === index && item.workflow_stage !== 'cancelled', viewing: viewing === step.id }]" :aria-current="currentIndex === index && item.workflow_stage !== 'cancelled' ? 'step' : undefined" :aria-pressed="viewing === step.id" @click="viewing = step.id; viewingChanged = true">
            <span class="step-marker"><Check v-if="currentIndex > index && item.workflow_stage !== 'cancelled'" :size="15" :stroke-width="2.4" /><component :is="stageIcons[step.id]" v-else :size="16" :stroke-width="1.8" /></span>
            <span class="step-label"><small>{{ String(index + 1).padStart(2, '0') }}</small><strong>{{ step.label }}</strong></span>
          </button>
        </nav>
      </section>
      <section class="workflow-stage-panel"><header><div><p class="eyebrow">正在查看</p><h2>{{ viewingStep?.label }}</h2></div><span v-if="viewing === item.workflow_stage" class="current-step-label"><span />当前环节</span></header><p>{{ context }}</p>
        <dl class="workflow-facts"><div><dt>交易金额</dt><dd>{{ item.currency }} {{ (item.amount_cents / 100).toFixed(2) }}</dd></div><div><dt>拍摄时间</dt><dd>{{ item.shoot_at ? new Date(item.shoot_at).toLocaleString('zh-CN') : '未安排' }}</dd></div><div><dt>下单时间</dt><dd>{{ new Date(item.order_at).toLocaleString('zh-CN') }}</dd></div></dl>
        <RouterLink class="btn ghost" :to="links[viewing].to">{{ links[viewing].label }}</RouterLink>
      </section>
    </template>
  </section>
</template>
