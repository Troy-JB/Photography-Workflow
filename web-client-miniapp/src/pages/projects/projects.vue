<script setup lang="ts">
import { onShow } from '@dcloudio/uni-app'
import { ref } from 'vue'
import { getSession, request } from '../../api'
import { allowedShareBase, isCustomerShareURL } from '../../share'

interface QuoteItem { kind: string; description: string; quantity: number; unit_price_cents: number; amount_cents: number }
interface Quote { id: string; project_id: string; version: number; status: 'sent' | 'accepted'; currency: string; total_cents: number; deposit_cents: number; valid_until?: string; terms: string; row_version: number; items: QuoteItem[] }
interface Project { id: string; title: string; state: string; studio_name: string; shoot_start_at?: string; shoot_end_at?: string; payment_status: string; currency: string; total_amount_cents: number; deposit_cents: number; balance_cents: number; quotes: Quote[]; live_balance_order?: PaymentOrder }
interface PaymentOrder { id: string; status: string; kind: string; amount_cents: number; pay_url: string }
const session = ref(getSession())
const projects = ref<Project[]>([])
const error = ref('')
const loading = ref(false)
const shareURL = ref('')
const shareError = ref('')
const quoteBusy = ref('')
const paymentBusy = ref('')
const paymentOrders = ref<Record<string, PaymentOrder>>({})

function projectStateLabel(state: string): string {
  const labels: Record<string, string> = {
    draft: '待沟通', quoted: '报价中', contract_pending: '待签约', contracted: '已签约',
    booked: '已预约', shooting: '拍摄中', post_production: '后期制作', completed: '已完成',
    archived: '已归档', on_hold: '暂停', cancelled: '已取消',
  }
  return labels[state] ?? state
}

async function load(): Promise<void> {
  session.value = getSession()
  if (!session.value) return
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    projects.value = (await request<{ items: Project[] }>('/c/v1/me/projects', 'GET', undefined, true)).items ?? []
    for (const project of projects.value) {
      if (project.live_balance_order) paymentOrders.value[project.id] = project.live_balance_order
      else if (paymentOrders.value[project.id]?.kind === 'balance') delete paymentOrders.value[project.id]
    }
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '拍摄项目加载失败'
  } finally {
    loading.value = false
  }
}

async function acceptQuote(quote: Quote): Promise<void> {
  if (quoteBusy.value) return
  quoteBusy.value = quote.id
  try {
    await request(`/c/v1/me/quotes/${quote.id}/accept`, 'POST', { expected_version: quote.row_version }, true)
    uni.showToast({ title: '报价已确认', icon: 'success' })
    await load()
  } catch (reason) {
    const message = reason instanceof Error ? reason.message : ''
    uni.showToast({ title: message.includes('project can no longer accept') ? '该项目已取消，不能确认报价' : message || '确认报价失败', icon: 'none' })
  } finally {
    quoteBusy.value = ''
  }
}

function nextPaymentKind(project: Project): 'deposit' | 'balance' | 'full' | '' {
  if (!project.quotes?.some((quote) => quote.status === 'accepted')) return ''
  if (project.payment_status === 'unpaid') return project.deposit_cents > 0 ? 'deposit' : 'full'
  if (project.payment_status === 'deposit_paid') return 'balance'
  return ''
}

async function createPayment(project: Project): Promise<void> {
  const kind = nextPaymentKind(project)
  if (!kind || paymentBusy.value) return
  paymentBusy.value = project.id
  try {
    const order = await request<PaymentOrder>(`/c/v1/me/projects/${project.id}/payment-orders`, 'POST', { kind }, true)
    paymentOrders.value = { ...paymentOrders.value, [project.id]: order }
    uni.showToast({ title: '支付单已创建', icon: 'success' })
  } catch (reason) {
    uni.showToast({ title: reason instanceof Error ? reason.message : '创建支付单失败', icon: 'none' })
  } finally {
    paymentBusy.value = ''
  }
}

async function completeSandboxPayment(project: Project): Promise<void> {
  const order = paymentOrders.value[project.id]
  if (!order || paymentBusy.value) return
  paymentBusy.value = project.id
  try {
    await request(`/c/v1/me/projects/${project.id}/payment-orders/${order.id}/sandbox-complete`, 'POST', {}, true)
    delete paymentOrders.value[project.id]
    await load()
    uni.showToast({ title: '沙箱付款已完成', icon: 'success' })
  } catch (reason) {
    uni.showToast({ title: reason instanceof Error ? reason.message : '模拟付款失败', icon: 'none' })
  } finally {
    paymentBusy.value = ''
  }
}

function copyPaymentLink(project: Project): void {
  const url = paymentOrders.value[project.id]?.pay_url
  if (url) uni.setClipboardData({ data: url })
}

function openShare(): void {
  shareError.value = ''
  const value = shareURL.value.trim()
  if (!isCustomerShareURL(value)) {
    shareError.value = `请粘贴 ${allowedShareBase() || '摄影师'} 发来的 /#/a、/#/p 或 /#/d 分享链接。`
    return
  }
  uni.navigateTo({ url: `/pages/share/share?url=${encodeURIComponent(value)}` })
}

