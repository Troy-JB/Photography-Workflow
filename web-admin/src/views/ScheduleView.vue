<script setup lang="ts">
import { CalendarDays } from '@lucide/vue'
import { computed, onMounted, ref } from 'vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'
import { ApiError } from '../api/client'
import {
  cancelBooking,
  confirmBooking,
  createPhotographerResource,
  getAvailability,
  listBookings,
  listResources,
  listServices,
  saveService,
  setAvailability,
  type AvailabilityRule,
  type Booking,
  type BookingService,
  type Resource,
} from '../api/schedule'
import { api, auth } from '../state/session'
import type { Profile } from '../api/types'
import { toast } from '../state/toast'
import { formatDateTime } from '../utils/format'

type Pane = 'appointments' | 'services' | 'availability'
const pane = ref<Pane>('appointments')
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const blockedProjectID = ref('')
const bookingToCancel = ref<Booking | null>(null)
const resources = ref<Resource[]>([])
const services = ref<BookingService[]>([])
const bookings = ref<Booking[]>([])
const bookingStatus = ref('held')
const activeService = ref('')
const resourceName = ref('我的摄影档期')
const serviceForm = ref({ name: '', description: '', duration: 120, price: 0, includedRetouchCount: 0, extraRetouchUnitPrice: 0 })
const ruleRows = ref<{ weekday: number; enabled: boolean; allDay: boolean; start: string; end: string }[]>([])
const availabilityVersion = ref(1)
const availabilityResourceID = ref('')

const weekdays = ['周一', '周二', '周三', '周四', '周五', '周六', '周日']
const photographerResources = computed(() => resources.value.filter((resource) => resource.kind === 'photographer' && resource.active))
const selectedService = computed(() => services.value.find((service) => service.id === activeService.value) ?? null)
const canWrite = computed(() => auth.role === 'owner' || auth.role === 'staff')

function minuteOf(value: string): number {
  const [hour, minute] = value.split(':').map(Number)
  return hour * 60 + minute
}

function clockOf(value: number): string {
  const safe = Math.max(0, Math.min(1440, value))
  return `${String(Math.floor(safe / 60)).padStart(2, '0')}:${String(safe % 60).padStart(2, '0')}`
}

async function loadAvailability(resourceID = availabilityResourceID.value): Promise<void> {
  if (!resourceID) return
  availabilityResourceID.value = resourceID
  const data = await getAvailability(resourceID)
  availabilityVersion.value = data.version
  const allDayDefault = data.rules.length === 0
  ruleRows.value = weekdays.map((_, index) => {
    const existing = data.rules.find((rule) => rule.weekday === index + 1)
    const allDay = allDayDefault || (existing?.start_minute === 0 && existing.end_minute === 1440)
    return {
      weekday: index + 1,
      enabled: allDayDefault || Boolean(existing),
      allDay,
      start: clockOf(existing?.start_minute === 0 && existing.end_minute === 1440 ? 9 * 60 : existing?.start_minute ?? 9 * 60),
      end: clockOf(existing?.end_minute === 1440 ? 18 * 60 : existing?.end_minute ?? 18 * 60),
    }
  })
}

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  if (!auth.user?.photographer_code) {
    try {
      const profile = await api<Profile>('/api/v1/auth/me')
      auth.user = profile.user
      auth.tenant = profile.tenant
      auth.role = profile.role
    } catch {
      // Keep schedule loading independent; the profile can be retried on next visit.
    }
  }
  try {
    const [resourcePage, servicePage, bookingPage] = await Promise.all([listResources(), listServices(), listBookings(bookingStatus.value)])
    resources.value = resourcePage.items ?? []
    services.value = servicePage.items ?? []
    bookings.value = bookingPage.items ?? []
    if (!photographerResources.value.length && canWrite.value) {
      const created = await createPhotographerResource(resourceName.value)
      resources.value = [created]
    }
    if (!activeService.value || !services.value.some((service) => service.id === activeService.value)) {
      activeService.value = services.value[0]?.id ?? ''
    }
    const resourceID = selectedService.value?.resource_id || photographerResources.value[0]?.id || ''
    if (resourceID) await loadAvailability(resourceID)
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '预约数据加载失败'
  } finally {
    loading.value = false
  }
}

function editService(service?: BookingService): void {
  activeService.value = service?.id ?? ''
  serviceForm.value = {
    name: service?.name ?? '',
    description: service?.description ?? '',
    duration: service?.duration_minutes ?? 120,
    price: service ? service.price_cents / 100 : 0,
    includedRetouchCount: service?.included_retouch_count ?? 0,
    extraRetouchUnitPrice: service ? service.extra_retouch_unit_price_cents / 100 : 0,
  }
  if (service?.resource_id) void loadAvailability(service.resource_id).catch(() => undefined)
}

