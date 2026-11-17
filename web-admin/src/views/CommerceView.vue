<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError } from '../api/client'
import {
	createPaymentOrder,
  createQuote,
  getProject,
	listProjectOrders,
  listProjectQuotes,
  listProjects,
  sendQuote,
  type CommerceProject,
	type PaymentOrder,
  type Quote,
} from '../api/commerce'
import { auth } from '../state/session'
import { toast } from '../state/toast'

const route = useRoute()
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const projects = ref<CommerceProject[]>([])
const projectID = ref(String(route.query.project_id ?? ''))
const quotes = ref<Quote[]>([])
const orders = ref<PaymentOrder[]>([])
const form = reactive({ description: '摄影服务', total: 0, deposit: 0, terms: '' })
const selectedProject = computed(() => projects.value.find((project) => project.id === projectID.value))
const canWrite = computed(() => auth.role === 'owner' || auth.role === 'staff')
const projectClosed = computed(() => selectedProject.value?.state === 'cancelled' || selectedProject.value?.state === 'archived')
const eligibleQuote = computed(() => quotes.value.find((quote) => quote.status === 'sent' || quote.status === 'accepted'))

function fail(reason: unknown, fallback: string): void {
  error.value = reason instanceof ApiError ? reason.detail || reason.message : fallback
}

async function refreshProject(): Promise<void> {
  if (!projectID.value) {
    quotes.value = []
    orders.value = []
    return
  }
  const [quotePage, orderPage] = await Promise.all([
    listProjectQuotes(projectID.value),
    listProjectOrders(projectID.value),
  ])
  quotes.value = quotePage.items ?? []
  orders.value = orderPage.items ?? []
}

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    projects.value = (await listProjects()).items ?? []
    if (projectID.value && !selectedProject.value) {
      const project = await getProject(projectID.value)
      projects.value = [project, ...projects.value]
    }
    if (!projectID.value) projectID.value = projects.value[0]?.id ?? ''
    await refreshProject()
  } catch (reason) {
    fail(reason, '项目与收款数据加载失败')
  } finally {
    loading.value = false
  }
}

watch(projectID, async () => {
  error.value = ''
  try {
    await refreshProject()
  } catch (reason) {
    fail(reason, '项目商务数据加载失败')
  }
})

async function makeQuote(): Promise<void> {
  if (!projectID.value || !form.description.trim() || form.total <= 0 || form.deposit < 0 || form.deposit > form.total) {
    error.value = '请填写服务内容、有效报价和不高于报价的定金'
    return
  }
  saving.value = true
  error.value = ''
  try {
    await createQuote({
      project_id: projectID.value,
      currency: 'CNY',
      deposit_cents: Math.round(form.deposit * 100),
      terms: form.terms.trim(),
      items: [{ kind: 'service', description: form.description.trim(), quantity: 1, unit_price_cents: Math.round(form.total * 100) }],
    })
    await refreshProject()
    toast('报价草稿已创建', 'success')
  } catch (reason) {
    fail(reason, '报价创建失败')
  } finally {
    saving.value = false
  }
}

async function dispatchQuote(quote: Quote): Promise<void> {
  saving.value = true
  error.value = ''
  try {
    await sendQuote(quote)
    await refreshProject()
    toast('报价已发出，客户可在小程序“我的拍摄”查看', 'success')
  } catch (reason) {
    fail(reason, '报价发送失败')
  } finally {
    saving.value = false
  }
}

async function makePaymentOrder(): Promise<void> {
  if (!projectID.value || !eligibleQuote.value) {
    error.value = '请先创建并发送报价'
    return
  }
  saving.value = true
  error.value = ''
  try {
    await createPaymentOrder({
      project_id: projectID.value,
      kind: eligibleQuote.value.deposit_cents > 0 ? 'deposit' : 'full',
    })
    await refreshProject()
    toast('收款订单已创建', 'success')
  } catch (reason) {
    fail(reason, '收款订单创建失败')
  } finally {
    saving.value = false
  }
}

async function copyPayLink(order: PaymentOrder): Promise<void> {
  if (!order.pay_url) return
  try {
    await navigator.clipboard.writeText(order.pay_url)
    toast('收款链接已复制，请发送给客户', 'success')
  } catch {
    error.value = '无法访问剪贴板，请打开收款链接并复制地址'
  }
}

onMounted(load)
</script>