onShow(() => {
  void load()
})
</script>

<template>
  <view class="client-page">
    <view class="client-brand"><text class="brand-name">CLIENT / PROJECTS</text><text>我的拍摄</text></view>
    <view class="client-title">拍摄计划，<br />一目了然。</view>
    <view v-if="loading" class="empty-state">正在加载拍摄项目…</view>
    <view v-else-if="!session" class="empty-state">请从摄影师邀请链接登录后查看项目。</view>
    <view v-else-if="error" class="notice">{{ error }}</view>
    <view v-else-if="!projects.length" class="empty-state">尚无关联的拍摄项目。</view>
    <view v-for="project in projects" :key="project.id" class="project-row">
      <text class="project-title">{{ project.title }}</text>
      <text>{{ project.studio_name }} · {{ projectStateLabel(project.state) }}</text>
      <text v-if="project.shoot_start_at">拍摄时间 · {{ new Date(project.shoot_start_at).toLocaleString('zh-CN', { hour12: false }) }}</text>
      <view class="payment-summary"><text>收款状态 · {{ project.payment_status }}</text><text>{{ project.currency }} {{ (project.total_amount_cents / 100).toFixed(2) }}</text></view>
      <view class="project-quotes">
        <view class="quote-heading"><text>报价</text><text v-if="project.quotes?.length">{{ project.quotes.length }} 份</text></view>
        <view v-if="!project.quotes?.length" class="quote-empty">摄影师发送报价后会显示在这里。</view>
        <view v-for="quote in project.quotes" :key="quote.id" class="quote-card">
          <view class="quote-topline"><text class="quote-version">报价 v{{ quote.version }}</text><text :class="['quote-status', project.state === 'cancelled' || project.state === 'archived' ? 'cancelled' : quote.status]">{{ project.state === 'cancelled' || project.state === 'archived' ? '项目已取消' : quote.status === 'accepted' ? '已确认' : '待确认' }}</text></view>
          <view v-for="(item, index) in quote.items" :key="`${quote.id}-${index}`" class="quote-item">
            <text>{{ item.description }} × {{ item.quantity }}</text>
            <text>{{ quote.currency }} {{ (item.amount_cents / 100).toFixed(2) }}</text>
          </view>
          <view class="quote-total"><text>报价总额</text><strong class="quote-total-amount">{{ quote.currency }} {{ (quote.total_cents / 100).toFixed(2) }}</strong></view>
          <view v-if="quote.deposit_cents > 0" class="quote-deposit">预约定金 {{ quote.currency }} {{ (quote.deposit_cents / 100).toFixed(2) }}</view>
          <view v-if="quote.valid_until" class="quote-deposit">有效期至 {{ new Date(quote.valid_until).toLocaleDateString('zh-CN') }}</view>
          <view v-if="quote.terms" class="quote-terms">{{ quote.terms }}</view>
          <button v-if="quote.status === 'sent' && project.state !== 'cancelled' && project.state !== 'archived'" class="quote-accept" :disabled="Boolean(quoteBusy)" @click="acceptQuote(quote)">{{ quoteBusy === quote.id ? '正在确认…' : '确认报价' }}</button>
        </view>
      </view>
      <view v-if="nextPaymentKind(project)" class="payment-actions">
        <button class="quote-accept" :disabled="Boolean(paymentBusy)" @click="createPayment(project)">{{ paymentBusy === project.id ? '正在创建…' : nextPaymentKind(project) === 'balance' ? '创建尾款支付单' : '创建定金支付单' }}</button>
        <text v-if="paymentOrders[project.id]" class="payment-hint">开发沙箱订单：{{ paymentOrders[project.id].kind }} · {{ (paymentOrders[project.id].amount_cents / 100).toFixed(2) }}</text>
      </view>
      <view v-if="paymentOrders[project.id]?.status === 'pending'" class="payment-actions">
        <text class="payment-hint">{{ paymentOrders[project.id]?.kind === 'balance' ? '尾款待支付，支付后即可打开选片链接。' : '支付订单待完成。' }} · {{ project.currency }} {{ ((paymentOrders[project.id]?.amount_cents ?? 0) / 100).toFixed(2) }}</text>
        <button class="quote-accept" @click="copyPaymentLink(project)">复制支付链接</button>
        <button class="sandbox-pay" :disabled="Boolean(paymentBusy)" @click="completeSandboxPayment(project)">模拟支付成功</button>
      </view>
    </view>
    <view v-if="session" class="client-section">
      <view class="client-section-head"><text class="section-name">选片 / 审片 / 交付</text><text>摄影师分享链接</text></view>
      <input v-model="shareURL" class="invite-input" placeholder="粘贴摄影师发来的分享链接" />
      <view v-if="shareError" class="notice">{{ shareError }}</view>
      <button class="primary-button" @click="openShare">打开分享内容</button>
    </view>
  </view>
</template>