async function savePackage(): Promise<void> {
  const resourceID = selectedService.value?.resource_id || photographerResources.value[0]?.id
  if (!resourceID) {
    error.value = '请先创建摄影师档期资源'
    return
  }
  if (!serviceForm.value.name.trim()) {
    error.value = '请填写服务名称'
    return
  }
  saving.value = true
  error.value = ''
  try {
    const saved = await saveService({
      ...(selectedService.value ? { id: selectedService.value.id, version: selectedService.value.version } : {}),
      resource_id: resourceID,
      name: serviceForm.value.name.trim(),
      description: serviceForm.value.description.trim(),
      duration_minutes: Number(serviceForm.value.duration),
      price_cents: Math.round(Number(serviceForm.value.price) * 100),
      included_retouch_count: Number(serviceForm.value.includedRetouchCount),
      extra_retouch_unit_price_cents: Math.round(Number(serviceForm.value.extraRetouchUnitPrice) * 100),
      active: true,
    })
    services.value = selectedService.value
      ? services.value.map((service) => service.id === saved.id ? saved : service)
      : [...services.value, saved]
    activeService.value = saved.id
    await loadAvailability(saved.resource_id)
    toast('服务套餐已保存', 'success')
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '服务套餐保存失败'
  } finally {
    saving.value = false
  }
}

async function saveRules(): Promise<void> {
  if (!availabilityResourceID.value) return
  saving.value = true
  error.value = ''
  const rules: AvailabilityRule[] = ruleRows.value
    .filter((row) => row.enabled)
    .map((row) => ({
      weekday: row.weekday,
      start_minute: row.allDay ? 0 : minuteOf(row.start),
      end_minute: row.allDay ? 1440 : minuteOf(row.end),
    }))
  try {
    const saved = await setAvailability(availabilityResourceID.value, availabilityVersion.value, rules)
    availabilityVersion.value = saved.version
    toast('每周可用时间已更新', 'success')
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '可用时间保存失败'
  } finally {
    saving.value = false
  }
}

async function refreshBookings(): Promise<void> {
  try {
    bookings.value = (await listBookings(bookingStatus.value)).items ?? []
  } catch (err) {
    error.value = err instanceof ApiError ? err.detail || err.message : '预约列表加载失败'
  }
}

async function updateBooking(booking: Booking, action: 'confirm' | 'cancel'): Promise<void> {
  saving.value = true
  error.value = ''
  blockedProjectID.value = ''
  try {
    if (action === 'confirm') await confirmBooking(booking)
    else await cancelBooking(booking)
    await refreshBookings()
    toast(action === 'confirm' ? '预约已确认' : '预约已取消', 'success')
  } catch (err) {
    if (action === 'confirm' && err instanceof ApiError && err.code === 'deposit_required') {
      blockedProjectID.value = booking.project_id
      error.value = '定金或全款尚未到账。请先创建收款订单；收款成功后预约会自动确认。'
    } else {
      error.value = err instanceof ApiError ? err.detail || err.message : '预约更新失败'
    }
  } finally {
    saving.value = false
  }
}

function requestCancelBooking(booking: Booking): void {
  bookingToCancel.value = booking
}

async function cancelBookingAndProject(): Promise<void> {
  if (!bookingToCancel.value) return
  const booking = bookingToCancel.value
  await updateBooking(booking, 'cancel')
  bookingToCancel.value = null
}

async function copyPhotographerCode(): Promise<void> {
  const code = auth.user?.photographer_code
  if (!code) return
  try {
    await navigator.clipboard?.writeText(code)
    toast(`摄影师编号 ${code} 已复制`, 'success')
  } catch {
    toast(`摄影师编号：${code}`, 'success')
  }
}

onMounted(load)
</script>