<template>
  <section class="page commerce-page">
    <header class="page-head">
      <div><div class="eyebrow">工作室 / 商务</div><h1>报价与收款</h1></div>
      <select v-model="projectID" aria-label="选择拍摄项目">
        <option value="" disabled>选择项目</option>
        <option v-for="project in projects" :key="project.id" :value="project.id">{{ project.title }} · {{ project.code }}</option>
      </select>
    </header>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="loading" class="empty muted">正在加载项目与收款…</p>
    <template v-else-if="selectedProject">
      <div class="project-context">
        <div><span class="muted">当前项目</span><strong>{{ selectedProject.title }}</strong></div>
        <small>{{ selectedProject.code }} · {{ selectedProject.id }}</small>
      </div>
      <p v-if="projectClosed" class="alert warning" role="status">该项目已关闭，历史报价和收款记录可查看，但不能继续创建报价或收款。</p>
      <section class="commerce-section">
        <div class="section-heading"><div><h2>报价</h2><p class="muted">发送报价后即可按报价创建收款订单；当前不要求合同签署。</p></div></div>
        <div v-if="!quotes.length && !projectClosed" class="quote-form">
          <label class="field"><span>服务内容</span><input v-model="form.description" maxlength="200" /></label>
          <div class="form-grid">
            <label class="field"><span>报价（元）</span><input v-model.number="form.total" type="number" min="0.01" step="0.01" /></label>
            <label class="field"><span>定金（元）</span><input v-model.number="form.deposit" type="number" min="0" step="0.01" /></label>
          </div>
          <label class="field"><span>报价条款</span><textarea v-model="form.terms" rows="3" maxlength="4000" placeholder="付款与服务约定" /></label>
          <button class="btn primary" type="button" :disabled="saving || !canWrite" @click="makeQuote">创建报价</button>
        </div>
        <div v-else-if="quotes.length" class="commerce-list">
          <article v-for="quote in quotes" :key="quote.id" class="commerce-row">
            <div><strong>报价 v{{ quote.version }} · ¥{{ (quote.total_cents / 100).toFixed(2) }}</strong><small>定金 ¥{{ (quote.deposit_cents / 100).toFixed(2) }} · {{ quote.status }}</small></div>
            <button v-if="quote.status === 'draft' && !projectClosed" class="btn primary sm" :disabled="saving || !canWrite" @click="dispatchQuote(quote)">发送报价</button>
            <span v-else-if="quote.status === 'draft'" class="status-label">项目已关闭</span>
            <span v-else class="status-label">{{ quote.status === 'sent' ? '已发送' : quote.status }}</span>
          </article>
        </div>
      </section>

      <section class="commerce-section">
        <div class="section-heading"><div><h2>收款订单</h2><p class="muted">金额由已发送报价计算；定金订单到账后会自动确认预约。</p></div><button v-if="eligibleQuote && !projectClosed" class="btn primary sm" :disabled="saving || !canWrite" @click="makePaymentOrder">创建{{ eligibleQuote.deposit_cents > 0 ? '定金' : '全款' }}订单</button></div>
        <div v-if="!orders.length" class="empty">尚未创建收款订单</div>
        <div v-else class="commerce-list">
          <article v-for="order in orders" :key="order.id" class="commerce-row">
            <div><strong>{{ order.kind === 'deposit' ? '定金' : order.kind === 'full' ? '全款' : order.kind === 'selection_extra' ? '补选精修' : order.kind === 'change' ? '项目变更' : '尾款' }} · ¥{{ (order.amount_cents / 100).toFixed(2) }}</strong><small>{{ order.status === 'pending' ? '待支付' : order.status === 'paid' ? '已支付' : '已关闭' }} · {{ order.id.slice(0, 8) }}</small></div>
            <div v-if="order.status === 'pending'" class="row-actions"><button class="btn ghost sm" type="button" @click="copyPayLink(order)">复制收款链接</button><a class="btn primary sm" :href="order.pay_url" target="_blank" rel="noreferrer">打开收款页</a></div>
            <span v-else class="status-label">{{ order.status === 'paid' ? '收款已入账' : '订单已关闭' }}</span>
          </article>
        </div>
      </section>
      <p class="next-step">付款成功后预约自动确认。合同创建、发送与签署接口仍保留，但当前流程不会要求签署。</p>
    </template>
    <div v-else class="empty">没有可处理的项目。</div>
  </section>
</template>

<style scoped>
.commerce-page { max-width: 1040px; }
.page-head > select { min-width: 280px; max-width: min(440px, 55vw); }
.project-context { display: flex; justify-content: space-between; align-items: end; gap: 16px; padding: 16px 0; border-bottom: 1px solid var(--border); }
.project-context div { display: grid; gap: 5px; }
.project-context strong { font-size: 18px; }
.project-context small, .commerce-row small { color: var(--muted); overflow-wrap: anywhere; }
.commerce-section { padding: 24px 0; border-bottom: 1px solid var(--border); }
.quote-form { max-width: 720px; }
.field { display: grid; gap: 6px; margin: 14px 0; }
.field > span { font-size: 13px; font-weight: 600; }
.form-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
.commerce-list { border-top: 1px solid var(--border); }
.commerce-row { display: flex; align-items: center; justify-content: space-between; gap: 16px; min-height: 66px; padding: 12px 0; border-bottom: 1px solid var(--border); }
.commerce-row > div:first-child { display: grid; gap: 4px; min-width: 0; }
.row-actions { display: flex; align-items: center; gap: 8px; }
.status-label { color: var(--muted); font-size: 13px; }
.next-step { margin-top: 20px; color: var(--muted); font-size: 13px; }
@media (max-width: 680px) {
  .page-head { align-items: stretch; flex-direction: column; }
  .page-head > select { width: 100%; max-width: none; }
  .project-context { align-items: start; flex-direction: column; }
  .form-grid { grid-template-columns: 1fr; gap: 0; }
  .commerce-row { align-items: flex-start; flex-direction: column; }
  .row-actions { align-self: flex-end; }
}
</style>
