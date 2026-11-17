<script setup lang="ts">
import { onLoad, onShow } from '@dcloudio/uni-app'
import { computed, ref } from 'vue'
import { getSession, login, loginWithPhone, request, requestPhoneCode, type BookingService, type PhotographerBookingTarget, type Slot } from '../../api'

interface Invite { studio_id: string; studio_name: string; services: BookingService[] }
const inviteToken = ref('')
const photographerCode = ref('')
const invite = ref<(Invite & Partial<PhotographerBookingTarget>) | null>(null)
const selectedService = ref('')
const firstWeekOffset = -new Date().getDay()
const weekStartOffset = ref(firstWeekOffset)
const selectedDayOffset = ref(0)
const slots = ref<Slot[]>([])
const selectedSlot = ref('')
const note = ref('')
const loading = ref(true)
const submitting = ref(false)
const error = ref('')
const account = ref(getSession())
const name = ref(account.value?.customer.name ?? '')
const devPhone = ref(account.value?.customer.phone ?? '')
const phoneCode = ref('')
const bookingDone = ref(false)
const bookingExpiry = ref('')
const requestID = ref('')
const verificationCode = ref('')
const devCode = ref('')

const service = computed(() => invite.value?.services.find((item) => item.id === selectedService.value) ?? null)
const chosenDay = computed(() => {
  const day = new Date()
  day.setDate(day.getDate() + selectedDayOffset.value)
  return day
})
const dayKey = computed(() => {
  const day = chosenDay.value
  return `${day.getFullYear()}-${String(day.getMonth() + 1).padStart(2, '0')}-${String(day.getDate()).padStart(2, '0')}`
})
const dayLabel = computed(() => chosenDay.value.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric', weekday: 'long' }))
const calendarLabel = computed(() => {
  const start = new Date()
  start.setDate(start.getDate() + weekStartOffset.value)
  const end = new Date(start)
  end.setDate(end.getDate() + 6)
  const monthLabel = (date: Date) => date.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long' })
  return monthLabel(start) === monthLabel(end) ? monthLabel(start) : `${monthLabel(start)} - ${monthLabel(end)}`
})
const shortDays = computed(() => Array.from({ length: 7 }, (_, index) => {
  const offset = weekStartOffset.value + index
  const day = new Date()
  day.setDate(day.getDate() + offset)
  return {
    key: `${day.getFullYear()}-${String(day.getMonth() + 1).padStart(2, '0')}-${String(day.getDate()).padStart(2, '0')}`,
    weekday: day.toLocaleDateString('zh-CN', { weekday: 'short' }),
    date: day.getDate(),
    offset,
    isToday: offset === 0,
    isPast: offset < 0,
  }
}))

function hourLabel(value: string): string {
  return new Date(value).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
}

async function loadInvite(): Promise<void> {
  if (!inviteToken.value && !photographerCode.value) {
    error.value = '请输入摄影师编号'
    loading.value = false
    return
  }
  loading.value = true
  error.value = ''
  try {
    invite.value = photographerCode.value
      ? await request<PhotographerBookingTarget>('/c/v1/photographers/resolve', 'POST', { code: photographerCode.value })
      : await request<Invite>('/c/v1/invites/resolve', 'POST', { token: inviteToken.value })
    if (!selectedService.value) selectedService.value = invite.value.services[0]?.id ?? ''
    await loadSlots()
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '预约链接无效或已过期'
  } finally {
    loading.value = false
  }
}

async function loadSlots(): Promise<void> {
  if (!selectedService.value || (!inviteToken.value && !photographerCode.value)) return
  selectedSlot.value = ''
  error.value = ''
  try {
    const result = await request<{ slots: Slot[] }>('/c/v1/invites/slots', 'POST', {
      ...(inviteToken.value ? { invite_token: inviteToken.value } : { photographer_code: photographerCode.value }),
      service_id: selectedService.value,
      day: dayKey.value,
    })
    slots.value = result.slots ?? []
  } catch (reason) {
    slots.value = []
    error.value = reason instanceof Error ? reason.message : '档期暂时无法加载'
  }
}

function selectDay(offset: number): void {
  if (offset < 0) return
  selectedDayOffset.value = offset
  void loadSlots()
}

function changeWeek(direction: -1 | 1): void {
  weekStartOffset.value = Math.max(firstWeekOffset, weekStartOffset.value + direction * 7)
  selectedDayOffset.value = weekStartOffset.value
  if (selectedDayOffset.value < 0) selectedDayOffset.value = 0
  void loadSlots()
}

async function finishLogin(event?: { detail?: { code?: string } }): Promise<void> {
  error.value = ''
  phoneCode.value = event?.detail?.code ?? ''
  if (!name.value.trim()) {
    error.value = '请填写称呼'
    return
  }
  try {
    if (phoneCode.value) await login(name.value.trim(), phoneCode.value, devPhone.value.trim(), inviteToken.value)
    else await loginWithPhone(name.value.trim(), devPhone.value.trim(), verificationCode.value.trim())
    account.value = getSession()
    error.value = ''
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '微信授权失败，请重试'
  }
}

async function sendCode(): Promise<void> {
  try { devCode.value = await requestPhoneCode(devPhone.value.trim()); error.value = '' }
  catch (reason) { error.value = reason instanceof Error ? reason.message : '验证码暂时无法发送' }
}

async function submitBooking(): Promise<void> {
  if (!account.value) {
    error.value = '请先完成微信登录和手机号授权'
    return
  }
  if (!service.value || !selectedSlot.value) return
  submitting.value = true
  error.value = ''
  if (!requestID.value) requestID.value = newRequestID()
  try {
    const result = await request<{ id: string; hold_expires_at: string }>('/c/v1/bookings', 'POST', {
      ...(inviteToken.value ? { invite_token: inviteToken.value } : { photographer_code: photographerCode.value }),
      service_id: service.value.id,
      request_id: requestID.value,
      starts_at: selectedSlot.value,
      note: note.value.trim(),
    }, true)
    bookingExpiry.value = new Date(result.hold_expires_at).toLocaleString('zh-CN', { hour12: false })
    bookingDone.value = true
    requestID.value = ''
  } catch (reason) {
    error.value = reason instanceof Error ? reason.message : '该时段刚被预约，请重新选择'
    await loadSlots()
  } finally {
    submitting.value = false
  }
}

function newRequestID(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 18)}`
}

function goProjects(): void {
  uni.switchTab({ url: '/pages/projects/projects' })
}

onShow(() => {
  if (invite.value && !loading.value && !submitting.value) void loadSlots()
})

onLoad((query) => {
  let token = String(query?.token || query?.scene || '')
  if (token.includes('token=')) token = token.split('token=').pop()?.split('&')[0] ?? ''
  inviteToken.value = decodeURIComponent(token)
  photographerCode.value = String(query?.code || '').toUpperCase()
  void loadInvite()
})
</script>

<template>
  <view class="client-page">
    <view v-if="loading" class="empty-state">正在读取摄影师档期…</view>
    <view v-else-if="bookingDone" class="booking-summary">
      <view class="client-brand"><text class="brand-name">{{ invite?.studio_name }}</text><text>预约已锁位</text></view>
      <view class="client-title">时间已为你<br />临时保留。</view>
      <text>请在 {{ bookingExpiry }} 前联系摄影师确认报价与款项。</text>
      <view class="client-section"><button class="primary-button" @click="goProjects">查看我的拍摄</button></view>
    </view>
    <template v-else-if="invite">
      <view class="client-brand"><text class="brand-name">{{ invite.studio_name }}</text><text>拍摄预约</text></view>
      <view class="client-title">选择你的<br />拍摄服务。</view>
      <view class="client-subtitle">可预约时间以摄影师当前档期为准。</view>

      <view class="client-section">
        <view class="client-section-head"><text class="section-name">拍摄服务</text><text>{{ invite.services.length }} 项</text></view>
        <view v-if="!invite.services.length" class="empty-state">摄影师暂未开放服务套餐。</view>
        <view class="service-list">
          <button v-for="item in invite.services" :key="item.id" class="service-item" :class="{ selected: selectedService === item.id }" @click="selectedService = item.id; void loadSlots()">
            <view class="service-line"><text class="service-name">{{ item.name }}</text><text class="service-price">¥{{ (item.price_cents / 100).toFixed(0) }}</text></view>
            <text class="service-meta">{{ item.duration_minutes }} 分钟</text>
            <text class="service-meta">套餐含精修 {{ item.included_retouch_count }} 张<template v-if="item.extra_retouch_unit_price_cents > 0"> · 超出 ¥{{ (item.extra_retouch_unit_price_cents / 100).toFixed(2) }}/张</template></text>
            <text v-if="item.description" class="service-description">{{ item.description }}</text>
          </button>
        </view>
      </view>

      <view class="client-section">
        <view class="client-section-head"><text class="section-name">可预约时间</text><text>{{ dayLabel }}</text></view>
        <view class="booking-calendar">
          <view class="calendar-card-head">
            <view><text class="calendar-month">{{ calendarLabel }}</text><text class="calendar-caption">选择日期后查看可用时段</text></view>
            <view class="calendar-nav"><button class="calendar-nav-button" :disabled="weekStartOffset === firstWeekOffset" aria-label="前一周" @click="changeWeek(-1)">‹</button><button class="calendar-nav-button" aria-label="后一周" @click="changeWeek(1)">›</button></view>
          </view>
          <view class="date-strip" role="group" aria-label="选择预约日期">
            <button v-for="day in shortDays" :key="day.key" class="date-button" :class="{ current: selectedDayOffset === day.offset, today: day.isToday, past: day.isPast }" :disabled="day.isPast" :aria-pressed="selectedDayOffset === day.offset" @click="selectDay(day.offset)">
              <text>{{ day.weekday }}</text><text class="date-number">{{ day.date }}</text><text v-if="day.isToday" class="today-label">今天</text>
            </button>
          </view>
          <view class="slot-heading"><text>可用时段</text><text v-if="slots.length">{{ slots.length }} 个</text></view>
          <view v-if="slots.length" class="slot-grid">
            <button v-for="slot in slots" :key="slot.starts_at" class="slot-button" :class="{ selected: selectedSlot === slot.starts_at }" :aria-pressed="selectedSlot === slot.starts_at" @click="selectedSlot = slot.starts_at">{{ hourLabel(slot.starts_at) }}</button>
          </view>
          <view v-else-if="!error" class="calendar-empty">这一天没有可预约时段，试试其他日期。</view>
        </view>
      </view>

      <view v-if="!account" class="client-section auth-panel">
        <view class="client-section-head"><text class="section-name">客户身份</text><text>手机号或微信</text></view>
        <input v-model="name" maxlength="60" placeholder="怎么称呼你" />
        <input v-model="devPhone" type="tel" placeholder="手机号" />
        <view class="auth-code-row"><input v-model="verificationCode" placeholder="短信验证码" /><button class="secondary-button" @click="sendCode">获取验证码</button></view>
        <text v-if="devCode">开发验证码：{{ devCode }}</text>
        <button class="secondary-button" @click="finishLogin()">手机号登录 / 注册</button>
        <!-- #ifdef MP-WEIXIN -->
        <button class="primary-button" open-type="getPhoneNumber" @getphonenumber="finishLogin">微信登录并授权手机号</button>
        <!-- #endif -->
        <!-- #ifdef H5 -->
        <input v-if="!devPhone" v-model="devPhone" type="tel" placeholder="本地预览手机号" />
        <button class="secondary-button" open-type="getPhoneNumber" @getphonenumber="finishLogin">微信授权登录</button>
        <!-- #endif -->
      </view>
      <view v-else class="client-section notice">已绑定 {{ account.customer.phone }}。提交预约后，摄影师会收到你的时段申请。</view>

      <view v-if="error" class="notice">{{ error }}</view>
      <view class="bottom-action"><button class="primary-button" :disabled="submitting || !selectedSlot || !service" @click="submitBooking">{{ submitting ? '正在锁定档期…' : '临时锁定此时段' }}</button></view>
    </template>
    <view v-else class="notice">{{ error || '预约链接不可用' }}</view>
  </view>
</template>