<template>
  <section class="page schedule-page">
    <header class="page-head">
      <div>
        <div class="eyebrow">工作室 / 排期</div>
        <h1>预约与档期</h1>
      </div>
      <button class="btn primary" type="button" :disabled="!auth.user?.photographer_code" @click="copyPhotographerCode">复制摄影师编号</button>
    </header>

    <div class="invite-result"><div class="invite-copy"><span>个人摄影师编号</span><strong>{{ auth.user?.photographer_code || '请重新登录以加载编号' }}</strong><small>客户输入此编号即可查看服务与档期。</small></div></div>
    <p v-if="error" class="alert error" role="alert">{{ error }} <RouterLink v-if="blockedProjectID" :to="{ name: 'commerce', query: { project_id: blockedProjectID } }">打开项目报价与收款</RouterLink></p>
    <p v-if="loading" class="empty muted">正在加载工作室排期…</p>

    <template v-else>
      <nav class="schedule-tabs" aria-label="预约管理">
        <button type="button" :class="{ active: pane === 'appointments' }" @click="pane = 'appointments'">预约 <span>{{ bookings.length }}</span></button>
        <button type="button" :class="{ active: pane === 'services' }" @click="pane = 'services'">服务套餐 <span>{{ services.length }}</span></button>
        <button type="button" :class="{ active: pane === 'availability' }" @click="pane = 'availability'">每周可用时间</button>
      </nav>

      <section v-if="pane === 'appointments'" class="schedule-section">
        <div class="section-heading"><div><h2>预约列表</h2><p class="muted">客户提交后先临时锁位；收到定金或全款后预约自动确认。</p></div><div class="booking-tools"><select v-model="bookingStatus" aria-label="按预约状态筛选" @change="refreshBookings"><option value="held">待处理</option><option value="confirmed">已确认</option><option value="cancelled">已取消</option><option value="expired">已过期</option><option value="">全部状态</option></select><button class="btn ghost sm" type="button" @click="refreshBookings">刷新</button></div></div>
        <div v-if="!bookings.length" class="booking-empty">
          <span><CalendarDays :size="20" /></span>
          <strong>还没有客户预约</strong>
          <p>客户输入你的摄影师编号并提交预约后，申请会显示在这里。</p>
        </div>
        <div v-else class="booking-list">
          <article v-for="booking in bookings" :key="booking.id" class="booking-row">
            <div class="booking-date"><strong>{{ formatDateTime(booking.starts_at) }}</strong><span>{{ formatDateTime(booking.ends_at) }}</span></div>
            <div class="booking-project"><strong>项目 {{ booking.project_id.slice(0, 8) }}</strong><small>预约号 {{ booking.id.slice(0, 8) }}</small></div>
            <div class="booking-state"><span :class="['status', booking.status]">{{ booking.status === 'held' ? '临时锁位' : booking.status === 'confirmed' ? '已确认' : booking.status === 'expired' ? '已过期' : '已取消' }}</span><small v-if="booking.status === 'held' && booking.hold_expires_at">锁位截止 {{ formatDateTime(booking.hold_expires_at) }}</small></div>
            <div class="booking-actions"><RouterLink v-if="booking.status === 'held'" class="btn ghost sm" :to="{ name: 'commerce', query: { project_id: booking.project_id } }">报价与收款</RouterLink><button v-if="booking.status === 'held'" class="btn primary sm" type="button" :disabled="saving" @click="updateBooking(booking, 'confirm')">确认</button><button v-if="booking.status === 'held' || booking.status === 'confirmed'" class="btn ghost sm" type="button" :disabled="saving" @click="requestCancelBooking(booking)">取消</button></div>
          </article>
        </div>
      </section>

      <section v-else-if="pane === 'services'" class="schedule-section service-layout">
        <div class="service-list">
          <div class="section-heading"><div><h2>服务套餐</h2><p class="muted">客户选套餐后，根据对应摄影师档期展示时间。</p></div><button class="btn ghost sm" type="button" @click="editService()">新建</button></div>
          <button v-for="service in services" :key="service.id" type="button" class="service-option" :class="{ selected: activeService === service.id }" @click="editService(service)">
            <span><strong>{{ service.name }}</strong><small>{{ service.duration_minutes }} 分钟 · ¥{{ (service.price_cents / 100).toFixed(0) }} · 含精修 {{ service.included_retouch_count }} 张</small></span><span class="service-arrow">›</span>
          </button>
          <p v-if="!services.length" class="empty">还没有套餐，请先添加第一个服务。</p>
        </div>
        <form class="service-editor" @submit.prevent="savePackage">
          <div class="section-heading"><div><h2>{{ selectedService ? '编辑套餐' : '新建套餐' }}</h2><p class="muted">客户预约时看到的服务和价格。</p></div></div>
          <label class="field"><span>服务名称</span><input v-model="serviceForm.name" maxlength="120" required placeholder="例如：个人肖像拍摄" /></label>
          <label class="field"><span>服务说明</span><textarea v-model="serviceForm.description" maxlength="2000" rows="3" placeholder="拍摄内容与交付说明" /></label>
          <div class="form-grid"><label class="field"><span>拍摄时长（分钟）</span><input v-model.number="serviceForm.duration" type="number" min="15" max="1440" step="15" required /></label><label class="field"><span>展示价格（元）</span><input v-model.number="serviceForm.price" type="number" min="0" step="1" required /></label></div>
          <div class="form-grid"><label class="field"><span>套餐包含精修张数</span><input v-model.number="serviceForm.includedRetouchCount" type="number" min="0" max="10000" step="1" required /></label><label class="field"><span>超出每张单价（元）</span><input v-model.number="serviceForm.extraRetouchUnitPrice" type="number" min="0" step="0.01" required /></label></div>
          <button class="btn primary" type="submit" :disabled="saving || !canWrite">保存套餐</button>
        </form>
      </section>

      <section v-else class="schedule-section">
        <div class="section-heading"><div><h2>每周可预约时间</h2><p class="muted">未设置规则的摄影师资源默认为全天可约；按服务器排期时区解释。</p></div><select v-model="availabilityResourceID" aria-label="选择摄影师资源" @change="loadAvailability(availabilityResourceID)"><option v-for="resource in photographerResources" :key="resource.id" :value="resource.id">{{ resource.name }}</option></select></div>
        <div class="availability-table">
          <div v-for="row in ruleRows" :key="row.weekday" class="availability-row">
            <label class="availability-day"><input v-model="row.enabled" type="checkbox" /><span>{{ weekdays[row.weekday - 1] }}</span></label>
            <template v-if="row.enabled"><select v-model="row.allDay" :aria-label="`${weekdays[row.weekday - 1]}开放时段类型`"><option :value="true">全天</option><option :value="false">自定义</option></select><template v-if="row.allDay"><span class="muted all-day-label">00:00 至 24:00</span></template><template v-else><input v-model="row.start" type="time" :aria-label="`${weekdays[row.weekday - 1]}开始时间`" /><span class="muted">至</span><input v-model="row.end" type="time" :aria-label="`${weekdays[row.weekday - 1]}结束时间`" /></template></template>
            <span v-else class="muted closed-day">不开放</span>
          </div>
        </div>
        <button class="btn primary" type="button" :disabled="saving || !canWrite" @click="saveRules">保存可用时间</button>
      </section>
    </template>
  </section>
  <ConfirmDialog
    :open="Boolean(bookingToCancel)"
    title="取消预约并关闭项目？"
    message="客户会看到项目已取消，当前报价将不能再确认。已收款项不会自动退款，请另行办理退款。"
    confirm-text="取消预约并关闭项目"
    tone="danger"
    :busy="saving"
    @confirm="cancelBookingAndProject"
    @cancel="bookingToCancel = null"
  />
</template>

<style scoped>
.schedule-page { max-width: 1180px; }
.schedule-page > .page-head { align-items: center; }
.schedule-page .eyebrow { color: var(--accent); font-size: 11px; font-weight: 650; margin-bottom: 5px; }
.schedule-tabs { display: inline-flex; gap: 4px; padding: 4px; margin: 16px 0 20px; border: 1px solid var(--border); border-radius: 12px; background: var(--panel-strong); }
.schedule-tabs button { display: inline-flex; align-items: center; gap: 7px; min-height: 42px; padding: 0 14px; border: 0; border-radius: 5px; background: transparent; color: var(--muted); font-size: 12px; }
.schedule-tabs button.active { background: var(--panel); color: var(--text); box-shadow: 0 1px 3px rgba(74, 43, 29, 0.1); font-weight: 650; }
.schedule-tabs span { color: var(--accent); font-size: 11px; font-variant-numeric: tabular-nums; }
.schedule-section { min-height: 300px; }
.section-heading { display: flex; align-items: center; justify-content: space-between; gap: 18px; margin-bottom: 18px; }
.section-heading h2 { font-size: 17px; margin-bottom: 3px; }
.section-heading p { font-size: 12px; }
.booking-list { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }
.booking-empty { display: grid; justify-items: center; gap: 7px; min-height: 176px; align-content: center; padding: 24px; border: 1px dashed var(--border-strong); border-radius: 14px; background: var(--panel); text-align: center; }
.booking-empty > span { display: grid; place-items: center; width: 42px; height: 42px; margin-bottom: 3px; border-radius: 50%; background: #f4e9df; color: var(--accent); }
.booking-empty strong { font-size: 14px; }
.booking-empty p { max-width: 340px; color: var(--muted); font-size: 12px; }
.booking-row { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-content: start; align-items: center; gap: 12px; min-width: 0; min-height: 0; padding: 15px; border: 1px solid var(--border); border-radius: 14px; background: var(--panel); transition: border-color 150ms ease, box-shadow 150ms ease; }
.booking-row:hover { border-color: var(--border-strong); box-shadow: 0 4px 14px rgba(74, 43, 29, 0.07); }
.booking-row small, .booking-date span { display: block; color: var(--muted); font-size: 11px; margin-top: 3px; }
.booking-date { min-width: 0; }
.booking-date strong { display: block; font-size: 14px; font-variant-numeric: tabular-nums; }
.booking-project { grid-column: 1 / -1; padding: 10px 0 2px; border-top: 1px solid var(--surface-subtle); }
.booking-project strong { display: block; overflow: hidden; font-size: 13px; text-overflow: ellipsis; white-space: nowrap; }
.booking-state { display: grid; justify-items: end; gap: 4px; }
.booking-state small { text-align: right; }
.booking-actions { grid-column: 1 / -1; display: flex; justify-content: end; gap: 7px; padding-top: 2px; }
.booking-actions .btn { min-height: 44px; }
.booking-tools { display: flex; align-items: center; gap: 8px; }
.booking-tools select { width: 130px; min-height: 40px; }
.status { padding: 4px 9px; border-radius: 999px; background: #f6ecd9; color: #75521f; font-size: 11px; white-space: nowrap; }
.status.confirmed { background: #e5f0e8; color: #28634b; }
.status.cancelled, .status.expired { background: #f4e9df; color: #705c50; }
.invite-result { display: flex; align-items: center; flex-wrap: wrap; gap: 18px; padding: 14px 16px; margin-top: 14px; border: 1px solid var(--border); border-left: 3px solid var(--accent); border-radius: 14px; background: var(--panel); overflow-wrap: anywhere; }
.invite-result span { color: var(--muted); }
.invite-copy { display: grid; gap: 3px; min-width: 0; }
.invite-copy small { color: var(--muted); overflow-wrap: anywhere; }
.invite-code { width: 144px; height: 144px; object-fit: contain; background: #fff; }
.service-layout { display: grid; grid-template-columns: minmax(260px, .8fr) minmax(420px, 1.2fr); gap: 16px; }
.service-list, .service-editor { min-width: 0; padding: 16px; border: 1px solid var(--border); border-radius: 14px; background: var(--panel); }
.service-option { display: flex; width: 100%; min-height: 58px; justify-content: space-between; align-items: center; gap: 12px; margin-top: 8px; padding: 10px 12px; border: 1px solid var(--border); border-radius: 11px; background: var(--panel); text-align: left; }
.service-option.selected { border-color: var(--accent); background: #f8eee7; }
.service-option strong, .service-option small { display: block; }
.service-option small { color: var(--muted); margin-top: 3px; }
.service-arrow { color: var(--accent); font-size: 22px; }
.service-editor { display: grid; align-content: start; gap: 14px; }
.service-editor .section-heading { margin-bottom: 0; }
.form-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
.availability-table { display: grid; gap: 8px; width: min(100%, 760px); margin-bottom: 18px; }
.availability-row { display: grid; grid-template-columns: minmax(130px, 1fr) 110px 130px 30px 130px; gap: 10px; align-items: center; min-height: 58px; padding: 8px 12px; border: 1px solid var(--border); border-radius: 11px; background: var(--panel); }
.availability-day { display: flex; align-items: center; gap: 10px; }
.availability-day input { width: 16px; min-height: 16px; }
.closed-day { grid-column: 2 / -1; }
.all-day-label { grid-column: 3 / -1; }
.availability-row input[type='time'] { min-height: 40px; }
.section-heading select { width: 220px; }
@media (max-width: 760px) {
  .service-layout { grid-template-columns: minmax(0, 1fr); gap: 12px; }
  .booking-list { grid-template-columns: minmax(0, 1fr); }
  .section-heading { align-items: start; }
  .section-heading select { width: 155px; }
  .availability-row { grid-template-columns: minmax(100px, 1fr) 100px minmax(90px, 1fr); }
  .closed-day, .all-day-label { grid-column: 2 / -1; }
  .availability-row input[type='time'] { width: 100%; }
}
@media (max-width: 480px) {
  .schedule-page > .page-head { align-items: start; }
  .schedule-page > .page-head .btn { align-self: start; }
  .schedule-tabs { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); width: 100%; }
  .schedule-tabs button { justify-content: center; padding-inline: 4px; font-size: 11px; }
  .section-heading { flex-direction: column; }
  .booking-tools { width: 100%; }
  .booking-tools select { flex: 1; width: auto; }
  .availability-row { grid-template-columns: 1fr 1fr; }
  .availability-day { grid-column: 1 / -1; }
  .closed-day, .all-day-label { grid-column: 1 / -1; }
  .availability-row input[type='time'] { min-width: 0; }
}
</style>
